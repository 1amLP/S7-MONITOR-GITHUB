//go:build linux && (amd64 || arm64)

package appliance

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"perimode/native/internal/journal"
	"perimode/native/internal/selinux"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const cacheMount = "/run/s7-cache"
func cacheConsent() string { return "S7-NATIVE-CACHE-1\n" + PinnedSerial + "\n" }

type CacheStore struct {
	Settings *SettingsStore
	device   string
	mounted  bool
}

// This is the one optional disk-writing feature. No arbitrary partition name,
// formatting, fsck, remount takeover, or writes outside CACHE. Automatic
// settings use ext4's own journal recovery after an interrupted mount.
func CacheDevice() (string, error)           { return cacheDevice(true) }
func cacheDevice(clean bool) (string, error) { return cacheDevicePolicy(clean, true) }

// cacheDeviceBeforePolicy is restricted to the serial-bound lab bootstrap.
// Before the first SELinux policy load there is no valid object context to set;
// every controller, rdev, PARTNAME and superblock check remains identical.
func cacheDeviceBeforePolicy(clean bool) (string, error) { return cacheDevicePolicy(clean, false) }

func cacheDevicePolicy(clean, applyLabel bool) (string, error) {
	paths, _ := filepath.Glob("/sys/class/block/*/uevent")
	result := ""
	for _, path := range paths {
		b, e := os.ReadFile(path)
		if e != nil {
			continue
		}
		fields := map[string]string{}
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if ok {
				fields[k] = v
			}
		}
		if fields["PARTNAME"] != "CACHE" {
			continue
		}
		real, e := filepath.EvalSymlinks(filepath.Dir(path))
		if e != nil || !strings.Contains(real, "/155a0000.ufs/") {
			return "", fmt.Errorf("CACHE not on pinned UFS controller")
		}
		name := fields["DEVNAME"]
		if name == "" || strings.ContainsAny(name, "/\\. \t\n") {
			return "", fmt.Errorf("invalid CACHE device node")
		}
		device := "/dev/" + name
		st, e := os.Stat(device)
		if e != nil || st.Mode()&os.ModeDevice == 0 || st.Mode()&os.ModeCharDevice != 0 {
			return "", fmt.Errorf("CACHE node missing or not block")
		}
		sys := st.Sys().(*syscall.Stat_t)
		major := (sys.Rdev>>8)&0xfff | (sys.Rdev>>32)&0xfffff000
		minor := sys.Rdev&0xff | (sys.Rdev>>12)&0xffffff00
		expectedMajor, em := strconv.ParseUint(fields["MAJOR"], 10, 64)
		expectedMinor, en := strconv.ParseUint(fields["MINOR"], 10, 64)
		if em != nil || en != nil || major != expectedMajor || minor != expectedMinor {
			return "", fmt.Errorf("CACHE device identity mismatch")
		}
		mounted, e := os.ReadFile("/proc/self/mountinfo")
		if e != nil {
			return "", e
		}
		dev := fmt.Sprintf("%d:%d", major, minor)
		for _, line := range strings.Split(string(mounted), "\n") {
			f := strings.Fields(line)
			if len(f) > 2 && f[2] == dev {
				return "", fmt.Errorf("CACHE already mounted; takeover refused")
			}
		}
		if result != "" {
			return "", fmt.Errorf("ambiguous CACHE")
		}
		result = device
	}
	if result == "" {
		return "", fmt.Errorf("CACHE partition unavailable")
	}
	if applyLabel {
		// The unique CACHE node/rdev/controller are already verified above. Label
		// only its RAM device inode after policy load; no disk xattrs.
		if e := selinux.Label(result, "u:object_r:cache_block_device:s0", selinux.KindBlock); e != nil {
			return "", e
		}
	}
	f, e := os.Open(result)
	if e != nil {
		return "", e
	}
	defer f.Close()
	sb := make([]byte, 1024)
	if _, e = f.ReadAt(sb, 1024); e != nil {
		return "", e
	}
	if clean {
		e = CheckCacheSuperblock(sb)
	} else {
		e = checkCacheReadable(sb)
	}
	if e != nil {
		return "", e
	}
	return result, nil
}
func CheckCacheSuperblock(sb []byte) error {
	if len(sb) != 1024 {
		return fmt.Errorf("truncated CACHE superblock")
	}
	le := binary.LittleEndian
	if le.Uint16(sb[56:]) != 0xef53 {
		return fmt.Errorf("CACHE is not ext4; no format attempted")
	}
	if le.Uint16(sb[58:])&1 == 0 || le.Uint16(sb[58:])&2 != 0 || le.Uint32(sb[96:])&4 != 0 {
		return fmt.Errorf("CACHE needs recovery; native code never replays/repairs it")
	}
	if le.Uint32(sb[24:]) > 6 {
		return fmt.Errorf("CACHE block size invalid")
	}
	return nil
}
func OpenCacheStore(explicitConsent bool) (out *CacheStore, err error) {
	if os.Getpid() != 1 {
		return nil, fmt.Errorf("CACHE access only from pinned native PID1")
	}
	if _, err = Identity(); err != nil {
		return nil, err
	}
	device, err := writableSettingsDevice()
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(cacheMount, 0700); err != nil {
		return nil, err
	}
	owner := &CacheStore{device: device}
	defer func() {
		if err != nil {
			err = errors.Join(err, owner.Close())
		}
	}()
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC | syscall.MS_NOATIME)
	if err = syscall.Mount(device, cacheMount, "ext4", flags|syscall.MS_RDONLY, selinux.CacheMountOptions); err != nil {
		return nil, err
	}
	owner.mounted = true
	dir := cacheMount + "/s7-native"
	if !explicitConsent {
		info, e := os.Lstat(dir)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("CACHE persistence not opted in")
		}
		info, e = os.Lstat(dir + "/consent")
		if e != nil || !info.Mode().IsRegular() || info.Size() > 256 {
			return nil, fmt.Errorf("CACHE consent missing or unsafe")
		}
		b, e := os.ReadFile(dir + "/consent")
		if e != nil || !bytes.Equal(b, []byte(cacheConsent())) {
			return nil, fmt.Errorf("CACHE consent for this device not found")
		}
	}
	if err = syscall.Unmount(cacheMount, 0); err != nil {
		return nil, err
	}
	owner.mounted = false
	// Recheck the exact device and superblock before allowing ext4 journal replay.
	if _, err = writableSettingsDevice(); err != nil {
		return nil, err
	}
	if err = syscall.Mount(device, cacheMount, "ext4", flags, selinux.CacheRWMountOptions); err != nil {
		return nil, err
	}
	owner.mounted = true
	if err = os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	owner.Settings = &SettingsStore{Directory: dir}
	if err = owner.Settings.checkDir(); err != nil {
		return nil, err
	}
	if explicitConsent {
		path := dir + "/consent"
		if st, e := os.Lstat(path); e == nil && !st.Mode().IsRegular() {
			return nil, fmt.Errorf("unsafe consent destination")
		}
		f, e := os.CreateTemp(dir, ".consent-*")
		if e != nil {
			return nil, e
		}
		temp := f.Name()
		defer os.Remove(temp)
		if _, e = f.Write([]byte(cacheConsent())); e != nil {
			f.Close()
			return nil, e
		}
		if e = f.Sync(); e != nil {
			f.Close()
			return nil, e
		}
		if e = f.Close(); e != nil {
			return nil, e
		}
		if e = os.Rename(temp, path); e != nil {
			return nil, e
		}
		d, e := os.Open(dir)
		if e != nil {
			return nil, e
		}
		e = d.Sync()
		ce := d.Close()
		if e != nil {
			return nil, e
		}
		if ce != nil {
			return nil, ce
		}
	}
	return owner, nil
}

func writableSettingsDevice() (string, error) {
	device, err := cacheDevice(false)
	if err != nil {
		return "", err
	}
	f, err := os.Open(device)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sb := make([]byte, 1024)
	if _, err = f.ReadAt(sb, 1024); err != nil {
		return "", err
	}
	return device, checkSettingsSuperblock(sb)
}

func checkSettingsSuperblock(sb []byte) error {
	if err := checkCacheReadable(sb); err != nil {
		return err
	}
	le := binary.LittleEndian
	if le.Uint16(sb[58:])&2 != 0 {
		return fmt.Errorf("CACHE has filesystem errors; repair refused")
	}
	if le.Uint32(sb[96:])&4 != 0 && le.Uint32(sb[92:])&4 == 0 {
		return fmt.Errorf("CACHE recovery flag without a journal")
	}
	return nil
}
func (c *CacheStore) Close() error {
	if c == nil || !c.mounted {
		return nil
	}
	if e := syscall.Unmount(cacheMount, 0); e != nil {
		return e
	}
	c.mounted = false
	return nil
}

// A dirty CACHE may be inspected read-only, without replay/repair. This does not
// enable persistence or relax CacheDevice's clean-state requirement for writes.
func checkCacheReadable(sb []byte) error {
	if len(sb) != 1024 || binary.LittleEndian.Uint16(sb[56:]) != 0xef53 || binary.LittleEndian.Uint32(sb[24:]) > 6 {
		return fmt.Errorf("invalid readable CACHE superblock")
	}
	return nil
}
func ReadCacheDiagnostics() (record *journal.Record, err error) {
	if os.Getpid() != 1 {
		return nil, fmt.Errorf("CACHE recovery only in pinned PID1")
	}
	if _, err = Identity(); err != nil {
		return nil, err
	}
	dev, err := cacheDevice(false)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(cacheMount, 0700); err != nil {
		return nil, err
	}
	flags := uintptr(syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC | syscall.MS_NOATIME)
	if err = syscall.Mount(dev, cacheMount, "ext4", flags, selinux.CacheMountOptions); err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, syscall.Unmount(cacheMount, 0)) }()
	return journal.Inspect(cacheMount+"/s7-native", PinnedSerial)
}

//go:build linux && (amd64 || arm64)

package appliance

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"perimode/native/internal/selinux"
)

const runtimeSourcePath = "/etc/s7-codec/runtime-source"
const systemRuntimeMount = "/run/s7-appliance-system"

var applianceSystemUUID = [16]byte{0x53, 0x37, 0x4e, 0x41, 0x50, 0x50, 0x4c, 0x49, 0x41, 0x4e, 0x43, 0x45, 0x00, 0x00, 0x00, 0x01}

func loadRuntimeSource(path string) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() < 1 || st.Size() > 16 {
		return "", fmt.Errorf("invalid media runtime source selector")
	}
	b, err := io.ReadAll(io.LimitReader(f, 17))
	if err != nil {
		return "", err
	}
	source := strings.TrimSuffix(string(b), "\n")
	if source != "cache" && source != "system" {
		return "", fmt.Errorf("unknown media runtime source %q", source)
	}
	return source, nil
}

func runtimeSource() (string, error) { return loadRuntimeSource(runtimeSourcePath) }

func systemDevice() (string, error) {
	return systemDeviceWithIdentity(applianceSystemUUID, "S7_APPLIANCE")
}

func systemDeviceWithIdentity(uuid [16]byte, label string) (string, error) {
	paths, _ := filepath.Glob("/sys/class/block/*/uevent")
	result := ""
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fields := map[string]string{}
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				fields[key] = value
			}
		}
		if fields["PARTNAME"] != "SYSTEM" {
			continue
		}
		real, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil || !strings.Contains(real, "/155a0000.ufs/") {
			return "", fmt.Errorf("SYSTEM not on pinned UFS controller")
		}
		name := fields["DEVNAME"]
		if name == "" || strings.ContainsAny(name, "/\\. \t\n") {
			return "", fmt.Errorf("invalid SYSTEM device node")
		}
		device := "/dev/" + name
		st, err := os.Stat(device)
		if err != nil || st.Mode()&os.ModeDevice == 0 || st.Mode()&os.ModeCharDevice != 0 {
			return "", fmt.Errorf("SYSTEM node missing or not block")
		}
		sys := st.Sys().(*syscall.Stat_t)
		major := (sys.Rdev>>8)&0xfff | (sys.Rdev>>32)&0xfffff000
		minor := sys.Rdev&0xff | (sys.Rdev>>12)&0xffffff00
		expectedMajor, majorErr := strconv.ParseUint(fields["MAJOR"], 10, 64)
		expectedMinor, minorErr := strconv.ParseUint(fields["MINOR"], 10, 64)
		if majorErr != nil || minorErr != nil || major != expectedMajor || minor != expectedMinor {
			return "", fmt.Errorf("SYSTEM device identity mismatch")
		}
		mounts, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			return "", err
		}
		identity := fmt.Sprintf("%d:%d", major, minor)
		for _, line := range strings.Split(string(mounts), "\n") {
			parts := strings.Fields(line)
			if len(parts) > 2 && parts[2] == identity {
				return "", fmt.Errorf("SYSTEM already mounted; takeover refused")
			}
		}
		if result != "" {
			return "", fmt.Errorf("ambiguous SYSTEM partition")
		}
		result = device
	}
	if result == "" {
		return "", fmt.Errorf("SYSTEM partition unavailable")
	}
	if err := selinux.Label(result, "u:object_r:system_block_device:s0", selinux.KindBlock); err != nil {
		return "", err
	}
	f, err := os.Open(result)
	if err != nil {
		return "", err
	}
	defer f.Close()
	super := make([]byte, 1024)
	if _, err = f.ReadAt(super, 1024); err != nil {
		return "", err
	}
	if binary.LittleEndian.Uint16(super[56:]) != 0xef53 || binary.LittleEndian.Uint16(super[58:])&1 == 0 {
		return "", fmt.Errorf("appliance SYSTEM is not a clean ext4 filesystem")
	}
	if !systemIdentityMatches(super, uuid, label) {
		return "", fmt.Errorf("foreign SYSTEM filesystem refused")
	}
	return result, nil
}

func systemIdentityMatches(super []byte, uuid [16]byte, label string) bool {
	return len(super) >= 136 && bytes.Equal(super[104:120], uuid[:]) &&
		strings.TrimRight(string(super[120:136]), "\x00") == label
}

func mountSystemRuntime() (root string, close func() error, err error) {
	if os.Getpid() != 1 {
		return "", nil, fmt.Errorf("SYSTEM runtime mount requires PID1")
	}
	device, err := systemDevice()
	if err != nil {
		return "", nil, err
	}
	if err = os.Mkdir(systemRuntimeMount, 0555); err != nil && !errors.Is(err, os.ErrExist) {
		return "", nil, err
	}
	flags := uintptr(syscall.MS_RDONLY | syscall.MS_NODEV | syscall.MS_NOSUID | syscall.MS_NOEXEC)
	if err = syscall.Mount(device, systemRuntimeMount, "ext4", flags, selinux.SystemMountOptions); err != nil {
		return "", nil, err
	}
	return systemRuntimeMount, func() error { return syscall.Unmount(systemRuntimeMount, 0) }, nil
}

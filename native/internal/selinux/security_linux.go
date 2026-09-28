//go:build linux && (amd64 || arm64)

// Package selinux implements the appliance's first-policy boot, not a policy
// override inside the media namespace. SELinux is global to the kernel.
package selinux

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const Base = "/etc/s7-security"
const Mount = "/sys/fs/selinux"
const ReadyArg = "--selinux-ready"
const NativeContext = "u:r:s7_native:s0"
const ClientContext = "u:r:s7_media_client:s0"
const CacheRWMountOptions = "errors=remount-ro,context=u:object_r:s7_native_cache:s0"
const CacheMountOptions = "noload,context=u:object_r:s7_native_cache:s0"
const SystemMountOptions = "noload,context=u:object_r:s7_appliance_system:s0"
const RuntimeMountOptions = "mode=0755,size=384m,fscontext=u:object_r:rootfs:s0,rootcontext=u:object_r:rootfs:s0"
const maxPolicy = 2 << 20

// Values are a BOOT input, never supplied by an unverified installed runtime.
type Pin struct {
	Schema        string `json:"schema"`
	Version       int    `json:"policy_version"`
	SHA256        string `json:"sha256"`
	Bytes         int    `json:"bytes"`
	LabelsSHA256  string `json:"labels_sha256"`
	LabelsBytes   int    `json:"labels_bytes"`
	RuntimeSHA256 string `json:"runtime_sha256"`
	Enforcing     bool   `json:"enforcing_required"`
	Reload        bool   `json:"reload_existing_policy"`
	Permissive    bool   `json:"new_domains_permissive"`
}

func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func validHash(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}
func (p Pin) Validate() error {
	if p.Schema != "S7-NATIVE-SELINUX-1" || p.Version != 30 || p.Bytes < 4096 || p.Bytes > maxPolicy || p.LabelsBytes < 1 || p.LabelsBytes > 1<<20 || !p.Enforcing || p.Reload || p.Permissive || !validHash(p.SHA256) || !validHash(p.LabelsSHA256) || !validHash(p.RuntimeSHA256) {
		return errors.New("invalid pinned SELinux configuration")
	}
	return nil
}
func regular(path string, limit int64) ([]byte, error) {
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	s, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !s.Mode().IsRegular() || s.Size() < 1 || s.Size() > limit {
		return nil, errors.New("invalid security input extent/type")
	}
	return io.ReadAll(io.LimitReader(f, limit+1))
}
func decode(b []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing security metadata")
	}
	return nil
}
func LoadPin() (Pin, error) {
	var p Pin
	b, e := regular(Base+"/PIN.json", 4096)
	if e != nil {
		return p, e
	}
	if e = decode(b, &p); e != nil {
		return p, e
	}
	return p, p.Validate()
}
func pinned(path string, size int, hash string) ([]byte, error) {
	b, e := regular(path, int64(size))
	if e != nil {
		return nil, e
	}
	if len(b) != size || digest(b) != hash {
		return nil, errors.New("SELinux input pin mismatch")
	}
	return b, nil
}

type kernelStatus struct{ Enforcing, Loads uint32 }

func decodeStatus(b []byte) (kernelStatus, error) {
	if len(b) < 20 {
		return kernelStatus{}, errors.New("short SELinux status page")
	}
	if binary.LittleEndian.Uint32(b) != 1 || binary.LittleEndian.Uint32(b[4:])&1 != 0 {
		return kernelStatus{}, errors.New("unstable/unknown SELinux status ABI")
	}
	return kernelStatus{binary.LittleEndian.Uint32(b[8:]), binary.LittleEndian.Uint32(b[12:])}, nil
}
func observe() (kernelStatus, error) {
	var fs syscall.Statfs_t
	if e := syscall.Statfs(Mount, &fs); e != nil {
		return kernelStatus{}, e
	}
	if uint64(fs.Type) != 0xf97cff8c {
		return kernelStatus{}, errors.New("SELinux mount has wrong filesystem")
	}
	f, e := os.Open(Mount + "/status")
	if e != nil {
		return kernelStatus{}, e
	}
	defer f.Close()
	m, e := syscall.Mmap(int(f.Fd()), 0, os.Getpagesize(), syscall.PROT_READ, syscall.MAP_SHARED)
	if e != nil {
		return kernelStatus{}, e
	}
	defer syscall.Munmap(m)
	// Kernel updates this status page using a seqlock. Do not mistake an in-flight
	// update for an unloaded policy or an enforcing state.
	for n := 0; n < 16; n++ {
		seq := atomic.LoadUint32((*uint32)(unsafe.Pointer(&m[4])))
		if seq&1 != 0 {
			continue
		}
		b := append([]byte(nil), m[:20]...)
		if seq != atomic.LoadUint32((*uint32)(unsafe.Pointer(&m[4]))) {
			continue
		}
		return decodeStatus(b)
	}
	return kernelStatus{}, errors.New("SELinux status remained unstable")
}
func Current() (string, error) {
	b, e := os.ReadFile("/proc/self/attr/current")
	if e != nil {
		return "", e
	}
	return strings.TrimRight(string(b), "\x00\n"), nil
}
func RequireContext(want string) error {
	s, e := observe()
	if e != nil {
		return e
	}
	if s.Enforcing != 1 || s.Loads == 0 {
		return errors.New("loaded enforcing SELinux policy required")
	}
	got, e := Current()
	if e != nil {
		return e
	}
	if got != want {
		return fmt.Errorf("SELinux context %q; expected %q", got, want)
	}
	return nil
}
func firstLoadAllowed(s kernelStatus) error {
	if s.Loads != 0 {
		return errors.New("foreign/already loaded SELinux policy; replacement refused")
	}
	if s.Enforcing > 1 {
		return errors.New("invalid initial SELinux enforcement state")
	}
	return nil
}

func setEnforcing() error {
	f, err := os.OpenFile(Mount+"/enforce", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	n, writeErr := f.Write([]byte("1"))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr)
	}
	if n != 1 {
		return io.ErrShortWrite
	}
	return nil
}

func enforceLoadedPolicy(initial kernelStatus, enable func() error, current func() (kernelStatus, error)) error {
	if initial.Enforcing == 0 {
		if err := enable(); err != nil {
			return fmt.Errorf("enable SELinux enforcing mode: %w", err)
		}
	}
	status, err := current()
	if err != nil {
		return err
	}
	if status.Enforcing != 1 || status.Loads != 1 {
		return fmt.Errorf("pinned SELinux policy not singly loaded and enforcing: %+v", status)
	}
	return nil
}

// FirstBoot runs once before watchdog/subsystem goroutines. Policy state is
// irreversible here: any failure stops boot. The only mode write is 0 -> 1
// after the pinned policy is loaded; this code never writes permissive mode.
// Exec performs the kernel->s7_native transition for the whole new process;
// writing attr/current from one Go thread would NOT change all runtime threads.
func FirstBoot() error {
	if os.Getpid() != 1 || os.Geteuid() != 0 {
		return errors.New("SELinux first load is restricted to native PID1")
	}
	pin, e := LoadPin()
	if e != nil {
		return e
	}
	policy, e := pinned(Base+"/sepolicy", pin.Bytes, pin.SHA256)
	if e != nil {
		return e
	}
	if _, e = pinned(Base+"/labels.json", pin.LabelsBytes, pin.LabelsSHA256); e != nil {
		return e
	}
	if e = syscall.Mount("selinuxfs", Mount, "selinuxfs", syscall.MS_NOSUID|syscall.MS_NOEXEC, ""); e != nil && e != syscall.EBUSY {
		return e
	}
	status, e := observe()
	if e != nil {
		return e
	}
	if e = firstLoadAllowed(status); e != nil {
		return e
	}
	f, e := os.OpenFile(Mount+"/load", os.O_WRONLY, 0)
	if e != nil {
		return e
	}
	n, we := f.Write(policy)
	ce := f.Close()
	if we != nil || ce != nil {
		return errors.Join(we, ce)
	}
	if n != len(policy) {
		return io.ErrShortWrite
	}
	if e = enforceLoadedPolicy(status, setEnforcing, observe); e != nil {
		return e
	}
	// No further mutable boot state or user data is needed in the kernel domain.
	if e = Label("/init", "u:object_r:s7_native_exec:s0", KindFile); e != nil {
		return e
	}
	return syscall.Exec("/init", []string{"/init", ReadyArg}, []string{"PATH=/sbin:/system/bin"})
}

const (
	KindFile  = "file"
	KindDir   = "dir"
	KindChar  = "char"
	KindBlock = "block"
)

var contextPattern = regexp.MustCompile(`^u:object_r:[a-z][a-z0-9_]*:s0$`)

func kindMatches(mode os.FileMode, kind string) bool {
	switch kind {
	case KindFile:
		return mode.IsRegular()
	case KindDir:
		return mode.IsDir()
	case KindChar:
		return mode&os.ModeDevice != 0 && mode&os.ModeCharDevice != 0
	case KindBlock:
		return mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0
	}
	return false
}

// lsetxattr never follows the final component. Callers use their own fresh RAM
// tree or validated devtmpfs device path, never a user-controlled disk path.
func Label(path, context, kind string) error {
	if !contextPattern.MatchString(context) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("invalid SELinux label request")
	}
	s, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !kindMatches(s.Mode(), kind) {
		return fmt.Errorf("security label wrong object kind: %s", path)
	}
	name, e := syscall.BytePtrFromString(path)
	if e != nil {
		return e
	}
	attr, _ := syscall.BytePtrFromString("security.selinux")
	value := append([]byte(context), 0)
	_, _, errno := syscall.Syscall6(syscall.SYS_LSETXATTR, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(attr)), uintptr(unsafe.Pointer(&value[0])), uintptr(len(value)), 0, 0)
	if errno != 0 {
		return fmt.Errorf("SELinux label %s: %w", path, errno)
	}
	return nil
}
func Resume() error {
	if os.Getpid() != 1 {
		return errors.New("native SELinux resume requires PID1")
	}
	if e := RequireContext(NativeContext); e != nil {
		return e
	}
	// These are in RAM. No partition inode/xattr is modified.
	for path, ty := range map[string]string{"/opt/s7-codec/bin/mediacodec-bridge": "s7_codec_exec"} {
		if e := Label(path, "u:object_r:"+ty+":s0", KindFile); e != nil {
			return e
		}
	}
	for name, ty := range map[string]string{"null": "null_device", "zero": "zero_device", "random": "random_device", "urandom": "random_device", "kmsg": "kmsg_device"} {
		if e := Label("/dev/"+name, "u:object_r:"+ty+":s0", KindChar); e != nil {
			return e
		}
	}
	return nil
}

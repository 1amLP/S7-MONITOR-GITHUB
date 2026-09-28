//go:build linux && (amd64 || arm64)

package sensorhub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"perimode/native/internal/selinux"
)

const ChildArgument = "--sensorhub-child"
const stateDirectory = "/run/s7-sensorhub"

// This is a fixed ramdisk/devtmpfs plan, not a path list from writable CACHE.
// Device identities are matched to kernel sysfs before relabeling. No GPIO,
// sysfs, partition, firmware or on-disk extended attribute is changed here.
type launchFile struct{ path, kind, label string }

func launchFiles() []launchFile {
	files := []launchFile{{stateDirectory, selinux.KindDir, "s7_hub_state"}}
	names := make([]string, 0, len(runtimeFiles))
	for name := range runtimeFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		label := "s7_hub_library"
		if name == "bin/lhd" {
			label = "s7_hub_exec"
		}
		if name == "lhd.conf" {
			label = "s7_hub_config"
		}
		files = append(files, launchFile{Prefix + "/" + name, selinux.KindFile, label})
	}
	for _, name := range []string{"bbd_patch", "bbd_control", "bbd_sensor", "ttyBCM"} {
		files = append(files, launchFile{"/dev/" + name, selinux.KindChar, "bbd_device"})
	}
	return files
}

type launchFS struct {
	stat  func(string) (os.FileInfo, error)
	fs    func(string) (int64, error)
	read  func(string) ([]byte, error)
	mkdir func(string, os.FileMode) error
	label func(string, string, string) error
}

func nativeLaunchFS() launchFS {
	return launchFS{os.Lstat, func(p string) (int64, error) {
		var st syscall.Statfs_t
		e := syscall.Statfs(p, &st)
		return int64(st.Type), e
	}, os.ReadFile, os.Mkdir, selinux.Label}
}
func requireRAM(f launchFS, path string) error {
	typ, e := f.fs(path)
	if e != nil {
		return e
	}
	if typ != 0x01021994 && typ != 0x858458f6 { // TMPFS_MAGIC / RAMFS_MAGIC
		return fmt.Errorf("hub launch path is not RAM-backed: %s", path)
	}
	return nil
}
func kernelDeviceName(data []byte, name string) bool {
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "DEVNAME=") {
			count++
			if line != "DEVNAME="+name {
				return false
			}
		}
	}
	return count == 1
}
func validateLaunchFile(f launchFS, item launchFile) error {
	// All ancestors belong to fixed in-memory boot trees. Reject symlinks even
	// for directories; lsetxattr alone protects only the final path component.
	for path := item.path; ; path = filepath.Dir(path) {
		st, e := f.stat(path)
		if e != nil {
			return e
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("hub launch symlink: %s", path)
		}
		if path != item.path && !st.IsDir() {
			return fmt.Errorf("hub ancestor not a directory: %s", path)
		}
		if path == "/" {
			break
		}
	}
	if e := requireRAM(f, item.path); e != nil {
		return e
	}
	st, e := f.stat(item.path)
	if e != nil {
		return e
	}
	if item.kind == selinux.KindDir {
		if !st.IsDir() || st.Mode().Perm() != 0700 {
			return errors.New("hub RAM state must be a private 0700 directory")
		}
	} else if item.kind == selinux.KindFile {
		if !st.Mode().IsRegular() {
			return fmt.Errorf("hub launch file is not regular: %s", item.path)
		}
	} else if item.kind == selinux.KindChar {
		if st.Mode()&os.ModeDevice == 0 || st.Mode()&os.ModeCharDevice == 0 {
			return fmt.Errorf("hub path not a character device: %s", item.path)
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.New("hub device stat unavailable")
		}
		major := ((sys.Rdev >> 8) & 0xfff) | ((sys.Rdev >> 32) & 0xfffff000)
		minor := (sys.Rdev & 0xff) | ((sys.Rdev >> 12) & 0xffffff00)
		raw, err := f.read(fmt.Sprintf("/sys/dev/char/%d:%d/uevent", major, minor))
		if err != nil {
			return err
		}
		if !kernelDeviceName(raw, filepath.Base(item.path)) {
			return fmt.Errorf("hub kernel device identity mismatch: %s", item.path)
		}
	} else {
		return errors.New("unknown hub launch object")
	}
	return nil
}
func prepareLaunch(ctx context.Context, f launchFS) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := requireRAM(f, "/run"); e != nil {
		return e
	}
	st, e := f.stat("/run")
	if e != nil {
		return e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("hub /run is not a real directory")
	}
	if e = f.mkdir(stateDirectory, 0700); e != nil && !errors.Is(e, os.ErrExist) {
		return e
	}
	files := launchFiles()
	// Validate the ENTIRE plan before the first label change. On any label
	// failure, no helper starts; a later retry can reapply the same exact plan.
	for _, item := range files {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := validateLaunchFile(f, item); e != nil {
			return e
		}
	}
	for _, item := range files {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := f.label(item.path, "u:object_r:"+item.label+":s0", item.kind); e != nil {
			return e
		}
	}
	return nil
}

func hubProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Cloneflags: syscall.CLONE_NEWNET}
}
func privateNetwork(readlink func(string) (string, error)) error {
	own, e := readlink("/proc/self/ns/net")
	if e != nil {
		return e
	}
	parent, e := readlink("/proc/1/ns/net")
	if e != nil {
		return e
	}
	if !strings.HasPrefix(own, "net:[") || !strings.HasSuffix(own, "]") || !strings.HasPrefix(parent, "net:[") || !strings.HasSuffix(parent, "]") || own == parent {
		return errors.New("sensor hub child is not in its private network namespace")
	}
	return nil
}

// The Linux 64-bit ifreq ABI is 40 bytes on both supported architectures.
// Only lo's IFF_UP changes. There is no route, veth, physical interface or USB
// reconfiguration. Using an AF_UNIX control socket avoids a supervisor TCP API.
func loopbackUp() error {
	var req struct {
		Name  [16]byte
		Flags uint16
		Pad   [22]byte
	}
	if unsafe.Sizeof(req) != 40 {
		return errors.New("unexpected ifreq ABI")
	}
	copy(req.Name[:], "lo")
	fd, e := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if e != nil {
		return e
	}
	defer syscall.Close(fd)
	for _, op := range []uintptr{0x8913, 0x8914, 0x8913} { // GET / SET / GET flags
		if op == 0x8914 {
			req.Flags |= 1
		}
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), op, uintptr(unsafe.Pointer(&req)))
		if errno != 0 {
			return fmt.Errorf("private loopback ioctl %#x: %w", op, errno)
		}
	}
	if req.Flags&1 == 0 {
		return errors.New("private loopback did not become UP")
	}
	return nil
}

// This entry is only spawned by the outer PID1. It does not emulate lhd or
// construct sensor data. After network setup it becomes the original binary.
func configurePrivateNetwork(readlink func(string) (string, error), up func() error) error {
	if e := privateNetwork(readlink); e != nil {
		return e
	}
	return up()
}

func ChildMain() (result error) {
	if os.Getpid() == 1 || os.Getppid() != 1 || os.Geteuid() != 0 {
		return errors.New("sensor hub child requires native PID1 parent")
	}
	if e := selinux.RequireContext(selinux.NativeContext); e != nil {
		return e
	}
	status := os.NewFile(3, "sensorhub-exec-status")
	st, e := status.Stat()
	if e != nil {
		return e
	}
	if st.Mode()&os.ModeNamedPipe == 0 {
		return errors.New("sensor hub startup status is not a pipe")
	}
	defer func() {
		if result != nil {
			_, _ = fmt.Fprintf(status, "%.768s", result.Error())
		}
		_ = status.Close()
	}()
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, 3, syscall.F_SETFD, syscall.FD_CLOEXEC); errno != 0 {
		return fmt.Errorf("sensor hub status CLOEXEC: %w", errno)
	}
	if e := configurePrivateNetwork(os.Readlink, loopbackUp); e != nil {
		return e
	}
	if e := os.Chdir(stateDirectory); e != nil {
		return e
	}
	syscall.Umask(0077)
	// Explicit clean environment; no inherited preload, Android properties or
	// writable loader search directory. Exec performs the SELinux transition.
	return syscall.Exec(Prefix+"/bin/lhd", []string{Prefix + "/bin/lhd", Prefix + "/lhd.conf"},
		[]string{"LD_LIBRARY_PATH=" + Prefix + "/lib64", "PATH=" + Prefix + "/bin", "TZ=UTC"})
}

// EOF is produced by CLOEXEC on the original lhd exec, not a synthetic "ready"
// byte from the launcher. Check the executable inode as well: a dead launcher
// closing all descriptors must not count as successful exec.
func awaitExec(ctx context.Context, pipe *os.File, confirm func() error) error {
	deadline := time.Now().Add(StartupGrace)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if e := pipe.SetReadDeadline(deadline); e != nil {
		return e
	}
	stop := context.AfterFunc(ctx, func() { _ = pipe.Close() })
	defer stop()
	raw, e := io.ReadAll(io.LimitReader(pipe, 1025))
	if ce := ctx.Err(); ce != nil {
		return ce
	}
	if e != nil {
		return fmt.Errorf("sensor hub exec handshake: %w", e)
	}
	if len(raw) > 1024 {
		return errors.New("oversized sensor hub exec failure")
	}
	if len(raw) != 0 {
		return fmt.Errorf("sensor hub launcher: %s", raw)
	}
	return confirm()
}
func startIsolatedHub(ctx context.Context) (Process, error) {
	reader, writer, e := os.Pipe()
	if e != nil {
		return nil, e
	}
	defer reader.Close()
	p, e := startConfiguredCommand(ctx, "/init", []string{ChildArgument}, []string{"PATH=/opt/s7-hub/bin", "TZ=UTC"}, cleanupWake, hubProcessAttributes(), writer)
	_ = writer.Close()
	if e != nil {
		return p, e
	}
	command := p.(*commandProcess)
	e = awaitExec(ctx, reader, func() error {
		if exited(p) {
			return exitError(p)
		}
		actual, e := os.Stat(fmt.Sprintf("/proc/%d/exe", command.cmd.Process.Pid))
		if e != nil {
			return e
		}
		expected, e := os.Stat(Prefix + "/bin/lhd")
		if e != nil {
			return e
		}
		if !os.SameFile(actual, expected) {
			return errors.New("hub launcher did not exec the original lhd")
		}
		return nil
	})
	// Return the owner even on failure. Manager stops/reaps it and preserves
	// ownership if stop cannot be confirmed. Never leave an untracked helper.
	return p, e
}

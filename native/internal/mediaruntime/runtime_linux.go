//go:build linux && (amd64 || arm64)

package mediaruntime

import (
	"context"
	"errors"
	"fmt"
	"perimode/native/internal/childproc"
	"perimode/native/internal/selinux"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
)

type Status struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	Ready bool   `json:"launcher_ready"`
}

var state = struct {
	sync.Mutex
	Status
}{Status: Status{State: "NOT STARTED"}}

func Snapshot() Status { state.Lock(); defer state.Unlock(); return state.Status }

// CodecReady distinguishes a still-starting runtime from a terminal failure.
// A registered Binder service is only an admission barrier, not codec success.
func (s Status) CodecReady() (bool, error) {
	if s.Error != "" {
		return false, fmt.Errorf("MediaCodec runtime %s: %s", s.State, s.Error)
	}
	if s.Ready {
		return true, nil
	}
	switch s.State {
	case "NOT STARTED", "UNPACKING", "STARTING NATIVE SERVICES":
		return false, nil
	default:
		return false, fmt.Errorf("MediaCodec runtime unavailable: %s", s.State)
	}
}

func publish(s string, ready bool, e error) {
	state.Lock()
	defer state.Unlock()
	state.Status = Status{State: s, Ready: ready}
	if e != nil {
		state.Error = e.Error()
	}
}
func Fail(e error) { publish("FAILED", false, e) }

// Run mounts executable RAM, not the CACHE partition. The only persistent I/O
// is the caller-supplied read-only archive descriptor. No loop/image device needed.
func Run(ctx context.Context, input *os.File, pin Pin, onUnpacked func() error, onReady func()) (err error) {
	if os.Getpid() != 1 {
		return errors.New("media runtime must be owned by native PID1")
	}
	if e := selinux.RequireContext(selinux.NativeContext); e != nil {
		return e
	}
	labels, e := selinux.LoadLabelPlan(pin.SHA256)
	if e != nil {
		return e
	}
	publish("UNPACKING", false, nil)
	defer func() {
		if err != nil {
			Fail(err)
		} else {
			publish("STOPPED", false, nil)
		}
	}()
	if e := os.Mkdir(Root, 0700); e != nil {
		return e
	}
	if e := os.Mkdir(Control, 0700); e != nil {
		return e
	}
	if err = syscall.Mount("s7-media", Root, "tmpfs", syscall.MS_NODEV, selinux.RuntimeMountOptions); err != nil {
		return err
	}
	mounted := true
	keep := false
	defer func() {
		if mounted && !keep {
			err = errors.Join(err, syscall.Unmount(Root, 0))
		}
	}()
	unpack, stop := context.WithTimeout(ctx, 60*time.Second)
	err = Extract(unpack, input, Root, pin)
	stop()
	if err != nil {
		return err
	}
	if err = validateSandbox(ctx, Root); err != nil {
		return err
	}
	if err = labels.Apply(ctx, Root, false); err != nil {
		return err
	}
	if err = selinux.Label(Control, "u:object_r:s7_media_control:s0", selinux.KindDir); err != nil {
		return err
	}
	if onUnpacked != nil {
		if err = onUnpacked(); err != nil {
			return err
		}
	}
	groups, e := prepareGroups(realGroups{})
	if e != nil {
		return e
	}
	defer func() {
		// Never prune while a codec/service may still own kernel resources.
		if !keep {
			err = errors.Join(err, groups.Close())
		}
	}()
	// Pin is verified before the original Android init/loader receives execution.
	publish("STARTING NATIVE SERVICES", false, nil)
	cmd := exec.Command("/init", "--media-runtime-child")
	cmd.Env = []string{"PATH=/nonexistent"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWPID | syscall.CLONE_NEWUTS, Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Stdout = nil
	cmd.Stderr = os.Stderr
	if err = childproc.Start(cmd); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- childproc.Wait(cmd) }()
	defer func() {
		publish("STOPPING NATIVE SERVICES", false, nil)
		_ = cmd.Process.Kill() // Kill namespace PID1; kernel tears down its descendants.
		select {
		case e := <-done:
			if ctx.Err() == nil && e != nil {
				err = errors.Join(err, e)
			}
		case <-time.After(1200 * time.Millisecond):
			keep = true
			err = errors.Join(err, errors.New("media namespace exit unconfirmed; keeping RAM owner"))
		}
	}()
	deadline, finish := context.WithTimeout(ctx, 15*time.Second)
	defer finish()
	var control net.Conn
	for {
		select {
		case e := <-done:
			done <- e
			return fmt.Errorf("native media init exited before broker: %w", e)
		default:
		}
		control, err = dial(deadline, 0)
		if err == nil {
			break
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("native media service startup: %w", deadline.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer control.Close()
	cancel := context.AfterFunc(ctx, func() { _ = control.Close() })
	defer cancel()
	publish("BINDER/HIDL SERVICES REGISTERED / CODEC NOT YET OPENED", true, nil)
	if onReady != nil {
		onReady()
	}
	_, err = io.Copy(io.Discard, control)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.Join(errors.New("media broker lost; no automatic service restart"), err)
}

func dial(ctx context.Context, role byte) (net.Conn, error) {
	d := net.Dialer{Timeout: 300 * time.Millisecond}
	c, e := d.DialContext(ctx, "unix", Socket)
	if e != nil {
		return nil, e
	}
	_ = c.SetDeadline(time.Now().Add(500 * time.Millisecond))
	hello := []byte{'S', '7', 'R', '3', role, 0, 0, 0}
	if e = writeAll(c, hello); e == nil {
		var reply [8]byte
		_, e = io.ReadFull(c, reply[:])
		if e == nil && (string(reply[:4]) != "S7RA" || reply[4] != role || reply[5] != 0) {
			e = errors.New("media broker rejected role")
		}
	}
	if e != nil {
		c.Close()
		return nil, e
	}
	_ = c.SetDeadline(time.Time{})
	return c, nil
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func touchFile(name string) error {
	f, e := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	return f.Close()
}
func bindRO(source, target string) error { return bindReadonly(source, target, true) }
func bindReadonly(source, target string, nosuid bool) error {
	if e := syscall.Mount(source, target, "", syscall.MS_BIND, ""); e != nil {
		return e
	}
	flags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY)
	if nosuid {
		flags |= syscall.MS_NOSUID
	}
	return syscall.Mount("", target, "", flags, "")
}

// ChildMain is entered via CLONE_NEWPID+CLONE_NEWNS, never on the host PID1.
// A dedicated /dev contains no block devices, USB, watchdog or panel controls.
func ChildMain() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if os.Getpid() != 1 || os.Geteuid() != 0 {
		return errors.New("private media init identity required")
	}
	own, e := os.Readlink("/proc/self/ns/pid")
	if e != nil {
		return e
	}
	parent, e := os.Readlink("/proc/1/ns/pid")
	if e != nil || own == parent {
		return errors.New("refusing Android second-stage init in host PID namespace")
	}
	if e = syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); e != nil {
		return e
	}
	if e = selinux.RequireContext(selinux.NativeContext); e != nil {
		return e
	}
	pin, e := LoadPin()
	if e != nil {
		return e
	}
	labels, e := selinux.LoadLabelPlan(pin.SHA256)
	if e != nil {
		return e
	}
	// The broker exec must undergo a SELinux type transition. NOSUID would prevent
	// that on kernels without the optional nosuid_transition policy capability.
	// The pinned file has no SUID/SGID bits; only this executable bind drops NOSUID.
	if e = bindReadonly("/init", Root+"/s7/native", false); e != nil {
		return e
	}
	if e = bindRO("/opt/s7-codec/bin/mediacodec-bridge", Root+"/s7/mediacodec-bridge"); e != nil {
		return e
	}
	if e = syscall.Mount(Control, Root+"/s7-control", "", syscall.MS_BIND, ""); e != nil {
		return e
	}
	// Android init creates its own APEX/linker mountpoints. Retain our flattened,
	// verified files at stable private paths; early-init restores them without apexd.
	for _, name := range []string{"apex", "linkerconfig"} {
		if e = bindRO(Root+"/"+name, Root+"/s7/"+name+"-seed"); e != nil {
			return e
		}
		if e = bindRO(Root+"/s7/"+name+"-seed", Root+"/"+name); e != nil {
			return e
		}
	}
	if e = bindRO(Root+"/s7/apex-seed", Root+"/bootstrap-apex"); e != nil {
		return e
	}
	if e = bindRO(Root+"/system/system_ext", Root+"/system_ext"); e != nil {
		return e
	}
	// OTA /etc is a symlink. Our archive deliberately excludes links, so make
	// the equivalent read-only mount explicitly before init reads task profiles.
	if e = os.Mkdir(Root+"/etc", 0755); e != nil {
		return e
	}
	if e = bindRO(Root+"/system/etc", Root+"/etc"); e != nil {
		return e
	}

	for _, d := range []struct{ name, size string }{{"dev", "16m"}, {"data", "8m"}, {"run", "8m"}, {"tmp", "4m"}} {
		if e = syscall.Mount("tmpfs", Root+"/"+d.name, "tmpfs", syscall.MS_NOSUID|syscall.MS_NOEXEC, "mode=0755,size="+d.size); e != nil {
			return e
		}
	}
	if e = installGroups(Root, realGroups{}); e != nil {
		return e
	}
	// StartPropertyService runs before rc early-init. Its socket directory must
	// exist in our new /dev already, not only in the later rc mkdir commands.
	if e = os.Mkdir(Root+"/dev/socket", 0755); e != nil {
		return e
	}
	syscall.Umask(0)
	// Private copies of an explicit character-device allowlist. No full /dev bind.
	required := []string{"null", "zero", "random", "urandom", "binder", "hwbinder", "vndbinder", "ashmem", "ion"}
	for _, name := range append(required, "kmsg", "mali0") {
		s, e := os.Stat("/dev/" + name)
		if e != nil {
			if name == "kmsg" || name == "mali0" {
				continue
			}
			return fmt.Errorf("media device %s: %w", name, e)
		}
		if s.Mode()&os.ModeCharDevice == 0 {
			return fmt.Errorf("media device is not character: %s", name)
		}

		// Copy device identity, not its host inode: permissions below are private.
		v := s.Sys().(*syscall.Stat_t)
		if e = syscall.Mknod(Root+"/dev/"+name, syscall.S_IFCHR|0666, int(v.Rdev)); e != nil {
			return e
		}
		if name == "kmsg" {
			_ = os.Chmod(Root+"/dev/"+name, 0600)
		}

	}
	// Only MFC V4L2 codec nodes; never expose the camera ISP or other video devices.
	videos, _ := filepath.Glob("/sys/class/video4linux/video*/name")
	for _, p := range videos {
		b, e := os.ReadFile(p)
		if e != nil {
			continue
		}
		if !isMFCName(string(b)) {
			continue
		}
		name := filepath.Base(filepath.Dir(p))
		source := "/dev/" + name
		st, e := os.Stat(source)
		if e != nil || st.Mode()&os.ModeCharDevice == 0 {
			return errors.New("MFC node unavailable")
		}

		v := st.Sys().(*syscall.Stat_t)
		if e = syscall.Mknod(Root+"/dev/"+name, syscall.S_IFCHR|0660, int(v.Rdev)); e != nil {
			return e
		}
		if e = selinux.LabelMFC(Root, "/dev/"+name); e != nil {
			return e
		}
		if e = os.Chown(Root+"/dev/"+name, 0, 1006); e != nil {
			return e
		} // camera group, private inode

	}
	if e = syscall.Mount("proc", Root+"/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); e != nil {
		return e
	}
	if e = syscall.Mount("sysfs", Root+"/sys", "sysfs", syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); e != nil {
		return e
	}
	// libselinux rejects an entirely read-only selinuxfs as disabled. Expose
	// the real query interface; the pinned binary policy denies load_policy,
	// setenforce and setbool to init, native supervisor and media clients.
	if e = syscall.Mount(selinux.Mount, Root+selinux.Mount, "", syscall.MS_BIND, ""); e != nil {
		return e
	}
	if e = isolateProcControls(Root); e != nil {
		return e
	}
	// This is our completed *allowlisted* device setup, not a claim that a full
	// Android coldboot ran. The matching rc publishes ro.cold_boot_done only now.
	if e = touchFile(Root + "/dev/.s7-media-devices-ready"); e != nil {
		return e
	}

	if e = labels.Apply(context.Background(), Root, true); e != nil {
		return e
	}
	// No ro.secure/SELinux bypass, sysfs writes, thermal HAL, vold or ueventd.
	if e = syscall.Mount("", Root, "", syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NODEV, ""); e != nil {
		return e
	}
	if e = enterMediaRoot(); e != nil {
		return e
	}
	return syscall.Exec("/system/bin/init", []string{"/system/bin/init", "second_stage"}, []string{
		"PATH=/system/bin", "ANDROID_ROOT=/system", "ANDROID_DATA=/data", "ANDROID_RUNTIME_ROOT=/apex/com.android.runtime",
		"ANDROID_I18N_ROOT=/apex/com.android.i18n", "ANDROID_ART_ROOT=/nonexistent", "INIT_SECOND_STAGE=1"})
}

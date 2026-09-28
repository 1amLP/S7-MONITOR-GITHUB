//go:build linux

package appliance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const installerImage = gpuSystemMount + "/s7-windows/install.img"
const autoUpdateStatus = "AUTO UPDATE IN PROGRESS"

var errAutoUpdatePending = errors.New("automatic driver update is in progress")

type installerSession struct {
	disk   *installerDisk
	paused bool
	cancel context.CancelFunc
	done   chan struct{}
}

func (u *UI) installerMessage(text string) {
	u.state.mu.Lock()
	u.state.InstallerStatus = text
	u.state.mu.Unlock()
}

func (u *UI) startInstallerMedia() error {
	if u.installer != nil {
		return nil
	}
	if installerUpdating(u.state.EndpointSnapshot()) {
		return errAutoUpdatePending
	}
	if u.transport == nil || u.transport.debug == nil || u.transport.debug.packageData == nil {
		return fmt.Errorf("signed installer package is not provisioned on S7")
	}
	pin := u.transport.debug.packageData.pin
	if pin.MediaBytes <= 0 || pin.MediaBytes > 256<<20 || len(pin.MediaSHA256) != 64 {
		return fmt.Errorf("installer disk pin missing")
	}
	fd, err := syscall.Open(installerImage, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), installerImage)
	owned := false
	defer func() {
		if !owned {
			_ = f.Close()
		}
	}()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != pin.MediaBytes {
		f.Close()
		return fmt.Errorf("installer disk size/type differs")
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != pin.MediaSHA256 {
		return fmt.Errorf("installer disk SHA256 differs")
	}
	s := &installerSession{}
	u.installer = s
	u.state.mu.Lock()
	u.state.InstallerActive = true
	u.state.mu.Unlock()
	rollback := func(e error) error { return errors.Join(e, u.stopInstallerMedia()) }
	requestReturn := func() {
		select {
		case u.installerDone <- struct{}{}:
		default:
		}
	}
	s.disk, err = openInstallerDisk(u.background, f, pin.MediaBytes, func(e error) { u.state.Error(e); u.installerMessage("INSTALLER ERROR: " + e.Error()); requestReturn() }, requestReturn)
	owned = true
	if err != nil {
		return rollback(err)
	}
	if u.transport.Bound() {
		err = u.transport.Toggle()
		s.paused = !u.transport.Bound()
		if err != nil {
			return rollback(err)
		}
	}
	if err = s.disk.g.SetUDC(controller); err != nil {
		return rollback(err)
	}
	u.installerMessage("S7 SETUP / INSTALLER AVAILABLE ON USB")
	ctx, cancel := context.WithCancel(u.background)
	s.cancel = cancel
	s.done = make(chan struct{})
	go func() { defer close(s.done); u.installerCompletion(ctx, pin.Release, s.disk.serialTTY) }()
	return nil
}

func (u *UI) stopInstallerMedia() error {
	s := u.installer
	if s == nil {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.done != nil {
		select {
		case <-s.done:
		case <-time.After(time.Second):
			return fmt.Errorf("installer control reader still active")
		}
	}
	err := s.disk.close()
	if err != nil {
		return err
	}
	if s.paused && u.background.Err() == nil && !u.transport.Bound() {
		err = u.transport.Toggle()
	}
	if err != nil {
		return err
	}
	u.installer = nil
	u.state.mu.Lock()
	u.state.InstallerActive = false
	if !strings.HasPrefix(u.state.InstallerStatus, "INSTALLER ERROR:") {
		u.state.InstallerStatus = "WAITING FOR WINDOWS DRIVER VERIFICATION"
	}
	u.state.mu.Unlock()
	return err
}

func (u *UI) toggleInstallerDisk() {
	u.async(func() error {
		var err error
		if u.installer != nil {
			err = u.stopInstallerMedia()
		} else {
			err = u.startInstallerMedia()
		}
		if err != nil {
			if errors.Is(err, errAutoUpdatePending) {
				u.installerMessage(autoUpdateStatus)
				return nil
			}
			u.installerMessage("INSTALLER ERROR: " + err.Error())
		}
		return err
	})
}

func installerUpdating(v EndpointStatus) bool {
	return v.PackageRelease != 0 && !v.PackageVerified && v.Error == 0x800703e5 &&
		!v.Updated.IsZero() && time.Since(v.Updated) <= 10*time.Second
}

func installerHeader(v EndpointStatus) (string, string) {
	status := installerPrompt(v)
	if status == "" || status == autoUpdateStatus {
		return status, ""
	}
	return status, "OPEN SETUP DISK"
}

func installerPrompt(v EndpointStatus) string {
	if v.PackageRelease == 0 || v.PackageVerified {
		return ""
	}
	if v.Updated.IsZero() || time.Since(v.Updated) > 10*time.Second {
		return "DRIVERS NOT VERIFIED"
	}
	switch v.Error {
	case 0x800703e5:
		return autoUpdateStatus
	case 0x8007051a, 0x80070002:
		return "DRIVER UPDATE REQUIRED"
	case 0x80070643:
		return "DRIVER UPDATE FAILED"
	default:
		return "DRIVERS NOT VERIFIED"
	}
}

func (u *UI) installerCompletion(ctx context.Context, release uint32, tty string) {
	// This ACM channel accepts only a completion notice. It cannot execute
	// commands, change settings or certify the Windows package as installed.
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	var pending string
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if f == nil {
			fd, err := syscall.Open(tty, syscall.O_RDONLY|syscall.O_NOCTTY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if err != nil {
				continue
			}
			f = os.NewFile(uintptr(fd), tty)
			var term syscall.Termios
			_, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&term)))
			if e != 0 {
				f.Close()
				f = nil
				continue
			}
			term.Iflag, term.Oflag, term.Lflag = 0, 0, 0
			term.Cflag = syscall.CS8 | syscall.CREAD | syscall.CLOCAL | syscall.B115200
			term.Cc[syscall.VMIN], term.Cc[syscall.VTIME] = 0, 0
			_, _, e = syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(&term)))
			if e != 0 {
				f.Close()
				f = nil
				continue
			}
		}
		var data [128]byte
		n, _ := syscall.Read(int(f.Fd()), data[:])
		if n > 0 {
			pending += string(data[:n])
		}
		if strings.Contains(pending, fmt.Sprintf("S7-INSTALL-DONE %d\n", release)) {
			select {
			case u.installerDone <- struct{}{}:
			default:
			}
			return
		}
		if len(pending) > 256 {
			pending = pending[len(pending)-128:]
		}
	}
}

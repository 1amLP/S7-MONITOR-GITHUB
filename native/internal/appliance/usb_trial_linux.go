//go:build linux && (amd64 || arm64)

package appliance

import (
	"fmt"
	"io"
	"os"
	"syscall"

	"perimode/native/internal/camera"
	"perimode/native/pkg/cameramode"
)

const usbTrialPath = "/etc/s7-codec/usb-trial-fps"

func parseUSBTrial(data []byte) (uint32, error) {
	switch string(data) {
	case "120\n":
		return 120, nil
	case "240\n":
		return 240, nil
	default:
		return 0, fmt.Errorf("invalid fixed high-FPS engineering BOOT option")
	}
}
func loadUSBTrial() (uint32, error) {
	fd, e := syscall.Open(usbTrialPath, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(e) {
		return 0, nil
	} // Normal release never contains this file.
	if e != nil {
		return 0, e
	}
	f := os.NewFile(uintptr(fd), usbTrialPath)
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return 0, e
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.Mode().IsRegular() || st.Size() != 4 || st.Mode().Perm()&0222 != 0 || !ok || sys.Uid != 0 {
		return 0, fmt.Errorf("USB trial option must be a read-only root-owned ramdisk file")
	}
	var fs syscall.Statfs_t
	if e = syscall.Fstatfs(fd, &fs); e != nil {
		return 0, e
	}
	if fs.Type != 0x01021994 && fs.Type != 0x858458f6 {
		return 0, fmt.Errorf("USB trial option is not RAM-backed")
	}
	raw, e := io.ReadAll(io.LimitReader(f, 5))
	if e != nil {
		return 0, e
	}
	return parseUSBTrial(raw)
}

func (u *UI) applyUSBTrial() error {
	if u.usbTrialFPS == 0 {
		return nil
	}
	mode, e := cameramode.USBTrialMode(u.usbTrialFPS)
	if e != nil {
		return e
	}
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	u.state.USBTrialFPS = u.usbTrialFPS
	u.state.Camera.Settings = camera.DefaultSettings()
	u.state.Camera.Settings.Mode = mode
	u.state.Camera.Enabled = false // Arming descriptors below still does not bind USB.
	u.state.Camera.Status = "ENGINEERING USB TRIAL / HOME THEN HOST CAPTURE / 30 S LIMIT"
	u.state.Preview.Enabled = false
	u.state.Persistence = "ENGINEERING USB TRIAL / SETTINGS READ ONLY"
	return nil
}

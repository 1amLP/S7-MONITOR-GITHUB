//go:build linux && (amd64 || arm64)

package appliance

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"perimode/native/internal/linuxio"
)

// PanelBrightness changes only the supplied firmware's standard panel brightness
// node. No HBM, weakness_ccb, panel timing, charge or thermal controls are written.
type PanelBrightness interface {
	Read() (int, error)
	Set(int) error
}
type Panel struct {
	path  string
	limit int
}

func OpenPanel() (*Panel, error) {
	path := "/sys/class/backlight/panel"
	b, e := os.ReadFile(path + "/max_brightness")
	if e != nil {
		return nil, e
	}
	maximum, e := strconv.Atoi(strings.TrimSpace(string(b)))
	if e != nil || maximum < 1 || maximum > 65535 {
		return nil, fmt.Errorf("invalid panel brightness limit")
	}
	// Keep the conventional 8-bit range. Samsung extended HBM indices are excluded.
	p := &Panel{path: path, limit: min(255, maximum)}
	if _, e = p.Read(); e != nil {
		return nil, e
	}
	return p, nil
}
func (p *Panel) Read() (int, error) {
	b, e := os.ReadFile(p.path + "/brightness")
	if e != nil {
		return 0, e
	}
	raw, e := strconv.Atoi(strings.TrimSpace(string(b)))
	if e != nil || raw < 0 || raw > 65535 || p.limit <= 0 {
		return 0, fmt.Errorf("invalid panel brightness readback")
	}
	return min(100, (raw*100+p.limit/2)/p.limit), nil
}
func brightnessRaw(percent, limit int) (int, error) {
	if percent < 5 || percent > 100 || limit < 1 || limit > 255 {
		return 0, fmt.Errorf("brightness requires 5..100 percent of verified normal range")
	}
	return max(1, (percent*limit+50)/100), nil
}
func (p *Panel) Set(percent int) error {
	raw, e := brightnessRaw(percent, p.limit)
	if e != nil {
		return e
	}
	if e = linuxio.WriteAttr(p.path+"/brightness", strconv.Itoa(raw)+"\n"); e != nil {
		return e
	}
	b, e := os.ReadFile(p.path + "/brightness")
	if e != nil {
		return e
	}
	got, e := strconv.Atoi(strings.TrimSpace(string(b)))
	if e != nil || got != raw {
		return fmt.Errorf("panel brightness readback mismatch")
	}
	return nil
}
func (u *UI) openPanel() {
	u.panelMu.Lock()
	defer u.panelMu.Unlock()
	p, e := OpenPanel()
	if e != nil {
		u.state.mu.Lock()
		u.state.BrightnessStatus = "UNAVAILABLE: " + e.Error()
		u.state.mu.Unlock()
		return
	}
	u.panel = p
	u.state.mu.Lock()
	saved := u.state.Brightness
	u.state.mu.Unlock()
	var restoreErr error
	if saved != 0 {
		restoreErr = p.Set(saved)
		u.state.Error(restoreErr)
	}
	v, e := p.Read()
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	if e != nil {
		u.state.BrightnessStatus = "READ ERROR: " + e.Error()
		return
	}
	u.state.BrightnessActual = v
	u.state.Ambient.Fallback = max(5, v)
	if restoreErr != nil {
		u.state.BrightnessStatus = "RESTORE ERROR: " + restoreErr.Error()
	} else {
		u.state.BrightnessStatus = "NORMAL RANGE / NO HBM"
	}
}
func (u *UI) displayLines() []string {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	return []string{"DEVICE / DISPLAY", fmt.Sprintf("BRIGHTNESS: %d PCT", u.state.BrightnessActual), "DIMMER - 5 PCT / MANUAL", "BRIGHTER + 5 PCT / MANUAL", fmt.Sprintf("AUTOMATIC: %t / TAP TO CHANGE", u.state.Ambient.Settings.Automatic), "AUTO SETTINGS / LIGHT SENSOR", u.state.Ambient.Status, u.state.BrightnessStatus, "PANEL NORMAL RANGE / NO HBM", "BACK: DEVICE"}
}
func (u *UI) selectDisplay(row int) {
	if row == 4 {
		u.selectAutoBrightness(1)
		return
	}
	if row == 5 {
		u.menu("DISPLAY_AUTO")
		return
	}
	if row != 2 && row != 3 {
		return
	}
	u.async(func() error {
		u.panelMu.Lock()
		defer u.panelMu.Unlock()
		if u.panel == nil {
			return fmt.Errorf("panel brightness backend unavailable")
		}
		// Explicit manual intent invalidates old ALS writes even when the write fails.
		u.state.mu.Lock()
		u.state.Ambient.Settings.Automatic = false
		u.state.Ambient.Revision++
		u.state.Ambient.Status = "MANUAL / SENSOR OFF"
		u.state.mu.Unlock()
		before, e := u.panel.Read()
		if e != nil {
			return e
		}
		delta := 5
		if row == 2 {
			delta = -5
		}
		next := max(5, min(100, before+delta))
		if e = u.panel.Set(next); e != nil {
			u.state.mu.Lock()
			u.state.BrightnessStatus = "WRITE ERROR: " + e.Error()
			u.state.mu.Unlock()
			return e
		}
		actual, e := u.panel.Read()
		if e != nil {
			return e
		}
		u.state.mu.Lock()
		u.state.Brightness = next
		u.state.BrightnessActual = actual
		u.state.Ambient.Fallback = next
		u.state.BrightnessStatus = "MANUAL / SAVED IF CACHE ENABLED"
		u.state.mu.Unlock()
		return nil
	})
}

type MachineAction byte

const (
	ActionNone MachineAction = iota
	ActionPowerOff
	ActionReboot
	ActionRecovery
)

func (a MachineAction) String() string {
	switch a {
	case ActionPowerOff:
		return "POWER OFF"
	case ActionReboot:
		return "REBOOT"
	case ActionRecovery:
		return "RECOVERY"
	}
	return "NONE"
}

var ErrRebootRequested = errors.New("user confirmed reboot of whole device")
var ErrRecoveryRequested = errors.New("user confirmed reboot into Recovery")

func (u *UI) requestPower(a MachineAction) {
	if a != ActionPowerOff && a != ActionReboot && a != ActionRecovery {
		return
	}
	u.pendingAction = a
	u.powerConfirm = time.Now().Add(30 * time.Second)
	u.draw()
}
func (u *UI) confirmPower(now time.Time) {
	if u.pendingAction == ActionNone || !now.Before(u.powerConfirm) {
		u.state.Error(fmt.Errorf("power confirmation expired"))
		u.pendingAction = ActionNone
		u.powerConfirm = time.Time{}
		u.draw()
		return
	}
	u.shutdownAction = u.pendingAction
	u.pendingAction = ActionNone
	u.powerConfirm = time.Time{}
}
func RestartMachine() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("reboot refused outside PID1")
	}
	// State/cache workers have been stopped and closed by RunNative before this call.
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
}

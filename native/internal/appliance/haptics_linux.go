//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"perimode/native/internal/linuxio"
	"perimode/native/pkg/hid"
)

const defaultHapticPercent = 40
const hapticRoot = "/sys/class/timed_output/vibrator/"

type HapticRuntime struct {
	Available         bool `json:"driver_available"`
	Accepted, Dropped uint64
	LastError         string `json:"last_error,omitempty"`
}

func (s *State) HapticSnapshot() HapticRuntime { s.mu.Lock(); defer s.mu.Unlock(); return s.Haptics }

type hapticPacing struct{ last time.Time }

func (p *hapticPacing) accept(request, now time.Time, percent int) bool {
	if percent <= 0 || percent > 100 || now.Before(request) || now.Sub(request) > 60*time.Millisecond ||
		(!p.last.IsZero() && now.Sub(p.last) < 35*time.Millisecond) {
		return false
	}
	p.last = now
	return true
}
func hapticLabel(percent int) string {
	if percent == 0 {
		return "Disabled"
	}
	return fmt.Sprintf("%d%%", percent)
}
func (u *UI) feedback() {
	if u.hapticPulse != nil {
		u.hapticPulse()
	}
}
func (u *UI) contactPress(contacts []hid.Contact) bool {
	var mask uint32
	for _, c := range contacts {
		if c.ID < 32 {
			mask |= 1 << c.ID
		}
	}
	down := mask & ^u.hapticContacts != 0
	u.hapticContacts = mask
	return down
}
func feedbackTouchMode(page string, kind byte, view ScreenView) bool {
	return page == "" && (kind == hid.Touchscreen || view == ViewCamera || (view == ViewSniper && kind != 0))
}
func (u *UI) startHaptics() error {
	requests := make(chan time.Time, 1)
	u.hapticPulse = func() {
		select {
		case requests <- time.Now():
		default:
		}
	}
	return u.workers.Start("haptic-feedback", func(ctx context.Context) error {
		// The motor callback dereferences the probed SSP state in this kernel.
		for _, path := range []string{hapticRoot + "enable", hapticRoot + "intensity", "/sys/class/sensors/ssp_sensor"} {
			if _, err := os.Stat(path); err != nil {
				u.state.mu.Lock()
				u.state.Haptics.LastError = err.Error()
				u.state.mu.Unlock()
				return nil
			}
		}
		u.state.mu.Lock()
		u.state.Haptics.Available = true
		u.state.mu.Unlock()
		defer func() { _ = linuxio.WriteAttr(hapticRoot+"enable", "0\n") }()
		var pacing hapticPacing
		for {
			select {
			case <-ctx.Done():
				return nil
			case request := <-requests:
				u.state.mu.Lock()
				percent := u.state.HapticPercent
				u.state.mu.Unlock()
				if !pacing.accept(request, time.Now(), percent) {
					u.state.mu.Lock()
					u.state.Haptics.Dropped++
					u.state.mu.Unlock()
					continue
				}
				if ctx.Err() != nil {
					return nil
				}
				err := linuxio.WriteAttr(hapticRoot+"intensity", strconv.Itoa(percent*100)+"\n")
				if err == nil {
					err = linuxio.WriteAttr(hapticRoot+"enable", "18\n")
				}
				u.state.mu.Lock()
				if err != nil {
					u.state.Haptics.LastError = err.Error()
					u.state.Haptics.Available = false
				} else {
					u.state.Haptics.Accepted++
				}
				u.state.mu.Unlock()
				if err != nil {
					return nil
				}
			}
		}
	})
}

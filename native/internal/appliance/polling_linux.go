//go:build linux && (amd64 || arm64)

package appliance

import (
	"fmt"
	"perimode/native/pkg/hid"
	"time"
)

func validPadPolling(hz int) bool { return hz == 90 }

// FS interrupt bInterval is in milliseconds; HS uses 2^(bInterval-1)
// microframes of 125 us. This only sets the host's requested service interval.
func padUSBIntervals(hz int) (byte, byte, error) {
	switch hz {
	case 125:
		return 8, 7, nil
	case 500:
		return 2, 5, nil
	case 1000:
		return 1, 4, nil
	}
	return 0, 0, fmt.Errorf("touchpad polling must be 125, 500 or 1000 Hz")
}

func (t *touchUSB) pollingRate() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pollingHz
}

func (t *touchUSB) pollingInterval() time.Duration {
	hz := t.pollingRate()
	if !validPadPolling(hz) {
		hz = 90
	}
	return time.Second / time.Duration(hz)
}

func (t *touchUSB) setPollingRate(hz int) error {
	if !validPadPolling(hz) {
		return fmt.Errorf("touchpad report limit is fixed at 90 Hz")
	}
	t.mu.Lock()
	changed := t.pollingHz != hz
	t.pollingHz = hz
	t.mu.Unlock()
	if changed && t.pollingChanged != nil {
		select {
		case t.pollingChanged <- struct{}{}:
		default:
		}
	}
	return nil
}

func (u *UI) pollingLines() []string {
	u.state.mu.Lock()
	wanted := u.state.PadPollingHz
	persistent := u.state.Persistence
	u.state.mu.Unlock()
	active := 0
	if u.transport != nil && u.transport.touch != nil {
		active = u.transport.touch.pollingRate()
	}
	u.state.mu.Lock()
	sensor := u.state.TouchSensorRateStatus
	u.state.mu.Unlock()
	return []string{"TOUCH / TOUCHPAD REPORT RATE", fmt.Sprintf("SOFTWARE LIMIT: %d HZ / FIXED", wanted), fmt.Sprintf("ACTIVE LIMIT: %d HZ", active), "USB ENDPOINT: 1000 HZ / UNCHANGED", "SENSOR: " + sensor, "SENSOR REQUEST IS NOT A MEASUREMENT", "PERSISTENCE: " + persistent, "BACK: INPUT"}
}

// Only motion of the same live contact set may be replaced. Release, tip or
// confidence transitions, new IDs and input mode switches are never coalesced.
func sameContactSet(a, b hid.TouchFrame) bool {
	if a.Kind != b.Kind || a.Count != b.Count || a.Count == 0 || a.Count > hid.MaxContacts {
		return false
	}
	for i := 0; i < int(a.Count); i++ {
		found := false
		for j := 0; j < int(b.Count); j++ {
			if a.Contacts[i].ID == b.Contacts[j].ID && a.Contacts[i].Tip == b.Contacts[j].Tip && a.Contacts[i].Confidence == b.Contacts[j].Confidence {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (t *touchUSB) motionQueueCrowded() bool {
	if t.reports == nil || cap(t.reports) == 0 {
		return false
	}
	reserve := min(2, max(0, cap(t.reports)-1))
	return len(t.reports) >= cap(t.reports)-reserve
}

func (t *touchUSB) submitAt(f hid.TouchFrame, now time.Time) error {
	if f.Kind > hid.Touchpad || f.Count > hid.MaxContacts || (f.Kind == 0 && f.Count != 0) {
		return fmt.Errorf("invalid native touch frame")
	}
	// Validate before indexing controls or remembering a pending frame.
	if f.Kind != 0 {
		var data [hid.DigitizerReportBytes]byte
		if e := hid.DigitizerReport(f.Kind, f.ScanTime, f.Contacts[:f.Count], data[:]); e != nil {
			return e
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.timing.SubmittedFrames++
	pureMotion := t.enabled && !t.stopping && sameContactSet(t.lastTouch, f)
	rateLimited := t.pollingHz > 0 && f.Kind == hid.Touchpad && now.Sub(t.lastTouchAt) < time.Second/time.Duration(t.pollingHz)
	if pureMotion && (rateLimited || t.motionQueueCrowded()) {
		t.timing.DeferredFrames++
		if t.havePendingTouch {
			t.timing.CoalescedFrames++
		}
		t.pendingTouch = f
		t.havePendingTouch = true
		select {
		case t.motionChanged <- struct{}{}:
		default:
		}
		return nil
	}
	t.havePendingTouch = false
	e := t.submitLocked(f)
	if e == nil {
		t.lastTouch = f
		t.lastTouchAt = now
	}
	return e
}

// The deadline follows the last submitted frame, not the phase of an unrelated
// ticker. Queue space becomes available when the single writer drains a report.
func (t *touchUSB) motionDelay(now time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.havePendingTouch || !t.enabled || t.stopping || t.motionQueueCrowded() {
		return 0, false
	}
	if t.pendingTouch.Kind == hid.Touchpad && t.pollingHz > 0 {
		return max(0, t.lastTouchAt.Add(time.Second/time.Duration(t.pollingHz)).Sub(now)), true
	}
	return 0, true
}

func (t *touchUSB) flushMotion(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.havePendingTouch {
		return
	}
	if !t.enabled || t.stopping {
		t.havePendingTouch = false
		return
	}
	if t.motionQueueCrowded() {
		return
	}
	if t.pendingTouch.Kind == hid.Touchpad && t.pollingHz > 0 && now.Sub(t.lastTouchAt) < time.Second/time.Duration(t.pollingHz) {
		return
	}
	f := t.pendingTouch
	t.havePendingTouch = false
	if e := t.submitLocked(f); e != nil {
		t.reportFailure(e)
	} else {
		t.lastTouch = f
		t.lastTouchAt = now
	}
}

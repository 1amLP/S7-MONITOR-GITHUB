//go:build linux

package appliance

import (
	"errors"
	"time"

	"perimode/native/pkg/hid"
)

type sniperHIDState struct {
	tracker       hid.ContactTracker
	command       hid.SniperMessage
	ack           hid.SniperMessage
	pending, down bool
	lastSeen      time.Time
}

func (s *State) sniperHIDActive(generation uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Generation == generation && s.activeViewLocked() == ViewSniper && s.Menu == "" && s.TouchKind != 0
}

func (s *State) sniperHIDContact(m hid.SniperMessage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := &s.sniperInput
	return s.Generation == m.Generation && s.activeViewLocked() == ViewSniper && s.Menu == "" && s.TouchKind != 0 && len(q.queue) != 0 && q.queue[0].sequence == m.Sequence && (!m.Down || q.queue[0].down)
}

func (t *touchUSB) sniperStatus() [hid.SniperBytes]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	ack := t.sniper.ack
	ack.Kind = hid.SniperStatus
	if !t.enabled || t.stopping {
		ack.Status = hid.SniperDisabled
	}
	return ack.Marshal()
}

func (t *touchUSB) sniperCommand(raw []byte) error {
	m, err := hid.ParseSniperMessage(raw)
	if err != nil || m.Kind == hid.SniperStatus {
		return errors.New("invalid Sniper HID request")
	}
	allowed, active := false, false
	if t.sniperState != nil {
		allowed = t.sniperState.sniperHIDContact(m)
		active = t.sniperState.sniperHIDActive(m.Generation)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if !t.enabled || t.stopping {
		t.sniper.ack = m
		t.sniper.ack.Status = hid.SniperDisabled
		return nil
	}
	if m.Kind == hid.SniperCancel {
		if m.Generation == t.sniper.command.Generation {
			return t.releaseSniperLocked(now)
		}
		return nil
	}
	if m == t.sniper.command && (t.sniper.pending || t.sniper.ack.Status == hid.SniperOK) {
		if active {
			t.sniper.lastSeen = now
		}
		return nil
	}
	if t.sniper.pending {
		return nil
	}
	if !allowed {
		t.sniper.ack = m
		t.sniper.ack.Status = hid.SniperStale
		return nil
	}
	return t.queueSniperLocked(m, now)
}

func (t *touchUSB) queueSniperLocked(m hid.SniperMessage, now time.Time) error {
	tracker := t.sniper.tracker
	var contacts []hid.Contact
	if m.Down {
		contacts = []hid.Contact{{ID: 0, X: m.X, Y: m.Y, Tip: true, Confidence: true}}
	}
	reports, count, err := tracker.Reports(hid.Touchscreen, uint16(now.UnixMicro()/100), contacts)
	if err != nil {
		return err
	}
	if t.reports == nil || cap(t.reports)-len(t.reports) < count {
		t.sniper.ack = m
		t.sniper.ack.Status = hid.SniperIOError
		return errors.New("Sniper HID output queue full")
	}
	t.sniper.command, t.sniper.ack = m, m
	t.sniper.ack.Kind, t.sniper.ack.Status = hid.SniperStatus, hid.SniperPending
	t.sniper.tracker, t.sniper.pending, t.sniper.lastSeen = tracker, true, now
	for i := 0; i < count; i++ {
		r := touchReport{index: 0, data: reports[i], generation: t.generation, queuedAt: now, sniper: m, sniperFinal: i == count-1}
		r.data[0] = hid.SniperInputID
		t.reports <- r
		t.timing.QueuedReports++
		t.timing.QueuePeak = max(t.timing.QueuePeak, len(t.reports))
	}
	return nil
}

func (t *touchUSB) sniperReportAllowed(r touchReport) bool {
	if r.sniper.Kind == hid.SniperCancel {
		return true
	}
	return t.sniperState != nil && t.sniperState.sniperHIDActive(r.sniper.Generation)
}

// Called under the HID writer lock, only after the interrupt write completes.
func (t *touchUSB) sniperReportDone(r touchReport, err error) {
	if r.data[0] != hid.SniperInputID || r.sniper != t.sniper.command {
		return
	}
	if err != nil {
		t.sniper.ack.Status = hid.SniperIOError
		t.sniper.pending = false
		return
	}
	if r.sniperFinal && t.sniper.ack.Status == hid.SniperPending {
		t.sniper.ack.Status = hid.SniperOK
		t.sniper.pending, t.sniper.down = false, r.sniper.Down
	}
}

func (t *touchUSB) releaseSniperLocked(now time.Time) error {
	if !t.sniper.down && !t.sniper.pending {
		return nil
	}
	m := hid.SniperMessage{Kind: hid.SniperCancel, Generation: t.sniper.command.Generation}
	return t.queueSniperLocked(m, now)
}

func (t *touchUSB) checkSniperRelease(now time.Time) {
	t.mu.Lock()
	generation, held := t.sniper.command.Generation, t.sniper.down || t.sniper.pending
	t.mu.Unlock()
	if !held {
		return
	}
	active := t.sniperState != nil && t.sniperState.sniperHIDActive(generation)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sniper.command.Kind == hid.SniperCancel {
		return
	}
	if !active || now.Sub(t.sniper.lastSeen) > 500*time.Millisecond {
		if err := t.releaseSniperLocked(now); err != nil {
			t.reportFailure(err)
		}
	}
}

//go:build linux && (amd64 || arm64)

package appliance

import (
	"errors"
	"fmt"
	"perimode/native/pkg/hid"
	"time"
)

// Delivered counts completed USB press+release pairs, NOT a measured PC level.
type PCVolumeStatus struct {
	Online    bool   `json:"online"`
	Delivered uint64 `json:"delivered_pulses"`
	Errors    uint64 `json:"write_errors"`
	LastError string `json:"last_error,omitempty"`
}

func (t *touchUSB) volumeInput(e VolumeInput) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if e.Lost {
		t.volume.LoseSync()
		return
	}
	if e.AllUp {
		t.volume.AllUp()
		return
	}
	t.volume.Key(e.Code, e.Value, time.Now())
}
func (t *touchUSB) pcVolumeStatus() PCVolumeStatus {
	if t == nil {
		return PCVolumeStatus{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.volumeStatus
	s.Online = t.enabled && !t.stopping && !t.volumeSuspended && !t.volumeFault
	return s
}

// Called under t.mu. Menu/touch mode changes do NOT call this: keys work in menus
// and with both touch functions and USB audio disabled.
func (t *touchUSB) volumeConnection(online bool) {
	t.volumeGeneration++
	t.volume.SetOnline(online)
	t.volumeNeutral = true
	t.volumeCurrent = 0
	t.volumeRetry = time.Time{}
	if online {
		t.volumeFault = false
	}
}
func (t *touchUSB) blockVolume() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.volumeConnection(false)
	// Home unbind must reject a pending tick before the DISABLE event is read.
	t.volumeSuspended = true
}

// One writer owns BOTH reports, so contact traffic cannot separate the pulse.
// Failed/stale writes never replay a volume press on reconnect. Neutral is sent
// before new input and retried after errors; no buffer/descriptor is reused here.
func (t *touchUSB) writeVolume(now time.Time, write func(int, []byte) error) {
	t.mu.Lock()
	if !t.enabled || t.stopping || t.volumeSuspended || now.Before(t.volumeRetry) {
		t.mu.Unlock()
		return
	}
	gen := t.volumeGeneration
	neutralOnly := t.volumeNeutral
	mask := byte(0)
	if !neutralOnly {
		mask = t.volume.Take(now)
	}
	if !neutralOnly && mask == 0 {
		t.mu.Unlock()
		return
	}
	// Even an error/short transfer can mean that the host received the press.
	t.volumeNeutral = true
	t.mu.Unlock()
	current := func() bool {
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.enabled && !t.stopping && !t.volumeSuspended && gen == t.volumeGeneration
	}
	setCurrent := func(v byte) {
		t.mu.Lock()
		defer t.mu.Unlock()
		if gen == t.volumeGeneration {
			t.volumeCurrent = v
		}
	}
	var err error
	if !neutralOnly && current() {
		p := hid.VolumeReport(mask)
		setCurrent(mask)
		err = write(0, p[:])
	}
	// Attempt release even if the press failed. If the session changed, the next
	// session owns its neutral barrier, and this completion changes nothing.
	if current() {
		p := hid.VolumeReport(0)
		setCurrent(0)
		err = errors.Join(err, write(0, p[:]))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.volumeGeneration || !t.enabled || t.stopping || t.volumeSuspended {
		return
	}
	if err != nil {
		t.volumeFault = true
		t.volume.SetOnline(false)
		t.volumeRetry = now.Add(100 * time.Millisecond)
		t.volumeStatus.Errors++
		t.volumeStatus.LastError = fmt.Sprintf("PC volume HID: %v", err)
		return
	}
	t.volumeNeutral = false
	t.volumeCurrent = 0
	t.volumeRetry = time.Time{}
	if t.volumeFault {
		t.volume.SetOnline(true)
	}
	t.volumeFault = false
	t.volumeStatus.LastError = ""
	if !neutralOnly {
		t.volumeStatus.Delivered++
	}
}

func (u *UI) pcVolumeLines() []string {
	var st PCVolumeStatus
	if u.transport != nil {
		st = u.transport.touch.pcVolumeStatus()
	}
	return []string{"DEVICE / PC VOLUME KEYS",
		"VOLUME UP / DOWN: PC AUDIO, NOT S7 GAIN",
		"TAP: ONE STEP / HOLD: REPEAT AFTER 450 MS",
		fmt.Sprintf("USB HID ONLINE: %t", st.Online),
		fmt.Sprintf("DELIVERED PRESS / RELEASE PAIRS: %d", st.Delivered),
		"OUTPUT AND STEP SIZE ARE CHOSEN BY WINDOWS",
		"PC VOLUME PERCENT IS NOT REPORTED BY HID",
		"S7 SPEAKER LEVEL: AUDIO MENU / SPEAKER VOLUME",
		"OFFLINE KEYS ARE NOT REPLAYED ON CONNECT",
		"ERROR: " + st.LastError, "BACK: DEVICE"}
}

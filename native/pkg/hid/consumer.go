package hid

import "time"

// ConsumerVolumeReportID is deliberately separate from digitizer (1..6) and
// camera feature (7,8) reports. It uses the EXISTING touchscreen IN endpoint.
const (
	ConsumerVolumeReportID byte = 9
	VolumeUpMask           byte = 1
	VolumeDownMask         byte = 2
	VolumeRepeatDelay           = 450 * time.Millisecond
	VolumeRepeatInterval        = 100 * time.Millisecond
	VolumeMaximumHold           = 15 * time.Second
)

// ConsumerVolumeDescriptor is a separate top-level collection, not a keyboard.
// USB HID Usage Tables: Consumer page 0x0c, E9 increment / EA decrement.
// Return fresh storage so assembling a composite descriptor cannot mutate it.
func ConsumerVolumeDescriptor() []byte {
	return []byte{
		0x05, 0x0c, 0x09, 0x01, 0xa1, 0x01, 0x85, ConsumerVolumeReportID,
		0x15, 0, 0x25, 1, 0x35, 0, 0x45, 1, 0x55, 0, 0x65, 0,
		0x09, 0xe9, 0x09, 0xea, 0x75, 1, 0x95, 2, 0x81, 2,
		0x75, 6, 0x95, 1, 0x81, 3, 0xc0,
	}
}

func VolumeReport(mask byte) [2]byte {
	// A chord is not "mute" and never sends two contradictory usages.
	if mask != VolumeUpMask && mask != VolumeDownMask {
		mask = 0
	}
	return [2]byte{ConsumerVolumeReportID, mask}
}

// VolumeButtons produces finite press/release pulses. Caller serializes access.
// It never queues history or synthesizes catch-up repeats after a slow USB host.
// Linux auto-repeat events are ignored: one clock owns the repetition policy.
type VolumeButtons struct {
	online, synchronized, blocked bool
	held, pending                 byte
	downAt, next                  time.Time
}

func volumeMask(code uint16) byte {
	switch code {
	case 115:
		return VolumeUpMask
	case 114:
		return VolumeDownMask
	}
	return 0
}
func (v *VolumeButtons) SetOnline(online bool) {
	if v.online == online {
		return
	}
	v.online = online
	v.pending = 0
	v.next = time.Time{}
	if v.held != 0 {
		v.blocked = true
	}
}
func (v *VolumeButtons) LoseSync() {
	v.synchronized = false
	v.blocked = true
	v.pending = 0
	v.next = time.Time{}
}

// AllUp may be called only after a physical EVIOCGKEY all-up observation.
func (v *VolumeButtons) AllUp() {
	v.synchronized, v.blocked = true, false
	v.held, v.pending = 0, 0
	v.next, v.downAt = time.Time{}, time.Time{}
}
func (v *VolumeButtons) Key(code uint16, value int32, now time.Time) {
	mask := volumeMask(code)
	if mask == 0 || value == 2 || value < 0 || value > 2 {
		return
	}
	if value == 0 {
		v.held &^= mask
		// Keep a short click pending even if key-up precedes the writer's tick.
		v.next = time.Time{}
		if v.held == 0 && v.synchronized {
			v.blocked = false
		}
		return
	}
	if v.held&mask != 0 {
		return
	}
	v.held |= mask
	if v.held == 3 || !v.online || !v.synchronized || v.blocked {
		v.blocked = true
		v.pending = 0
		v.next = time.Time{}
		return
	}
	v.pending = mask
	v.downAt = now
	v.next = now.Add(VolumeRepeatDelay)
}
func (v *VolumeButtons) Take(now time.Time) byte {
	if !v.online || !v.synchronized || v.blocked {
		return 0
	}
	if v.held != 0 && !v.downAt.IsZero() && now.Sub(v.downAt) >= VolumeMaximumHold {
		v.blocked = true
		v.pending = 0
		return 0
	}
	if v.pending != 0 {
		if now.Sub(v.downAt) > 150*time.Millisecond {
			v.pending = 0
			if v.held != 0 {
				v.next = now.Add(VolumeRepeatInterval)
			}
			return 0
		}
		p := v.pending
		v.pending = 0
		return p
	}
	if v.held != 0 && !v.next.IsZero() && !now.Before(v.next) {
		v.next = now.Add(VolumeRepeatInterval)
		return v.held
	}
	return 0
}

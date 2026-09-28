package hid

import (
	"encoding/binary"
	"errors"
)

const (
	SniperFeatureID = 12
	SniperInputID   = 13
	SniperMaximumID = 14
	SniperBytes     = 33
	SniperStatus    = 0
	SniperPoint     = 1
	SniperCancel    = 2
	SniperOK        = 0
	SniperPending   = 1
	SniperDisabled  = 2
	SniperStale     = 3
	SniperIOError   = 4
)

// Appended after the existing five touchscreen-interface collections. The
// independent Windows touchscreen PDO can be mapped without changing Monitor.
func SniperDescriptor() []byte {
	d := []byte{0x05, 0x0d, 0x09, 4, 0xa1, 1, 0x85, SniperInputID,
		0x55, 0x0c, 0x66, 0x01, 0x10, 0x35, 0, 0x47, 0xff, 0xff, 0, 0,
		0x09, 0x56, 0x15, 0, 0x27, 0xff, 0xff, 0, 0, 0x75, 16, 0x95, 1, 0x81, 2,
		0x55, 0, 0x65, 0, 0x45, 0, 0x09, 0x54, 0x25, MaxContacts, 0x75, 8, 0x81, 2}
	for i := 0; i < MaxContacts; i++ {
		d = append(d, 0x09, 0x22, 0xa1, 2,
			0x09, 0x42, 0x09, 0x47, 0x15, 0, 0x25, 1, 0x75, 1, 0x95, 2, 0x81, 2,
			0x75, 6, 0x95, 1, 0x81, 3, 0x09, 0x51, 0x25, 31, 0x75, 8, 0x95, 1, 0x81, 2,
			0x05, 1, 0x09, 0x30, 0x26, 0xff, 0x7f, 0x35, 0, 0x46, 0x67, 4,
			0x55, 0x0e, 0x65, 0x11, 0x75, 16, 0x81, 2,
			0x09, 0x31, 0x46, 0x7a, 2, 0x81, 2,
			0x55, 0, 0x65, 0, 0x35, 0, 0x45, 0, 0x05, 0x0d, 0xc0)
	}
	d = append(d, 0x85, SniperMaximumID, 0x09, 0x55, 0x15, 0, 0x25, MaxContacts, 0x75, 8, 0x95, 1, 0xb1, 2, 0xc0)
	return append(d, 0x06, 0x53, 0xff, 0x09, 4, 0xa1, 1, 0x85, SniperFeatureID,
		0x15, 0, 0x26, 0xff, 0, 0x75, 8, 0x95, SniperBytes-1, 0x09, 4, 0xb1, 2, 0xc0)
}

type SniperMessage struct {
	Kind, Status byte
	Generation   uint32
	Sequence     uint16
	Down         bool
	X, Y         uint16
}

func (m SniperMessage) Marshal() [SniperBytes]byte {
	var b [SniperBytes]byte
	b[0], b[5], b[6], b[7] = SniperFeatureID, 1, m.Kind, m.Status
	copy(b[1:], "S7H1")
	binary.LittleEndian.PutUint32(b[8:], m.Generation)
	binary.LittleEndian.PutUint16(b[12:], m.Sequence)
	if m.Down {
		b[14] = 1
	}
	binary.LittleEndian.PutUint16(b[16:], m.X)
	binary.LittleEndian.PutUint16(b[18:], m.Y)
	return b
}

func ParseSniperMessage(b []byte) (SniperMessage, error) {
	bad := errors.New("invalid Sniper HID command")
	if len(b) != SniperBytes || b[0] != SniperFeatureID || string(b[1:5]) != "S7H1" || b[5] != 1 || b[6] > SniperCancel || b[7] > SniperIOError || b[14] > 1 || b[15] != 0 {
		return SniperMessage{}, bad
	}
	for _, v := range b[20:] {
		if v != 0 {
			return SniperMessage{}, bad
		}
	}
	m := SniperMessage{Kind: b[6], Status: b[7], Generation: binary.LittleEndian.Uint32(b[8:]), Sequence: binary.LittleEndian.Uint16(b[12:]), Down: b[14] != 0, X: binary.LittleEndian.Uint16(b[16:]), Y: binary.LittleEndian.Uint16(b[18:])}
	if m.X > 32767 || m.Y > 32767 || (m.Kind != SniperStatus && (m.Generation == 0 || m.Status != 0)) || (m.Kind == SniperPoint && m.Sequence == 0) || (m.Kind == SniperCancel && (m.Down || m.X != 0 || m.Y != 0)) {
		return SniperMessage{}, bad
	}
	return m, nil
}

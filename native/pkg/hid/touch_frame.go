package hid

import (
	"encoding/binary"
	"errors"
	"io"
)

const TouchFrameHeader = 12

type TouchFrame struct {
	Sequence uint32
	ScanTime uint16
	Kind     byte
	Count    byte
	Contacts [MaxContacts]Contact
}

// Authenticated local transport. Kind zero is an explicit release, not input.
func ReadTouchFrame(reader io.Reader, previous uint32) (TouchFrame, error) {
	var f TouchFrame
	var head [TouchFrameHeader]byte
	if _, err := io.ReadFull(reader, head[:]); err != nil {
		return f, err
	}
	if string(head[:4]) != "S7T1" {
		return f, errors.New("invalid touch magic")
	}
	f.Sequence = binary.LittleEndian.Uint32(head[4:8])
	f.ScanTime = binary.LittleEndian.Uint16(head[8:10])
	f.Kind = head[10]
	f.Count = head[11]
	sequenceDelta := f.Sequence - previous
	if sequenceDelta == 0 || sequenceDelta >= 1<<31 || f.Kind > Touchpad || f.Count > MaxContacts || f.Kind == 0 && f.Count != 0 {
		return f, errors.New("invalid touch frame")
	}
	var data [MaxContacts * 6]byte
	if _, err := io.ReadFull(reader, data[:int(f.Count)*6]); err != nil {
		return f, err
	}
	var seen uint32
	for i := 0; i < int(f.Count); i++ {
		p := data[i*6:]
		c := Contact{ID: p[1], X: binary.LittleEndian.Uint16(p[2:4]), Y: binary.LittleEndian.Uint16(p[4:6]), Tip: p[0]&1 != 0, Confidence: p[0]&2 != 0}
		if p[0] != 3 || c.ID > 31 || c.X > 32767 || c.Y > 32767 || seen&(1<<c.ID) != 0 {
			return f, errors.New("invalid live contact")
		}
		seen |= 1 << c.ID
		f.Contacts[i] = c
	}
	return f, nil
}

// Preserve liftoff IDs. Replacing five contacts needs two bounded reports.
type ContactTracker struct {
	previous [MaxContacts]Contact
	count    int
}

func (t *ContactTracker) Reports(kind byte, scan uint16, current []Contact) ([2][DigitizerReportBytes]byte, int, error) {
	var out [2][DigitizerReportBytes]byte
	if err := DigitizerReport(kind, scan, current, out[1][:]); err != nil {
		return out, 0, err
	}
	var transition [MaxContacts]Contact
	var oldIDs uint32
	changed := false
	for i := 0; i < t.count; i++ {
		old := t.previous[i]
		oldIDs |= 1 << old.ID
		transition[i] = old
		transition[i].Tip = false
		for _, c := range current {
			if c.ID == old.ID {
				transition[i] = c
				break
			}
		}
		if !transition[i].Tip {
			changed = true
		}
	}
	if changed {
		if err := DigitizerReport(kind, scan, transition[:t.count], out[0][:]); err != nil {
			return out, 0, err
		}
		newContact := false
		for _, c := range current {
			if oldIDs&(1<<c.ID) == 0 {
				newContact = true
			}
		}
		copy(t.previous[:], current)
		t.count = len(current)
		if newContact {
			return out, 2, nil
		}
		return out, 1, nil
	}
	out[0] = out[1]
	copy(t.previous[:], current)
	t.count = len(current)
	return out, 1, nil
}

func (t *ContactTracker) Reset() { clear(t.previous[:]); t.count = 0 }

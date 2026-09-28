package hid

import (
	"encoding/binary"
	"errors"
	"perimode/native/pkg/cameractl"
)

const (
	Touchscreen          = 1
	Touchpad             = 2
	MaxContacts          = 5
	DigitizerReportBytes = 4 + MaxContacts*6
)

// Windows consumes standard HID contacts and performs gesture recognition.
// Physical units are 0.01 cm, matching the S7 panel in landscape orientation.
func DigitizerDescriptor(kind byte) ([]byte, error) {
	if kind != Touchscreen && kind != Touchpad {
		return nil, errors.New("invalid digitizer kind")
	}
	if kind == Touchpad {
		return precisionDescriptor(), nil
	}
	usage := byte(4)
	if kind == Touchpad {
		usage = 5
	}
	d := []byte{0x05, 0x0d, 0x09, usage, 0xa1, 1, 0x85, kind,
		0x55, 0x0c, 0x66, 0x01, 0x10,
		0x35, 0, 0x47, 0xff, 0xff, 0, 0,
		0x09, 0x56, 0x15, 0, 0x27, 0xff, 0xff, 0, 0, 0x75, 16, 0x95, 1, 0x81, 2,
		0x55, 0, 0x65, 0, 0x45, 0,
		0x09, 0x54, 0x25, MaxContacts, 0x75, 8, 0x81, 2}
	for i := 0; i < MaxContacts; i++ {
		d = append(d, 0x09, 0x22, 0xa1, 2,
			0x09, 0x42, 0x09, 0x47, 0x15, 0, 0x25, 1, 0x75, 1, 0x95, 2, 0x81, 2,
			0x75, 6, 0x95, 1, 0x81, 3,
			0x09, 0x51, 0x25, 31, 0x75, 8, 0x81, 2,
			0x05, 1, 0x09, 0x30, 0x26, 0xff, 0x7f, 0x35, 0, 0x46, 0x67, 4,
			0x55, 0x0e, 0x65, 0x11, 0x75, 16, 0x81, 2,
			0x09, 0x31, 0x46, 0x7a, 2, 0x81, 2,
			0x55, 0, 0x65, 0, 0x35, 0, 0x45, 0, 0x05, 0x0d, 0xc0)
	}
	d = append(d, 0x85, 3, 0x09, 0x55, 0x15, 0, 0x25, MaxContacts, 0x75, 8, 0x95, 1, 0xb1, 2)
	if kind == Touchpad {
		d = append(d, 0x09, 0x59, 0x25, 2, 0xb1, 2,
			0x85, 4, 0x06, 0, 0xff, 0x09, 0xc5, 0x15, 0, 0x26, 0xff, 0, 0x75, 8, 0x96, 0, 1, 0xb1, 2)
	}
	d = append(d, 0xc0)
	if kind == Touchpad {
		d = append(d, 0x05, 0x0d, 0x09, 0x0e, 0xa1, 1,
			0x85, 5, 0x09, 0x22, 0xa1, 2,
			0x09, 0x52, 0x15, 0, 0x25, 10, 0x75, 8, 0x95, 1, 0xb1, 2, 0xc0,
			0x09, 0x22, 0xa1, 0,
			0x85, 6, 0x09, 0x57, 0x09, 0x58, 0x25, 1, 0x75, 1, 0x95, 2, 0xb1, 2,
			0x75, 6, 0x95, 1, 0xb1, 3, 0xc0, 0xc0)
	}
	d = append(d, 0x06, 0x53, 0xff, 0x09, 1, 0xa1, 1, 0x85, 7, 0x15, 0, 0x26, 0xff, 0, 0x75, 8, 0x95, 16, 0x09, 1, 0xb1, 2, 0xc0)
	// Separate top-level collection: legacy S7W1 remains 17 bytes (Usage 1).
	// This 65-byte feature (Usage 2) does not alter touch/PTP report layouts.
	if kind == Touchscreen {
		d = append(d, 0x06, 0x53, 0xff, 0x09, 2, 0xa1, 1, 0x85, 8, 0x15, 0, 0x26, 0xff, 0, 0x75, 8, 0x95, 64, 0x09, 2, 0xb1, 2,
			0x85, 11, 0x09, 3, 0x95, 64, 0xb1, 2, 0xc0)
	}
	return d, nil
}

type Contact struct {
	ID              byte
	X, Y            uint16
	Tip, Confidence bool
}

func DigitizerReport(kind byte, scanTime uint16, contacts []Contact, out []byte) error {
	if (kind != Touchscreen && kind != Touchpad) || len(contacts) > MaxContacts || len(out) != DigitizerReportBytes {
		return errors.New("invalid touch report geometry")
	}
	var seen uint32
	for _, c := range contacts {
		if c.ID > 31 || c.X > 32767 || c.Y > 32767 || seen&(1<<c.ID) != 0 {
			return errors.New("invalid touch contact")
		}
		seen |= 1 << c.ID
	}
	clear(out)
	out[0] = kind
	if kind == Touchpad {
		for i, c := range contacts {
			p := out[1+i*5:]
			p[0] = c.ID << 2
			if c.Confidence {
				p[0] |= 1
			}
			if c.Tip {
				p[0] |= 2
			}
			binary.LittleEndian.PutUint16(p[1:3], c.X)
			binary.LittleEndian.PutUint16(p[3:5], c.Y)
		}
		binary.LittleEndian.PutUint16(out[26:28], scanTime)
		out[28] = byte(len(contacts))
		return nil
	}
	binary.LittleEndian.PutUint16(out[1:3], scanTime)
	out[3] = byte(len(contacts))
	for i, c := range contacts {
		p := out[4+i*6:]
		if c.Tip {
			p[0] |= 1
		}
		if c.Confidence {
			p[0] |= 2
		}
		p[1] = c.ID
		binary.LittleEndian.PutUint16(p[2:4], c.X)
		binary.LittleEndian.PutUint16(p[4:6], c.Y)
	}
	return nil
}

type DigitizerControl struct{ Kind, InputMode, Selective byte }

func NewDigitizerControl(kind byte) (*DigitizerControl, error) {
	if kind != Touchscreen && kind != Touchpad {
		return nil, errors.New("invalid digitizer kind")
	}
	c := &DigitizerControl{Kind: kind}
	c.Reset()
	return c, nil
}

func (c *DigitizerControl) Reset() {
	c.InputMode = 0
	c.Selective = 3
}
func (c *DigitizerControl) ContactsEnabled() bool {
	return c.Kind == Touchscreen || c.InputMode == 3 && c.Selective&1 != 0
}

func (c *DigitizerControl) Feature(id byte) ([]byte, error) {
	switch id {
	case 3:
		if c.Kind == Touchpad {
			return []byte{3, 0x20 | MaxContacts}, nil
		}
		return []byte{3, MaxContacts}, nil
	case 4:
		if c.Kind == Touchpad {
			// Explicitly uncertified. Windows 10+ accepts a 256-byte status blob.
			data := make([]byte, 257)
			data[0] = 4
			return data, nil
		}
	case 5:
		if c.Kind == Touchpad {
			return []byte{5, c.InputMode}, nil
		}
	case 6:
		if c.Kind == Touchpad {
			return []byte{6, c.Selective}, nil
		}
	case cameractl.ReportID:
		if c.Kind == Touchscreen {
			b := (cameractl.Message{}).Marshal()
			return b[:], nil
		}
	case 7:
		if c.Kind == Touchscreen {
			data := make([]byte, 17)
			data[0] = 7
			copy(data[1:], "S7W1")
			return data, nil
		}
	}
	return nil, errors.New("unsupported HID feature")
}

func (c *DigitizerControl) SetFeature(id byte, data []byte) error {
	if c.Kind != Touchpad || len(data) != 2 || data[0] != id {
		return errors.New("invalid HID feature")
	}
	switch id {
	case 5:
		// Microsoft requires unknown modes to fall back to mouse mode (0).
		// This appliance has no emulated mouse collection, so reporting stops.
		c.InputMode = 0
		if data[1] == 3 {
			c.InputMode = 3
		}
	case 6:
		if data[1]&^3 != 0 {
			return errors.New("invalid selective reporting")
		}
		c.Selective = data[1]
	default:
		return errors.New("read-only or unknown HID feature")
	}
	return nil
}

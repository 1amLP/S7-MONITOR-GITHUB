// Package hid provides HID (Human Interface Device) descriptors and report structures
// for keyboard and mouse emulation.
package hid


type KeyboardReport struct {
	Modifiers byte
	Reserved  byte
	Keys      [6]byte
}

var KeyboardDescriptor = []byte{
	0x05, 0x01, // Usage Page (Generic Desktop Ctrls)
	0x09, 0x06, // Usage (Keyboard)
	0xA1, 0x01, // Collection (Application)
	0x05, 0x07, //   Usage Page (Kbrd/Keypad)
	0x19, 0xE0, //   Usage Minimum (0xE0)
	0x29, 0xE7, //   Usage Maximum (0xE7)
	0x15, 0x00, //   Logical Minimum (0)
	0x25, 0x01, //   Logical Maximum (1)
	0x75, 0x01, //   Report Size (1)
	0x95, 0x08, //   Report Count (8)
	0x81, 0x02, //   Input (Data,Var,Abs,No Wrap,Linear,Preferred State,No Null Position)
	0x95, 0x01, //   Report Count (1)
	0x75, 0x08, //   Report Size (8)
	0x81, 0x03, //   Input (Const,Var,Abs,No Wrap,Linear,Preferred State,No Null Position)
	0x95, 0x05, //   Report Count (5)
	0x75, 0x01, //   Report Size (1)
	0x05, 0x08, //   Usage Page (LEDs)
	0x19, 0x01, //   Usage Minimum (Num Lock)
	0x29, 0x05, //   Usage Maximum (Kana)
	0x91, 0x02, //   Output (Data,Var,Abs,No Wrap,Linear,Preferred State,No Null Position,Non-volatile)
	0x95, 0x01, //   Report Count (1)
	0x75, 0x03, //   Report Size (3)
	0x91, 0x03, //   Output (Const,Var,Abs,No Wrap,Linear,Preferred State,No Null Position,Non-volatile)
	0x95, 0x06, //   Report Count (6)
	0x75, 0x08, //   Report Size (8)
	0x15, 0x00, //   Logical Minimum (0)
	0x25, 0x65, //   Logical Maximum (101)
	0x05, 0x07, //   Usage Page (Kbrd/Keypad)
	0x19, 0x00, //   Usage Minimum (0x00)
	0x29, 0x65, //   Usage Maximum (0x65)
	0x81, 0x00, //   Input (Data,Array,Abs,No Wrap,Linear,Preferred State,No Null Position)
	0x06, 0x00, 0xFF, // Usage Page (Vendor Defined 0xFF00)
	0x09, 0x01, // Usage (0x01)
	0x75, 0x08, // Report Size (8)
	0x95, 0x01, // Report Count (1)
	0x91, 0x02, // Output (Data,Var,Abs)
	0xC0,       // End Collection
}

func (r *KeyboardReport) Bytes() []byte {
	return []byte{
		r.Modifiers,
		r.Reserved,
		r.Keys[0], r.Keys[1], r.Keys[2], r.Keys[3], r.Keys[4], r.Keys[5],
	}
}

func (r *KeyboardReport) AddKey(key byte) bool {
	for i := range r.Keys {
		if r.Keys[i] == key {
			return true
		}
		if r.Keys[i] == 0 {
			r.Keys[i] = key
			return true
		}
	}
	return false
}

func (r *KeyboardReport) RemoveKey(key byte) {
	for i := range r.Keys {
		if r.Keys[i] == key {
			for j := i; j < len(r.Keys)-1; j++ {
				r.Keys[j] = r.Keys[j+1]
			}
			r.Keys[len(r.Keys)-1] = 0
			break
		}
	}
}

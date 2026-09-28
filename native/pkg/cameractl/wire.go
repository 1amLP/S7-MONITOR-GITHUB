// Package cameractl defines the bounded, versioned S7 camera-selection HID ABI.
// It has no device side effects. All integers are little endian, never Go/C structs.
package cameractl

import (
	"encoding/binary"
	"errors"
	"perimode/native/pkg/cameramode"
)

const ReportID byte = 8
const ReportBytes = 65
const Version byte = 2
const (
	Status byte = iota
	Acquire
	Release
	Keepalive
)
const (
	OK byte = iota
	Disabled
	Unavailable
	Busy
	Stale
	Invalid
	Thermal
	Unsafe
)
const (
	Enabled uint16 = 1 << iota
	USBReady
	Streaming
	Paused
	OwnershipFault
)

type Mode = cameramode.Mode

func Modes(sensor byte) []Mode { return cameramode.Modes(cameramode.Sensor(sensor)) }
func ModeBit(sensor byte, m Mode) byte {
	for i, v := range Modes(sensor) {
		if m == v {
			return 1 << i
		}
	}
	return 0
}
func Allowed(sensor byte, m Mode, masks [2]byte) bool {
	if sensor > 1 || !m.WebcamEligible() {
		return false
	}
	b := ModeBit(sensor, m)
	return b != 0 && masks[sensor]&b != 0
}

type Message struct {
	Kind, Sensor, Result byte
	Sequence             uint32
	Token, Generation    uint64
	Mode                 Mode
	Masks                [2]byte
	Flags                uint16
	Owner                uint64
}

func (m Message) Marshal() [ReportBytes]byte {
	var b [ReportBytes]byte
	b[0] = ReportID
	copy(b[1:5], "S7S2")
	b[5] = Version
	b[6] = m.Kind
	b[7] = m.Sensor
	b[8] = m.Result
	binary.LittleEndian.PutUint32(b[9:13], m.Sequence)
	binary.LittleEndian.PutUint64(b[13:21], m.Token)
	binary.LittleEndian.PutUint64(b[21:29], m.Generation)
	binary.LittleEndian.PutUint32(b[29:33], m.Mode.Width)
	binary.LittleEndian.PutUint32(b[33:37], m.Mode.Height)
	binary.LittleEndian.PutUint32(b[37:41], m.Mode.FPS)
	b[41] = m.Masks[0]
	b[42] = m.Masks[1]
	binary.LittleEndian.PutUint16(b[43:45], m.Flags)
	binary.LittleEndian.PutUint64(b[45:53], m.Owner)
	return b
}
func Parse(b []byte) (Message, error) {
	var m Message
	bad := errors.New("invalid S7S2 camera control report")
	if len(b) != ReportBytes || b[0] != ReportID || string(b[1:5]) != "S7S2" || b[5] != Version {
		return m, bad
	}
	for _, v := range b[53:] {
		if v != 0 {
			return m, bad
		}
	}
	m = Message{Kind: b[6], Sensor: b[7], Result: b[8], Sequence: binary.LittleEndian.Uint32(b[9:]), Token: binary.LittleEndian.Uint64(b[13:]), Generation: binary.LittleEndian.Uint64(b[21:]), Mode: Mode{Width: binary.LittleEndian.Uint32(b[29:]), Height: binary.LittleEndian.Uint32(b[33:]), FPS: binary.LittleEndian.Uint32(b[37:])}, Masks: [2]byte{b[41], b[42]}, Flags: binary.LittleEndian.Uint16(b[43:]), Owner: binary.LittleEndian.Uint64(b[45:])}
	if m.Kind > Keepalive || m.Sensor > 1 || m.Result > Unsafe || m.Masks[0]&^127 != 0 || m.Masks[1]&^7 != 0 || m.Flags&^31 != 0 {
		return Message{}, bad
	}
	if m.Kind != Status {
		if m.Token == 0 || m.Sequence == 0 || m.Result != 0 || m.Generation != 0 || m.Masks != [2]byte{} || m.Flags != 0 || m.Owner != 0 {
			return Message{}, bad
		}
		if m.Kind == Acquire {
			if ModeBit(m.Sensor, m.Mode) == 0 {
				return Message{}, bad
			}
		} else if m.Mode != (Mode{}) {
			return Message{}, bad
		}
	}
	return m, nil
}

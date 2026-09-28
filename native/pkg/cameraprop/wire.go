// Package cameraprop carries camera controls independently of video and touch.
package cameraprop

import (
	"encoding/binary"
	"errors"
)

const (
	ReportID     = 11
	Size         = 65
	Query        = 1
	Set          = 2
	Reply        = 3
	OK           = 0
	Pending      = 1
	Unsupported  = 2
	Invalid      = 3
	Busy         = 4
	Failed       = 5
	Exposure     = 1
	ISO          = 2
	Focus        = 3
	WhiteBalance = 4
	AELock       = 5
	AWBLock      = 6
	Compensation = 7
	Zoom         = 8
	Mirror       = 9
	Rotation     = 10
	PowerLine    = 11
	Brightness   = 12
	Contrast     = 13
	Gamma        = 14
	Sharpness    = 15
	Temperature  = 16
	Auto         = 1
	Manual       = 2
	Locked       = 4
	Continuous   = 8
	ActiveSensor = 255
)

var ErrWire = errors.New("invalid camera property report")
var le = binary.LittleEndian

type Message struct {
	Kind, Property, Sensor, Result byte
	Sequence                       uint32
	Token                          uint64
	Value, Min, Max, Step          int64
	Flags                          uint32 // current flags 0..7, supported flags 8..15, enum mask 16..31
}

func (m Message) Marshal() [Size]byte {
	var b [Size]byte
	b[0] = ReportID
	copy(b[1:5], "S7P1")
	b[5], b[6], b[7], b[8] = m.Kind, m.Property, m.Sensor, m.Result
	le.PutUint32(b[9:], m.Sequence)
	le.PutUint64(b[13:], m.Token)
	le.PutUint64(b[21:], uint64(m.Value))
	le.PutUint64(b[29:], uint64(m.Min))
	le.PutUint64(b[37:], uint64(m.Max))
	le.PutUint64(b[45:], uint64(m.Step))
	le.PutUint32(b[53:], m.Flags)
	return b
}
func Parse(b []byte) (Message, error) {
	if len(b) != Size || b[0] != ReportID || string(b[1:5]) != "S7P1" {
		return Message{}, ErrWire
	}
	m := Message{Kind: b[5], Property: b[6], Sensor: b[7], Result: b[8], Sequence: le.Uint32(b[9:]), Token: le.Uint64(b[13:]),
		Value: int64(le.Uint64(b[21:])), Min: int64(le.Uint64(b[29:])), Max: int64(le.Uint64(b[37:])), Step: int64(le.Uint64(b[45:])), Flags: le.Uint32(b[53:])}
	if m.Kind < Query || m.Kind > Reply || m.Property < Exposure || m.Property > Temperature || (m.Sensor > 1 && m.Sensor != ActiveSensor) || m.Result > Failed {
		return Message{}, ErrWire
	}
	if m.Kind != Reply && (m.Token == 0 || m.Sequence == 0 || m.Result != 0 || m.Min != 0 || m.Max != 0 || m.Step != 0 || m.Flags&^uint32(15) != 0) {
		return Message{}, ErrWire
	}
	if m.Kind == Query && (m.Value != 0 || m.Flags != 0) {
		return Message{}, ErrWire
	}
	canonical := m.Marshal()
	if string(canonical[:]) != string(b) {
		return Message{}, ErrWire
	}
	return m, nil
}

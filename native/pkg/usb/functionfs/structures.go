package functionfs

import "encoding/binary"

const (
	DescriptorsMagic = 3
	StringsMagic     = 2
	EventWireSize    = 12
)

type DescsFlags uint32

const (
	HasFullSpeedDesc  DescsFlags = 1
	HasHighSpeedDesc  DescsFlags = 2
	HasSuperSpeedDesc DescsFlags = 4
	HasMsOsDesc       DescsFlags = 8
	AllCtrlRecip      DescsFlags = 64
	Config0Setup      DescsFlags = 128
)

type DescsHead struct {
	Magic  uint32
	Length uint32
	Flags  DescsFlags
}

type StringsHead struct {
	Magic     uint32
	Length    uint32
	StrCount  uint32
	LangCount uint32
}

type EventType uint8

const (
	EventBind EventType = iota
	EventUnbind
	EventEnable
	EventDisable
	EventSetup
	EventSuspend
	EventResume
)

type Event struct {
	Setup UsbCtrlRequest
	Type  EventType
	_     [3]byte // Padding
}

type UsbCtrlRequest struct {
	RequestType uint8
	Request     uint8
	Value       uint16
	Index       uint16
	Length      uint16
}

func (e *Event) Size() int {
	return EventWireSize
}

func (h *DescsHead) Write(buf []byte) {
	binary.LittleEndian.PutUint32(buf[0:4], h.Magic)
	binary.LittleEndian.PutUint32(buf[4:8], h.Length)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(h.Flags))
}

func (h *StringsHead) Write(buf []byte) {
	binary.LittleEndian.PutUint32(buf[0:4], h.Magic)
	binary.LittleEndian.PutUint32(buf[4:8], h.Length)
	binary.LittleEndian.PutUint32(buf[8:12], h.StrCount)
	binary.LittleEndian.PutUint32(buf[12:16], h.LangCount)
}

// Package camerawire carries encoded H.264, never raw camera pixels, over WinUSB.
package camerawire

import (
	"encoding/binary"
	"errors"
	"hash/crc32"

	"perimode/native/pkg/cameramode"
)

const (
	InterfaceGUID = "{D8718F42-69A7-4EC2-A0E5-443A07C15372}"
	Size          = 64
	MaxFrame      = 4 << 20
	Open          = 1
	Stop          = 2
	Frame         = 1
	Ack           = 2
	Failure       = 3
	KeyFrame      = 1
	Discontinuity = 2
)

var ErrWire = errors.New("invalid S7 camera transport message")
var le = binary.LittleEndian

type Command struct {
	Kind           uint16
	Token, Session uint64
	Mode           cameramode.Mode
}

func (c Command) Marshal() ([Size]byte, error) {
	var b [Size]byte
	if c.Token == 0 || c.Session == 0 || (c.Kind != Open && c.Kind != Stop) ||
		(c.Kind == Open && !c.Mode.WebcamEligible()) || (c.Kind == Stop && c.Mode != (cameramode.Mode{})) {
		return b, ErrWire
	}
	copy(b[:], "S7WC")
	le.PutUint16(b[4:], 1)
	le.PutUint16(b[6:], c.Kind)
	le.PutUint64(b[8:], c.Token)
	le.PutUint64(b[16:], c.Session)
	le.PutUint32(b[24:], c.Mode.Width)
	le.PutUint32(b[28:], c.Mode.Height)
	le.PutUint32(b[32:], c.Mode.FPS)
	le.PutUint32(b[60:], crc32.ChecksumIEEE(b[:60]))
	return b, nil
}

func ParseCommand(b []byte) (Command, error) {
	if len(b) != Size || string(b[:4]) != "S7WC" || le.Uint16(b[4:]) != 1 || le.Uint32(b[60:]) != crc32.ChecksumIEEE(b[:60]) {
		return Command{}, ErrWire
	}
	c := Command{Kind: le.Uint16(b[6:]), Token: le.Uint64(b[8:]), Session: le.Uint64(b[16:]),
		Mode: cameramode.Mode{Width: le.Uint32(b[24:]), Height: le.Uint32(b[28:]), FPS: le.Uint32(b[32:])}}
	canonical, err := c.Marshal()
	if err != nil || string(canonical[:]) != string(b) {
		return Command{}, ErrWire
	}
	return c, nil
}

type Header struct {
	Kind              uint16
	Session, Sequence uint64
	PTS               int64 // phone monotonic microseconds
	Mode              cameramode.Mode
	Flags, Bytes, CRC uint32
	Error             uint32
}

// A short final USB packet closes every frame even when encoded size happens
// to align to 512 bytes. Receivers discard this single, verified zero pad byte.
func (h Header) PacketBytes() int {
	n := Size + int(h.Bytes)
	if n%512 == 0 {
		n++
	}
	return n
}

func (h Header) Marshal() ([Size]byte, error) {
	var b [Size]byte
	if h.Session == 0 || h.PTS < 0 || h.Flags & ^uint32(KeyFrame|Discontinuity) != 0 ||
		(h.Kind != Frame && h.Kind != Ack && h.Kind != Failure) {
		return b, ErrWire
	}
	if h.Kind == Frame {
		if !h.Mode.WebcamEligible() || h.Sequence == 0 || h.Bytes < 4 || h.Bytes > MaxFrame || h.Error != 0 || (h.Flags&Discontinuity != 0 && h.Flags&KeyFrame == 0) {
			return b, ErrWire
		}
	} else if h.Bytes != 0 || h.CRC != 0 || h.Flags != 0 || h.Sequence != 0 || h.PTS != 0 ||
		(h.Kind == Ack && (h.Error != 0 || !h.Mode.WebcamEligible())) || (h.Kind == Failure && h.Error == 0) {
		return b, ErrWire
	}
	copy(b[:], "S7WF")
	le.PutUint16(b[4:], 1)
	le.PutUint16(b[6:], h.Kind)
	le.PutUint64(b[8:], h.Session)
	le.PutUint64(b[16:], h.Sequence)
	le.PutUint64(b[24:], uint64(h.PTS))
	le.PutUint32(b[32:], h.Mode.Width)
	le.PutUint32(b[36:], h.Mode.Height)
	le.PutUint32(b[40:], h.Mode.FPS)
	le.PutUint32(b[44:], h.Flags)
	le.PutUint32(b[48:], h.Bytes)
	le.PutUint32(b[52:], h.CRC)
	le.PutUint32(b[56:], h.Error)
	le.PutUint32(b[60:], crc32.ChecksumIEEE(b[:60]))
	return b, nil
}

func ParseHeader(b []byte) (Header, error) {
	if len(b) != Size || string(b[:4]) != "S7WF" || le.Uint16(b[4:]) != 1 || le.Uint32(b[60:]) != crc32.ChecksumIEEE(b[:60]) {
		return Header{}, ErrWire
	}
	h := Header{Kind: le.Uint16(b[6:]), Session: le.Uint64(b[8:]), Sequence: le.Uint64(b[16:]), PTS: int64(le.Uint64(b[24:])),
		Mode:  cameramode.Mode{Width: le.Uint32(b[32:]), Height: le.Uint32(b[36:]), FPS: le.Uint32(b[40:])},
		Flags: le.Uint32(b[44:]), Bytes: le.Uint32(b[48:]), CRC: le.Uint32(b[52:]), Error: le.Uint32(b[56:])}
	_, err := h.Marshal()
	return h, err
}

func USBDescriptors() (fs, hs [][]byte) {
	intf := []byte{9, 4, 0, 0, 2, 0xff, 0x53, 0x72, 1}
	return [][]byte{intf, {7, 5, 1, 2, 64, 0, 0}, {7, 5, 0x81, 2, 64, 0, 0}},
		[][]byte{intf, {7, 5, 1, 2, 0, 2, 0}, {7, 5, 0x81, 2, 0, 2, 0}}
}

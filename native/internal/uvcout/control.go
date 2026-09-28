// Package uvcout implements the S7 frame-based H.264 UVC output function.
// Wire layout follows the UVC 1.1 descriptors and g_uvc ABI included in this tree.
package uvcout

import (
	"encoding/binary"
	"fmt"
	"perimode/native/internal/camera"
	"perimode/native/pkg/usb/uvcmode"
)

var le = binary.LittleEndian

const WireSize = 34
const MaxFrame = 4 << 20
const (
	EventConnect uint32 = 0x08000000 + iota
	EventDisconnect
	EventStreamOn
	EventStreamOff
	EventSetup
	EventData
)

type Setup struct {
	Type, Request        byte
	Value, Index, Length uint16
}

func ParseSetup(b []byte) (Setup, error) {
	if len(b) != 8 {
		return Setup{}, fmt.Errorf("invalid USB setup size")
	}
	return Setup{b[0], b[1], le.Uint16(b[2:]), le.Uint16(b[4:]), le.Uint16(b[6:])}, nil
}

type Response struct {
	Length int32
	Data   [60]byte
}
type Controller struct {
	VC, VS        uint8
	table         uvcmode.Table
	probe, commit [WireSize]byte
	pending       byte
	committed     bool
	RequestError  byte
	HighSpeed     bool
}

var modes = []camera.Mode{{Width: 1280, Height: 720, FPS: 30}, {Width: 1280, Height: 720, FPS: 60}, {Width: 1920, Height: 1080, FPS: 30}, {Width: 1920, Height: 1080, FPS: 60}, {Width: 2560, Height: 1440, FPS: 30}}

func defaultTable() uvcmode.Table {
	var v []uvcmode.Mode
	for _, m := range modes {
		v = append(v, uvcmode.Mode(m))
	}
	table, err := uvcmode.New(v)
	if err != nil {
		panic(err)
	}
	return table
}
func control(m camera.Mode) [WireSize]byte { return controlTable(defaultTable(), m) }
func controlTable(table uvcmode.Table, m camera.Mode) [WireSize]byte {
	var b [WireSize]byte
	index, err := table.Index(uvcmode.Mode(m))
	if err != nil {
		return b
	}
	le.PutUint16(b[:], 1)
	b[2] = 1
	b[3] = index
	le.PutUint32(b[4:], m.Interval100ns())
	le.PutUint32(b[18:], MaxFrame)
	le.PutUint32(b[22:], 2048)
	le.PutUint32(b[26:], 48_000_000)
	b[30] = 3
	b[31] = 1
	b[32] = 1
	return b
}
func NewController(vc, vs uint8) (*Controller, error) {
	return NewControllerTable(vc, vs, defaultTable())
}
func NewControllerTable(vc, vs uint8, table uvcmode.Table) (*Controller, error) {
	if len(table.Frames()) == 0 {
		return nil, fmt.Errorf("empty advertised UVC table")
	}
	if vs != vc+1 || vc == 255 {
		return nil, fmt.Errorf("adjacent actual UVC interface numbers required")
	}
	c := &Controller{VC: vc, VS: vs, table: table}
	c.Reset()
	return c, nil
}
func (c *Controller) Reset() {
	c.pending = 0
	c.committed = false
	c.RequestError = 0
	c.HighSpeed = false
	c.probe = controlTable(c.table, camera.Mode(c.table.Default()))
	c.commit = c.probe
}
func (c *Controller) stall(code byte) Response { c.RequestError = code; return Response{Length: -51} }
func (c *Controller) Setup(q Setup) Response {
	c.pending = 0
	if q.Type != 0xa1 && q.Type != 0x21 {
		return c.stall(6)
	}
	if q.Value&255 != 0 {
		return c.stall(6)
	}
	in := q.Type == 0xa1
	cs := byte(q.Value >> 8)
	intf := byte(q.Index)
	entity := byte(q.Index >> 8)
	if entity != 0 {
		return c.stall(6)
	}
	r := Response{}
	if intf == c.VC && cs == 2 && in {
		if q.Request == 0x81 {
			r.Length = 1
			r.Data[0] = c.RequestError
			c.RequestError = 0
		} else if q.Request == 0x86 {
			r.Length = 1
			r.Data[0] = 1
		} else {
			return c.stall(7)
		}
	} else if intf == c.VS && (cs == 1 || cs == 2) {
		if !in {
			if q.Request != 1 || q.Length != WireSize {
				return c.stall(7)
			}
			c.pending = cs
			r.Length = WireSize
			return r
		}
		switch q.Request {
		case 0x81:
			r.Length = WireSize
			if cs == 1 {
				copy(r.Data[:], c.probe[:])
			} else {
				copy(r.Data[:], c.commit[:])
			}
		case 0x82, 0x87:
			r.Length = WireSize
			v := controlTable(c.table, camera.Mode(c.table.Default()))
			copy(r.Data[:], v[:])
		case 0x83:
			r.Length = WireSize
			v := controlTable(c.table, camera.Mode(c.table.Maximum()))
			copy(r.Data[:], v[:])
		case 0x84:
			r.Length = WireSize // GET_RES, discrete modes: zero step.
		case 0x85:
			r.Length = 2
			r.Data[0] = WireSize
		case 0x86:
			r.Length = 1
			r.Data[0] = 3
		default:
			return c.stall(7)
		}
	} else {
		return c.stall(6)
	}
	if int(r.Length) > int(q.Length) {
		r.Length = int32(q.Length)
	}
	return r
}
func wireMode(b []byte) (camera.Mode, error) { return wireModeTable(defaultTable(), b) }
func wireModeTable(table uvcmode.Table, b []byte) (camera.Mode, error) {
	if len(b) != WireSize || b[2] != 1 {
		return camera.Mode{}, fmt.Errorf("only UVC1.1 H264 format 1 is supported")
	}
	mode, err := table.Negotiate(b[3], le.Uint32(b[4:]))
	return camera.Mode(mode), err
}

// DATA belongs only to the immediately preceding accepted SET_CUR. Any malformed
// COMMIT invalidates the prior commit; otherwise old frames could leak on reopen.
func (c *Controller) Data(b []byte) error {
	pending := c.pending
	c.pending = 0
	if pending == 0 {
		return fmt.Errorf("unsolicited UVC DATA")
	}
	if pending == 2 {
		c.committed = false
	}
	m, e := wireModeTable(c.table, b)
	if e != nil {
		c.RequestError = 4
		return e
	}
	canonical := controlTable(c.table, m)
	if pending == 2 {
		if b[2] != c.probe[2] || b[3] != c.probe[3] || le.Uint32(b[4:]) != le.Uint32(c.probe[4:]) {
			c.RequestError = 4
			return fmt.Errorf("COMMIT differs from negotiated PROBE")
		}
		c.commit = canonical
		c.committed = true
	} else {
		c.probe = canonical
	}
	c.RequestError = 0
	return nil
}
func (c *Controller) Mode() (camera.Mode, error) {
	if !c.HighSpeed {
		return camera.Mode{}, fmt.Errorf("UVC H264 streaming requires USB High-Speed")
	}
	if !c.committed {
		return camera.Mode{}, fmt.Errorf("UVC streaming has no valid COMMIT")
	}
	return wireModeTable(c.table, c.commit[:])
}

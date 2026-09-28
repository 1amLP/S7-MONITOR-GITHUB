// Package fimcshot is the byte-level ABI for the supplied herolte camera HAL's
// camera2_shot_ext. It initializes version-pinned metadata, but does NOT
// initialize the ISP, allocate DMA, or claim availability of a camera. Fields are derived from the supplied
// library's instructions; see reports/fimc-shot-abi-evidence.json.
package fimcshot

import (
	"encoding/binary"
	"fmt"
)

const (
	Size                  = 0x7df0
	MagicOffset           = 0x7de8
	Magic                 = 0x23456789
	CaptureCount          = 5
	NodeBytes             = 0x28
	LeaderOffset          = 0x4
	CaptureOffset         = 0x2c
	FPSMinOffset          = 0x234
	FPSMaxOffset          = 0x238
	FrameDurationOffset   = 0x3c0
	SensorTimestampOffset = 0x29e0
)

var le = binary.LittleEndian

// View must be used only while its caller owns the metadata buffer. It does not
// retain a DMA buffer. Bind never adds a missing marker. NewTemplate is the
// separate, source-derived metadata initializer; neither proves sensor startup.
type View struct{ data []byte }

func Bind(data []byte) (View, error) {
	if len(data) != Size {
		return View{}, fmt.Errorf("shot extent is %d, expected %d", len(data), Size)
	}
	if le.Uint32(data[MagicOffset:]) != Magic {
		return View{}, fmt.Errorf("shot marker missing or incompatible")
	}
	return View{data: data}, nil
}
func (v View) valid() error { _, e := Bind(v.data); return e }

type Rect struct{ X, Y, Width, Height uint32 }

func (r Rect) valid() bool {
	return r.Width > 0 && r.Height > 0 && uint64(r.X)+uint64(r.Width) <= 16384 && uint64(r.Y)+uint64(r.Height) <= 16384
}

type Node struct {
	VideoID       uint32
	Requested     bool
	Input, Output Rect
}

func (n Node) valid() bool {
	if n.VideoID > 255 {
		return false
	}
	if n.Requested {
		return n.Input.valid() && n.Output.valid()
	}
	return n.Input == (Rect{}) && n.Output == (Rect{})
}

type Group struct {
	Leader  Node
	Capture [CaptureCount]Node
}

// SetGroup validates the WHOLE group before the first store. Metadata following
// the 240-byte node group, including 3A and dynamic results, is left untouched.
func (v View) SetGroup(g Group) error {
	if e := v.valid(); e != nil {
		return e
	}
	if !g.Leader.valid() {
		return fmt.Errorf("invalid leader geometry")
	}
	for _, n := range g.Capture {
		if !n.valid() {
			return fmt.Errorf("invalid capture geometry")
		}
	}
	put := func(at int, n Node) {
		vals := []uint32{n.VideoID, 0, n.Input.X, n.Input.Y, n.Input.Width, n.Input.Height, n.Output.X, n.Output.Y, n.Output.Width, n.Output.Height}
		if n.Requested {
			vals[1] = 1
		}
		for i, x := range vals {
			le.PutUint32(v.data[at+i*4:], x)
		}
	}
	put(LeaderOffset, g.Leader)
	for i, n := range g.Capture {
		put(CaptureOffset+i*NodeBytes, n)
	}
	return nil
}
func (v View) SetFrameRange(minimum, maximum uint32) error {
	if e := v.valid(); e != nil {
		return e
	}
	if minimum == 0 || minimum > maximum || maximum > 240 {
		return fmt.Errorf("invalid requested FPS range")
	}
	le.PutUint32(v.data[FPSMinOffset:], minimum)
	le.PutUint32(v.data[FPSMaxOffset:], maximum)
	le.PutUint64(v.data[FrameDurationOffset:], 1_000_000_000/uint64(maximum))
	return nil
}
func (v View) SensorTimestampNS() (uint64, error) {
	if e := v.valid(); e != nil {
		return 0, e
	}
	return le.Uint64(v.data[SensorTimestampOffset:]), nil
}

// camera2_sensor_dm in e418: two u64 fields, sensitivity u32, alignment, timestamp.
func (v View) SensorResult() (exposure, frameDuration uint64, iso uint32, valid bool) {
	if v.valid() != nil {
		return
	}
	exposure = le.Uint64(v.data[SensorTimestampOffset-24:])
	frameDuration = le.Uint64(v.data[SensorTimestampOffset-16:])
	iso = le.Uint32(v.data[SensorTimestampOffset-8:])
	valid = exposure > 0 && frameDuration >= exposure && frameDuration <= 1_000_000_000 && iso > 0 && iso <= 1_000_000
	return
}

// PackInput reproduces the supplied HAL's m_getSensorId(uint,uint,bool,bool)
// bit layout. The caller MUST derive module ID and route from the actual device;
// this helper deliberately does not guess rear/front module IDs or node names.
func PackInput(upstreamVideoNode uint32, connection uint8, leader, reprocessing bool, moduleID uint32) (uint32, error) {
	if upstreamVideoNode < 100 || upstreamVideoNode > 355 || connection > 15 || moduleID == 0 || moduleID > 255 {
		return 0, fmt.Errorf("invalid FIMC input selector fields")
	}
	v := (upstreamVideoNode-100)<<8 | uint32(connection)<<4 | moduleID<<16
	if leader {
		v |= 1
	}
	if reprocessing {
		v |= 1 << 24
	}
	return v, nil
}

// Contract is read-only diagnostics. It deliberately reports that capture is
// unavailable, despite these individually validated request-layout primitives.
func Contract() map[string]any {
	return map[string]any{
		"source":         "pinned supplied libexynoscamera3.so; reports/fimc-shot-abi-evidence.json",
		"metadata_bytes": Size, "magic_offset": MagicOffset, "capture_nodes_per_group": CaptureCount,
		"source_sha256":        "9604961bf7e4e8c657b70185f359cb87c368769eb71d24ea6d4b58f5ac8045f3",
		"native_capture_ready": false, "android_library_loaded": false,
		"missing":              "sensor startup, per-module profile/calibration, verified graph and streaming integration",
		"metadata_initializer": "source-derived initShotData; no Android code executed",
	}
}

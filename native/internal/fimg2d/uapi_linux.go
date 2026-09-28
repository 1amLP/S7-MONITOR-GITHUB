//go:build linux && (amd64 || arm64)

package fimg2d

import (
	"fmt"
	"unsafe"
)

// Pinned e418 m2m1shot2.h, selected by VIDEO_EXYNOS_FIMG2D_1SHOT2.
const (
	processIOCTL        uintptr = 0xc1104d04
	bufferEmpty         uint8   = 1
	bufferUser          uint8   = 2
	bufferDMA           uint8   = 3
	colorFill           uint32  = 1 << 11
	processError        uint32  = 1 << 4
	formatARGB32        uint32  = 0x34324142 // V4L2_PIX_FMT_ARGB32, BA24
	blendSource         uint16  = 2
	panelWidth                  = 1440
	panelHeight                 = 2560
	frameBytes                  = panelWidth * panelHeight * 4
	RawFrameOffsetBytes         = frameBytes
	g2dMaxSources               = 3 // e418 g2d1shot.h; UAPI permits eight, hardware permits three.
	probeRows                   = 16
	probeCols                   = 64
	probeSpan                   = probeRows * panelWidth * 4
	probeMappedSpan             = (probeSpan + 4095) &^ 4095
)

type buffer struct {
	Memory   uintptr // userptr or sign-extended fd
	Offset   uint32
	Length   uint32
	Payload  uint32
	_        [4]byte
	Reserved uintptr
}

type rect struct {
	Left, Top     int32
	Width, Height uint32
}
type format struct {
	Width, Height, PixelFormat uint32
	Crop, Window               rect
}
type extra struct {
	HorizontalFactor, VerticalFactor, FillColor uint32
	Transform, Composite                        uint16
	Alpha, AlphaRed, AlphaGreen, AlphaBlue      uint8
	XRepeat, YRepeat, ScalerFilter              uint8
	_                                           [1]byte
}
type image struct {
	Flags     uint32
	Fence     int32
	Memory    uint8
	NumPlanes uint8
	_         [6]byte
	Plane     [4]buffer
	Format    format
	Extra     extra
	Reserved  [4]uint32
	_         [4]byte
}
type task struct {
	Sources    uintptr
	Target     image
	NumSources uint8
	_          [3]byte
	Flags      uint32
	Reserved1  uint32
	Reserved2  uint32
	Reserved3  uintptr
	Reserved4  uintptr
}

func verifyABI() error {
	var b buffer
	var f format
	var e extra
	var i image
	var t task
	if unsafe.Sizeof(b) != 32 || unsafe.Sizeof(f) != 44 || unsafe.Sizeof(e) != 24 ||
		unsafe.Sizeof(i) != 232 || unsafe.Sizeof(t) != 272 ||
		unsafe.Offsetof(i.Plane) != 16 || unsafe.Offsetof(i.Format) != 144 ||
		unsafe.Offsetof(i.Extra) != 188 || unsafe.Offsetof(t.Target) != 8 ||
		unsafe.Offsetof(t.Flags) != 244 {
		return fmt.Errorf("e418 G2D one-shot LP64 ABI mismatch")
	}
	return nil
}

func solidOffscreen(mapped []byte) (*image, *task, error) {
	if len(mapped) != 2*frameBytes || len(mapped) == 0 {
		return nil, nil, fmt.Errorf("offscreen fb0 mapping size changed")
	}
	if err := verifyABI(); err != nil {
		return nil, nil, err
	}
	src := &image{Flags: colorFill, Fence: -1, Memory: bufferEmpty,
		Format: format{Width: probeCols, Height: probeRows, PixelFormat: formatARGB32,
			Crop: rect{Width: probeCols, Height: probeRows}, Window: rect{Width: probeCols, Height: probeRows}},
		Extra: extra{FillColor: 0xffffffff, Composite: blendSource}}
	dst := image{Fence: -1, Memory: bufferUser, NumPlanes: 1,
		Format: format{Width: panelWidth, Height: probeRows, PixelFormat: formatARGB32,
			Crop: rect{Width: probeCols, Height: probeRows}, Window: rect{Width: probeCols, Height: probeRows}}}
	dst.Plane[0] = buffer{Memory: uintptr(unsafe.Pointer(&mapped[frameBytes])), Length: probeMappedSpan}
	cmd := &task{Sources: uintptr(unsafe.Pointer(src)), Target: dst, NumSources: 1}
	return src, cmd, nil
}

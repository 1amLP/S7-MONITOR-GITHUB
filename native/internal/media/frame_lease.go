package media

import (
	"fmt"
	"sync/atomic"
)

type ColorDescription struct {
	Matrix string `json:"matrix"`
	Range  string `json:"range"`
	Chroma string `json:"chroma"`
}

type SensorResult struct {
	FrameCount                  uint32
	Available                   bool   `json:"available"`
	ISO                         uint32 `json:"iso"`
	ExposureNS, FrameDurationNS uint64
}

type CameraStages struct {
	RawMatched, RawDropped, RawExpired, RawRejected uint64
	ISPMatched, ISPDropped, ISPExpired, ISPRejected uint64
}

var BT601Limited = ColorDescription{Matrix: "BT601", Range: "LIMITED", Chroma: "UV"}
var BT709Limited = ColorDescription{Matrix: "BT709", Range: "LIMITED", Chroma: "UV"}

type DMAPlane struct {
	FD                     int
	Length, Offset, Stride uint32
}

// FrameLease pins a completed frame. The fds are borrowed, not dup/close rights.
// The final consumer must finish its DMA before Release returns the buffer.
type FrameLease struct {
	ID, Generation              uint64
	Width, Height               int
	StorageWidth, StorageHeight int
	PTS                         int64
	Color                       ColorDescription
	Sensor                      SensorResult
	CameraStages                CameraStages
	Planes                      []DMAPlane
	refs                        atomic.Int32
	retire                      func() error
}

func NewFrameLease(id, generation uint64, width, height int, pts int64, color ColorDescription, planes []DMAPlane, retire func() error) (*FrameLease, error) {
	if id == 0 || generation == 0 || width < 2 || height < 2 || width > 4096 || height > 4096 || width%2 != 0 || height%2 != 0 || pts < 0 || retire == nil || len(planes) < 1 || len(planes) > 2 || (color != BT601Limited && color != BT709Limited) {
		return nil, fmt.Errorf("invalid DMA frame lease")
	}
	for i, p := range planes {
		rows := height
		if i == 1 {
			rows = height / 2
		}
		if p.FD < 0 || p.Stride < uint32(width) || p.Stride > 32768 || uint64(p.Offset)+uint64(p.Stride)*uint64(rows) > uint64(p.Length) {
			return nil, fmt.Errorf("DMA frame plane outside allocation")
		}
	}
	if len(planes) == 1 && uint64(planes[0].Offset)+uint64(planes[0].Stride)*uint64(height*3/2) > uint64(planes[0].Length) {
		return nil, fmt.Errorf("short packed NV12 lease")
	}
	l := &FrameLease{ID: id, Generation: generation, Width: width, Height: height, PTS: pts, Color: color, Planes: append([]DMAPlane(nil), planes...), retire: retire}
	l.refs.Store(1)
	l.StorageWidth, l.StorageHeight = width, height
	return l, nil
}
func (l *FrameLease) Retain() error {
	if l == nil {
		return fmt.Errorf("nil frame lease")
	}
	for {
		n := l.refs.Load()
		if n <= 0 || n >= 16 {
			return fmt.Errorf("invalid frame lease retain")
		}
		if l.refs.CompareAndSwap(n, n+1) {
			return nil
		}
	}
}
func (l *FrameLease) Release() error {
	if l == nil {
		return nil
	}
	for {
		n := l.refs.Load()
		if n <= 0 {
			return fmt.Errorf("frame lease released twice")
		}
		if l.refs.CompareAndSwap(n, n-1) {
			if n == 1 {
				return l.retire()
			}
			return nil
		}
	}
}
func (l *FrameLease) Live() bool { return l != nil && l.refs.Load() > 0 }

func PreviewCrop(width, height, zoom int) (int, int) {
	a, b := width, height
	for b != 0 {
		a, b = b, a%b
	}
	k := max(2, (a*100/zoom)&^1)
	return width / a * k, height / a * k
}

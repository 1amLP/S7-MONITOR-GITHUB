//go:build linux && (amd64 || arm64)

package media

import (
	"encoding/binary"
	"fmt"
	"perimode/native/internal/linuxio"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	QueryCap              = 0x80685600
	EnumFmt               = 0xc0405602
	GFormat               = 0xc0d05604
	SFormat               = 0xc0d05605
	RequestBuffers        = 0xc0145608
	QueryBuffer           = 0xc0585609
	QueueBuffer           = 0xc058560f
	DequeueBuffer         = 0xc0585611
	StreamOn              = 0x40045612
	StreamOff             = 0x40045613
	GetControl            = 0xc008561b
	GetCrop               = 0xc014563b
	Capture        uint32 = 9
	Output         uint32 = 10
	MemoryMMap     uint32 = 1
	MemoryUserPtr  uint32 = 2
	MemoryDMABuf   uint32 = 4
	H264           uint32 = 0x34363248
	NV12M          uint32 = 0x32314d4e
	NV12           uint32 = 0x3231564e
)

var le = binary.LittleEndian

type Capability struct {
	Driver                            [16]byte
	Card                              [32]byte
	Bus                               [32]byte
	Version, Capabilities, DeviceCaps uint32
	Reserved                          [3]uint32
}
type FormatDescription struct {
	Index, Type, Flags uint32
	Description        [32]byte
	PixelFormat        uint32
	Reserved           [4]uint32
}

// Force union alignment as in struct v4l2_format (LP64).
type Format struct {
	Type    uint32
	Padding uint32
	Raw     [200]byte
}

func (f *Format) Width() uint32          { return le.Uint32(f.Raw[0:4]) }
func (f *Format) Height() uint32         { return le.Uint32(f.Raw[4:8]) }
func (f *Format) PixelFormat() uint32    { return le.Uint32(f.Raw[8:12]) }
func (f *Format) Planes() uint32         { return uint32(f.Raw[180]) }
func (f *Format) PlaneSize(i int) uint32 { return le.Uint32(f.Raw[20+20*i:]) }

// The pinned Samsung 3.18 UAPI stores bytesperline in a packed __u16.
func (f *Format) Stride(i int) uint32 { return uint32(le.Uint16(f.Raw[24+20*i:])) }
func (f *Format) Set(width, height, pixel uint32, planes byte) {
	le.PutUint32(f.Raw[0:], width)
	le.PutUint32(f.Raw[4:], height)
	le.PutUint32(f.Raw[8:], pixel)
	le.PutUint32(f.Raw[12:], 1)
	f.Raw[180] = planes
}
func (f *Format) SetPlaneSize(i int, n uint32) { le.PutUint32(f.Raw[20+20*i:], n) }

type Request struct {
	Count, Type, Memory uint32
	Reserved            [2]uint32
}
type Plane struct {
	Used, Length uint32
	Memory       uint64
	Offset       uint32
	Reserved     [11]uint32
}
type Buffer struct {
	Index, Type, Used, Flags, Field    uint32
	Pad                                uint32
	Seconds, Microseconds              int64
	Timecode                           [16]byte
	Sequence, Memory                   uint32
	PlanesPointer                      uint64
	Length, Reserved2, RequestFD, Pad2 uint32
}
type Control struct {
	ID    uint32
	Value int32
}
type Crop struct {
	Type          uint32
	Left, Top     int32
	Width, Height uint32
}

func FourCC(v uint32) string {
	return string([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
}
func ioctl(fd int, req uintptr, p unsafe.Pointer) error { return linuxio.Ioctl(fd, req, p) }
func bufferIoctl(fd int, req uintptr, b *Buffer, planes []Plane) error {
	if len(planes) < 1 || len(planes) > 8 {
		return fmt.Errorf("invalid plane array")
	}
	b.PlanesPointer = uint64(uintptr(unsafe.Pointer(&planes[0])))
	b.Length = uint32(len(planes))
	err := ioctl(fd, req, unsafe.Pointer(b))
	runtime.KeepAlive(planes)
	if b.Length > uint32(len(planes)) {
		return fmt.Errorf("kernel returned excessive plane count")
	}
	return err
}
func VerifyABI() error {
	checks := map[string][2]uintptr{"capability": {unsafe.Sizeof(Capability{}), 104}, "fmt_desc": {unsafe.Sizeof(FormatDescription{}), 64}, "format": {unsafe.Sizeof(Format{}), 208}, "request": {unsafe.Sizeof(Request{}), 20}, "plane": {unsafe.Sizeof(Plane{}), 64}, "buffer": {unsafe.Sizeof(Buffer{}), 88}, "crop": {unsafe.Sizeof(Crop{}), 20}}
	for name, p := range checks {
		if p[0] != p[1] {
			return fmt.Errorf("LP64 ABI mismatch %s: %d != %d", name, p[0], p[1])
		}
	}
	return nil
}
func IsRetry(err error) bool { return err == syscall.EAGAIN || err == syscall.EINTR }

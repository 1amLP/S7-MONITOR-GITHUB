//go:build linux && (amd64 || arm64)

package camera

// This adapter is for an ALREADY CONFIGURED standard V4L2 NV12 camera source.
// It is not a Samsung ISP backend. Raw Bayer/FIMC-IS/metadata/tiled queues are
// explicitly rejected, not treated as NV12 by guessing /dev/video numbers.
import (
	"encoding/binary"
	"errors"
	"fmt"
	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var le = binary.LittleEndian

type CaptureTarget struct {
	Path, Driver, Card string
	Mode               Mode
}
type captureMap struct {
	planes [][]byte
	queued bool
}
type LinearCapture struct {
	file                          *os.File
	format                        media.Format
	mode                          Mode
	maps                          []captureMap
	on, closed, poison, allocated bool
	closeErr                      error
}

var captureQuarantine struct {
	sync.Mutex
	objects []*LinearCapture
}

func ioctl(fd int, req uintptr, p unsafe.Pointer) error { return linuxio.Ioctl(fd, req, p) }
func planesIO(fd int, req uintptr, b *media.Buffer, p []media.Plane) error {
	if len(p) < 1 || len(p) > 2 {
		return fmt.Errorf("unsafe capture plane count")
	}
	b.PlanesPointer = uint64(uintptr(unsafe.Pointer(&p[0])))
	b.Length = uint32(len(p))
	e := ioctl(fd, req, unsafe.Pointer(b))
	runtime.KeepAlive(p)
	if b.Length != uint32(len(p)) {
		return fmt.Errorf("capture plane count changed")
	}
	return e
}
func videoName(s string) bool {
	if !strings.HasPrefix(s, "video") || len(s) == 5 {
		return false
	}
	for _, c := range s[5:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func validateTarget(t CaptureTarget) error {
	if filepath.Clean(t.Path) != t.Path || filepath.Dir(t.Path) != "/dev" || !videoName(filepath.Base(t.Path)) || t.Driver == "" || t.Card == "" || !t.Mode.Known() {
		return fmt.Errorf("exact capture node, driver, card and native mode required")
	}
	name := strings.ToLower(t.Driver + " " + t.Card)
	if strings.Contains(name, "fimc") || strings.Contains(name, "exynos") || strings.Contains(name, "sensor") || strings.Contains(name, "mfc") || strings.Contains(name, "g_uvc") {
		return ErrSensorGraph
	}
	return nil
}
func OpenLinearCapture(t CaptureTarget) (s *LinearCapture, err error) {
	if err = validateTarget(t); err != nil {
		return nil, err
	}
	if err = media.VerifyABI(); err != nil {
		return nil, err
	}
	f, e := os.OpenFile(t.Path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	s = &LinearCapture{file: f, mode: t.Mode}
	owner := s
	defer func() {
		if err != nil {
			err = errors.Join(err, owner.Close())
		}
	}()
	var caps media.Capability
	if err = ioctl(int(f.Fd()), media.QueryCap, unsafe.Pointer(&caps)); err != nil {
		return nil, err
	}
	c := caps.Capabilities
	if c&0x80000000 != 0 {
		c = caps.DeviceCaps
	}
	if linuxio.CString(caps.Driver[:]) != t.Driver || linuxio.CString(caps.Card[:]) != t.Card || c&0x04001000 != 0x04001000 || c&0x0000e002 != 0 {
		return nil, fmt.Errorf("capture identity/capabilities mismatch")
	}
	// Multi-planar is deliberate: no accidental interpretation of a single-plane
	// v4l2_pix_format as v4l2_pix_format_mplane. Packed NV12 is accepted in MPLANE.
	s.format = media.Format{Type: media.Capture}
	s.format.Set(t.Mode.Width, t.Mode.Height, media.NV12M, 2)
	if err = ioctl(int(f.Fd()), media.SFormat, unsafe.Pointer(&s.format)); err != nil {
		return nil, err
	}
	if err = ValidateCaptureLayout(s.format, t.Mode); err != nil {
		return nil, err
	}
	sp := media.StreamParameters{Type: media.Capture}
	le.PutUint32(sp.Raw[8:], 1)
	le.PutUint32(sp.Raw[12:], t.Mode.FPS)
	if err = ioctl(int(f.Fd()), media.SetParameters, unsafe.Pointer(&sp)); err != nil {
		return nil, err
	}
	n, d := le.Uint32(sp.Raw[8:]), le.Uint32(sp.Raw[12:])
	if n == 0 || uint64(d) != uint64(n)*uint64(t.Mode.FPS) {
		return nil, fmt.Errorf("camera changed requested FPS")
	}
	q := media.Request{Count: 4, Type: media.Capture, Memory: media.MemoryMMap}
	if err = ioctl(int(f.Fd()), media.RequestBuffers, unsafe.Pointer(&q)); err != nil {
		return nil, err
	}
	s.allocated = true
	if q.Count < 2 || q.Count > 8 {
		return nil, fmt.Errorf("unsafe camera buffer count")
	}
	s.maps = make([]captureMap, q.Count)
	total := uint64(0)
	for i := range s.maps {
		b := media.Buffer{Index: uint32(i), Type: media.Capture, Memory: media.MemoryMMap}
		p := make([]media.Plane, s.format.Planes())
		if err = planesIO(int(f.Fd()), media.QueryBuffer, &b, p); err != nil {
			return nil, err
		}
		for j, v := range p {
			total += uint64(v.Length)
			if v.Length < s.format.PlaneSize(j) || v.Length > 32<<20 || total > 128<<20 || v.Memory > 0xffffffff || v.Memory%4096 != 0 {
				return nil, fmt.Errorf("unsafe camera mmap geometry")
			}
			data, e := syscall.Mmap(int(f.Fd()), int64(v.Memory), int(v.Length), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
			if e != nil {
				return nil, e
			}
			s.maps[i].planes = append(s.maps[i].planes, data)
		}
		if err = s.queue(i); err != nil {
			return nil, err
		}
	}
	qt := uint32(media.Capture)
	s.on = true // A failed STREAMON may still have driver-owned queued buffers.
	if err = ioctl(int(f.Fd()), media.StreamOn, unsafe.Pointer(&qt)); err != nil {
		return nil, err
	}
	return s, nil
}
func ValidateCaptureLayout(f media.Format, m Mode) error {
	if !m.Known() || f.Width() != m.Width || f.Height() != m.Height || !((f.PixelFormat() == media.NV12M && f.Planes() == 2) || (f.PixelFormat() == media.NV12 && f.Planes() == 1)) {
		return fmt.Errorf("capture must supply exact native-size linear NV12, without metadata planes")
	}
	ys, us := f.Stride(0), f.Stride(1)
	if us == 0 || f.Planes() == 1 {
		us = ys
	}
	if ys < m.Width || us < m.Width || ys > 16384 || us > 16384 {
		return fmt.Errorf("invalid capture strides")
	}
	y, u := uint64(ys)*uint64(m.Height), uint64(us)*uint64(m.Height/2)
	if f.Planes() == 1 {
		if uint64(f.PlaneSize(0)) != y+u {
			return fmt.Errorf("ambiguous packed NV12 UV offset; separate planes required")
		}
	} else if uint64(f.PlaneSize(0)) < y || uint64(f.PlaneSize(1)) < u {
		return fmt.Errorf("short capture sizeimage")
	}
	return nil
}
func (s *LinearCapture) queue(i int) error {
	p := make([]media.Plane, len(s.maps[i].planes))
	for k, b := range s.maps[i].planes {
		p[k].Length = uint32(len(b))
	}
	b := media.Buffer{Index: uint32(i), Type: media.Capture, Memory: media.MemoryMMap}
	if e := planesIO(int(s.file.Fd()), media.QueueBuffer, &b, p); e != nil {
		return e
	}
	s.maps[i].queued = true
	return nil
}
func CaptureImage(f media.Format, m Mode, storage [][]byte, p []media.Plane, b media.Buffer) (media.Image, error) {
	if e := ValidateCaptureLayout(f, m); e != nil {
		return media.Image{}, e
	}
	if b.Flags&0x40 != 0 || b.Seconds < 0 || b.Microseconds < 0 || b.Microseconds >= 1_000_000 || b.Seconds > (math.MaxInt64-b.Microseconds)/1_000_000 {
		return media.Image{}, fmt.Errorf("invalid raw frame flags/timestamp")
	}
	if len(storage) != int(f.Planes()) || len(p) != len(storage) {
		return media.Image{}, fmt.Errorf("invalid raw frame planes")
	}
	for i, v := range p {
		if v.Offset > v.Used || v.Used > uint32(len(storage[i])) {
			return media.Image{}, fmt.Errorf("raw frame payload bounds")
		}
		storage[i] = storage[i][v.Offset:v.Used]
	}
	ys, us := int(f.Stride(0)), int(f.Stride(1))
	if us == 0 || len(p) == 1 {
		us = ys
	}
	yn, un := ys*int(m.Height), us*int(m.Height/2)
	im := media.Image{Width: int(m.Width), Height: int(m.Height), StrideY: ys, StrideUV: us, PTS: b.Seconds*1_000_000 + b.Microseconds}
	if len(p) == 1 {
		if len(storage[0]) < yn+un {
			return im, fmt.Errorf("truncated packed NV12")
		}
		im.Y, im.UV = storage[0][:yn], storage[0][yn:yn+un]
	} else {
		if len(storage[0]) < yn || len(storage[1]) < un {
			return im, fmt.Errorf("truncated NV12M")
		}
		im.Y, im.UV = storage[0][:yn], storage[1][:un]
	}
	return im, nil
}
func (s *LinearCapture) Drain(emit func(media.Image) error) (int, error) {
	if s == nil || s.closed {
		return 0, os.ErrClosed
	}
	n := 0
	for range s.maps {
		b := media.Buffer{Type: media.Capture, Memory: media.MemoryMMap}
		p := make([]media.Plane, s.format.Planes())
		e := planesIO(int(s.file.Fd()), media.DequeueBuffer, &b, p)
		if wouldBlock(e) {
			return n, nil
		}
		if e != nil {
			return n, e
		}
		if int(b.Index) >= len(s.maps) || !s.maps[b.Index].queued {
			return n, fmt.Errorf("invalid raw buffer completion")
		}
		v := &s.maps[b.Index]
		v.queued = false
		im, e := CaptureImage(s.format, s.mode, append([][]byte(nil), v.planes...), p, b)
		if e == nil {
			e = emit(im)
			n++
		}
		qe := s.queue(int(b.Index))
		if e != nil || qe != nil {
			return n, errors.Join(e, qe)
		}
	}
	return n, nil
}
func (s *LinearCapture) Close() error {
	if s == nil {
		return nil
	}
	if s.closed {
		return s.closeErr
	}
	if s.poison {
		return ErrOwnership
	}
	// STREAMOFF also releases QBUFs queued before a successful STREAMON.
	queued := s.on
	for _, v := range s.maps {
		queued = queued || v.queued
	}
	if queued {
		t := uint32(media.Capture)
		if e := ioctl(int(s.file.Fd()), media.StreamOff, unsafe.Pointer(&t)); e != nil {
			s.poison = true
			captureQuarantine.Lock()
			captureQuarantine.objects = append(captureQuarantine.objects, s)
			captureQuarantine.Unlock()
			return errors.Join(ErrOwnership, e)
		}
	}
	var e error
	for _, v := range s.maps {
		for _, p := range v.planes {
			e = errors.Join(e, syscall.Munmap(p))
		}
	}
	q := media.Request{Type: media.Capture, Memory: media.MemoryMMap}
	if s.allocated {
		e = errors.Join(e, ioctl(int(s.file.Fd()), media.RequestBuffers, unsafe.Pointer(&q)))
	}
	e = errors.Join(e, s.file.Close())
	s.closed = true
	s.closeErr = e
	return e
}

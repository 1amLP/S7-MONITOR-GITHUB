//go:build linux && (amd64 || arm64)

package uvcout

import (
	"errors"
	"fmt"
	"perimode/native/internal/camera"
	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
	"perimode/native/pkg/usb/uvcmode"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const (
	dqevent             = 0x80885659
	subscribe           = 0x4020565a
	sendResponse        = 0x40405501
	output       uint32 = 2
)

type Event struct {
	Type                 uint32
	Pad                  uint32
	Data                 [64]byte
	Pending, Sequence    uint32
	Seconds, Nanoseconds int64
	ID                   uint32
	Reserved             [8]uint32
	Pad2                 uint32
}
type Subscription struct {
	Type, ID, Flags uint32
	Reserved        [5]uint32
}
type slot struct {
	data   []byte
	queued bool
}

// deviceOps is per-owner, not a mutable package global. Tests exercise teardown
// ordering without opening a video device or pretending to emulate the S7 ISP.
type deviceOps struct {
	ctl    func(int, uintptr, unsafe.Pointer) error
	mmap   func(int, int64, int, int, int) ([]byte, error)
	munmap func([]byte) error
}
type Stats struct {
	QueuedFrames, CompletedFrames, QueuedBytes, DiscardedOnStop uint64
	Outstanding                                                 int
}
type Device struct {
	ops                         deviceOps
	stats                       Stats
	allocated                   bool
	closeErr                    error
	file                        *os.File
	Controller                  *Controller
	slots                       []slot
	mode                        camera.Mode
	on, closed, poison, havePTS bool
	lastPTS                     int64
}

var quarantine struct {
	sync.Mutex
	objects []*Device
}

func io(fd int, r uintptr, p unsafe.Pointer) error { return linuxio.Ioctl(fd, r, p) }
func (d *Device) request(r uintptr, p unsafe.Pointer) error {
	if d.ops.ctl != nil {
		return d.ops.ctl(int(d.file.Fd()), r, p)
	}
	return io(int(d.file.Fd()), r, p)
}
func (d *Device) mapBuffer(offset int64, n int) ([]byte, error) {
	fn := d.ops.mmap
	if fn == nil {
		fn = syscall.Mmap
	}
	return fn(int(d.file.Fd()), offset, n, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
}
func (d *Device) unmapBuffer(b []byte) error {
	fn := d.ops.munmap
	if fn == nil {
		fn = syscall.Munmap
	}
	return fn(b)
}
func (d *Device) Streaming(m camera.Mode) bool { return d.on && !d.closed && !d.poison && d.mode == m }
func (d *Device) Stats() Stats                 { return d.stats }
func (d *Device) quarantine(e error) error {
	if !d.poison {
		d.poison = true
		quarantine.Lock()
		quarantine.objects = append(quarantine.objects, d)
		quarantine.Unlock()
	}
	d.closeErr = errors.Join(camera.ErrOwnership, e)
	return d.closeErr
}
func VerifyABI() error {
	if unsafe.Sizeof(Event{}) != 136 || unsafe.Offsetof(Event{}.Data) != 8 || unsafe.Sizeof(Subscription{}) != 32 || unsafe.Sizeof(Response{}) != 64 {
		return fmt.Errorf("UVC LP64 ABI mismatch")
	}
	return media.VerifyABI()
}
func FindDevice() (string, error) {
	paths, e := filepath.Glob("/sys/class/video4linux/video*/name")
	if e != nil {
		return "", e
	}
	found := ""
	var readError error
	for _, p := range paths {
		v, e := os.ReadFile(p)
		if e != nil {
			if readError == nil {
				readError = fmt.Errorf("read UVC identity %s: %w", p, e)
			}
			continue
		}
		if strings.TrimSpace(string(v)) != "dwc3-gadget" {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("multiple S7 UVC output nodes")
		}
		found = "/dev/" + filepath.Base(filepath.Dir(p))
	}
	if found == "" {
		return "", errors.Join(fmt.Errorf("no dwc3-gadget at /sys/class/video4linux/video*/name (%d nodes scanned): %w", len(paths), os.ErrNotExist), readError)
	}
	return found, nil
}
func Open(path string, vc, vs uint8) (d *Device, err error) {
	return OpenTable(path, vc, vs, defaultTable())
}

// Once the producer fd is opened, even an initialization error returns its
// owner. On e418 closing this fd disconnects the entire composite USB gadget.
// The caller must retain a non-nil Device until explicit binding teardown.
func OpenTable(path string, vc, vs uint8, table uvcmode.Table) (d *Device, err error) {
	if err = VerifyABI(); err != nil {
		return nil, err
	}
	c, e := NewControllerTable(vc, vs, table)
	if e != nil {
		return nil, e
	}
	if filepath.Dir(path) != "/dev" || !strings.HasPrefix(filepath.Base(path), "video") {
		return nil, fmt.Errorf("UVC requires a selected /dev/video node")
	}
	f, e := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	d = &Device{file: f, Controller: c}
	return initializeDevice(d)
}

func initializeDevice(d *Device) (*Device, error) {
	var cap media.Capability
	if err := d.request(media.QueryCap, unsafe.Pointer(&cap)); err != nil {
		return d, fmt.Errorf("VIDIOC_QUERYCAP on %s: %w", d.file.Name(), err)
	}
	flags := cap.Capabilities
	if flags&0x80000000 != 0 {
		flags = cap.DeviceCaps
	}
	if linuxio.CString(cap.Driver[:]) != "g_uvc" || linuxio.CString(cap.Card[:]) != "dwc3-gadget" || flags&0x04000002 != 0x04000002 {
		return d, fmt.Errorf("not the pinned S7 UVC output")
	}
	for t := EventConnect; t <= EventData; t++ {
		s := Subscription{Type: t}
		if err := d.request(subscribe, unsafe.Pointer(&s)); err != nil {
			return d, fmt.Errorf("VIDIOC_SUBSCRIBE_EVENT type %#x on %s: %w", t, d.file.Name(), err)
		}
	}
	return d, nil
}

// Events performs only bounded nonblocking operations. Caller owns stream state.
func (d *Device) Events(emit func(uint32) error) error {
	if d.closed {
		return os.ErrClosed
	}
	for i := 0; i < 16; i++ {
		var e Event
		err := d.request(dqevent, unsafe.Pointer(&e))
		// This Samsung 3.18 v4l2_event_dequeue returns ENOENT, rather than
		// EAGAIN, when a nonblocking event queue is empty.
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.EINTR) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("VIDIOC_DQEVENT on %s: %w", d.file.Name(), err)
		}
		switch e.Type {
		case EventConnect:
			d.Controller.Reset()
			d.Controller.HighSpeed = le.Uint32(e.Data[:]) == 3
		case EventDisconnect:
			d.Controller.Reset()
			if err = emit(e.Type); err != nil {
				return err
			}
		case EventSetup:
			q, x := ParseSetup(e.Data[:8])
			if x != nil {
				return x
			}
			r := d.Controller.Setup(q)
			if err = d.request(sendResponse, unsafe.Pointer(&r)); err != nil {
				return fmt.Errorf("UVCIOC_SEND_RESPONSE on %s: %w", d.file.Name(), err)
			}
		case EventData:
			n := int32(le.Uint32(e.Data[:]))
			if n < 0 || n > 60 {
				d.Controller.pending = 0
				d.Controller.committed = false
				return fmt.Errorf("oversized UVC DATA event")
			}
			if err = d.Controller.Data(e.Data[4 : 4+int(n)]); err != nil {
				return err
			}
			if err = emit(e.Type); err != nil {
				return err
			}
		case EventStreamOn, EventStreamOff:
			if err = emit(e.Type); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown UVC event %x", e.Type)
		}
	}
	return nil
}
func (d *Device) Start(m camera.Mode) error {
	if d.closed {
		return os.ErrClosed
	}
	if d.poison {
		return camera.ErrOwnership
	}
	if d.Controller == nil || !d.Controller.table.Contains(uvcmode.Mode(m)) {
		return fmt.Errorf("mode is absent from this UVC session's immutable descriptors")
	}
	if e := d.Stop(); e != nil {
		return e
	}
	f := media.Format{Type: output}
	le.PutUint32(f.Raw[:], m.Width)
	le.PutUint32(f.Raw[4:], m.Height)
	le.PutUint32(f.Raw[8:], media.H264)
	le.PutUint32(f.Raw[12:], 1)
	le.PutUint32(f.Raw[20:], MaxFrame)
	if e := d.request(media.SFormat, unsafe.Pointer(&f)); e != nil {
		return fmt.Errorf("VIDIOC_S_FMT on %s: %w", d.file.Name(), e)
	}
	if le.Uint32(f.Raw[:]) != m.Width || le.Uint32(f.Raw[4:]) != m.Height || le.Uint32(f.Raw[8:]) != media.H264 || le.Uint32(f.Raw[20:]) != MaxFrame {
		return fmt.Errorf("UVC changed compressed geometry")
	}
	q := media.Request{Count: 3, Type: output, Memory: media.MemoryMMap}
	if e := d.request(media.RequestBuffers, unsafe.Pointer(&q)); e != nil {
		return fmt.Errorf("VIDIOC_REQBUFS on %s: %w", d.file.Name(), e)
	}
	d.allocated = true
	if q.Count < 2 || q.Count > 8 {
		return fmt.Errorf("invalid UVC buffer count")
	}
	d.slots = make([]slot, q.Count)
	for i := range d.slots {
		b := media.Buffer{Index: uint32(i), Type: output, Memory: media.MemoryMMap}
		if e := d.request(media.QueryBuffer, unsafe.Pointer(&b)); e != nil {
			return fmt.Errorf("VIDIOC_QUERYBUF index %d on %s: %w", i, d.file.Name(), e)
		}
		if b.Length < MaxFrame || b.Length > MaxFrame+4096 || b.PlanesPointer > 0xffffffff || b.PlanesPointer%4096 != 0 {
			return fmt.Errorf("invalid UVC MMAP geometry")
		}
		data, e := d.mapBuffer(int64(b.PlanesPointer), int(b.Length))
		if e != nil {
			return fmt.Errorf("mmap UVC buffer %d on %s: %w", i, d.file.Name(), e)
		}
		d.slots[i].data = data
	}
	// This private kernel permits empty compressed queues. Starting OUTPUT now
	// finishes the pending USB SET_INTERFACE, without sending a fabricated frame.
	d.on = true
	t := output
	if e := d.request(media.StreamOn, unsafe.Pointer(&t)); e != nil {
		return fmt.Errorf("VIDIOC_STREAMON on %s: %w", d.file.Name(), e)
	}
	d.mode = m
	d.havePTS = false
	return nil
}
func (d *Device) recycle() error {
	for range d.slots {
		b := media.Buffer{Type: output, Memory: media.MemoryMMap}
		e := d.request(media.DequeueBuffer, unsafe.Pointer(&b))
		if errors.Is(e, syscall.EAGAIN) || errors.Is(e, syscall.EINTR) {
			return nil
		}
		if e != nil {
			return fmt.Errorf("VIDIOC_DQBUF on %s: %w", d.file.Name(), e)
		}
		if int(b.Index) >= len(d.slots) || !d.slots[b.Index].queued {
			return fmt.Errorf("invalid UVC buffer completion")
		}
		d.slots[b.Index].queued = false
		d.stats.Outstanding--
		if b.Flags&0x40 != 0 {
			return fmt.Errorf("UVC transfer returned ERROR buffer")
		}
		d.stats.CompletedFrames++
	}
	return nil
}
func (d *Device) Submit(p media.Encoded) error {
	if d.closed {
		return os.ErrClosed
	}
	if !d.on {
		return fmt.Errorf("UVC stream is stopped")
	}
	if len(p.Data) < 4 || len(p.Data) > MaxFrame || p.PTS < 0 || (d.havePTS && p.PTS <= d.lastPTS) {
		return fmt.Errorf("invalid UVC access unit/PTS")
	}
	if e := d.recycle(); e != nil {
		return e
	}
	for i := range d.slots {
		v := &d.slots[i]
		if v.queued {
			continue
		}
		copy(v.data, p.Data)
		b := media.Buffer{Index: uint32(i), Type: output, Memory: media.MemoryMMap, Used: uint32(len(p.Data)), Length: uint32(len(v.data)), Seconds: p.PTS / 1_000_000, Microseconds: p.PTS % 1_000_000}
		if e := d.request(media.QueueBuffer, unsafe.Pointer(&b)); e != nil {
			return fmt.Errorf("VIDIOC_QBUF on %s: %w", d.file.Name(), e)
		}
		v.queued = true
		d.stats.QueuedFrames++
		d.stats.QueuedBytes += uint64(len(p.Data))
		d.stats.Outstanding++
		d.lastPTS = p.PTS
		d.havePTS = true
		return nil
	}
	return syscall.EAGAIN
}
func (d *Device) Stop() error {
	if d.poison {
		return d.closeErr
	}
	if d.closed {
		return d.closeErr
	}
	queued := d.on
	for _, s := range d.slots {
		queued = queued || s.queued
	}
	if queued {
		t := output
		if e := d.request(media.StreamOff, unsafe.Pointer(&t)); e != nil {
			return d.quarantine(fmt.Errorf("VIDIOC_STREAMOFF on %s: %w", d.file.Name(), e))
		}
	}
	d.on = false
	for i := range d.slots {
		s := &d.slots[i]
		if s.queued {
			s.queued = false
			d.stats.DiscardedOnStop++
		}
		if s.data != nil {
			if e := d.unmapBuffer(s.data); e != nil {
				return d.quarantine(fmt.Errorf("munmap UVC buffer %d on %s: %w", i, d.file.Name(), e))
			}
			s.data = nil
		}
	}
	d.stats.Outstanding = 0
	if d.allocated {
		q := media.Request{Type: output, Memory: media.MemoryMMap}
		if e := d.request(media.RequestBuffers, unsafe.Pointer(&q)); e != nil {
			return d.quarantine(fmt.Errorf("VIDIOC_REQBUFS release on %s: %w", d.file.Name(), e))
		}
		d.allocated = false
	}
	d.slots = nil
	return nil
}
func (d *Device) Close() error {
	if d == nil {
		return nil
	}
	if d.closed {
		return d.closeErr
	}
	if e := d.Stop(); e != nil {
		return e
	}
	d.closed = true
	d.closeErr = d.file.Close()
	return d.closeErr
}

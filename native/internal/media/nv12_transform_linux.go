//go:build linux && (amd64 || arm64)

package media

import (
	"errors"
	"fmt"
	"perimode/native/internal/fimg2d"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type dmaTransformSlot struct {
	fds    [2]*os.File
	length [2]uint32
	busy   atomic.Bool
}
type NV12Transformer struct {
	file                           *os.File
	fill                           *fimg2d.FrameCopier
	slots                          [3]dmaTransformSlot
	width, height, stride          int
	zoom                           int
	id                             uint64
	outOn, capOn, closed, poisoned bool
	retained                       bool
	input                          *FrameLease
	pending                        int
}

var transformPoolID atomic.Uint64
var retainedTransforms struct {
	sync.Mutex
	items []*NV12Transformer
}

func (t *NV12Transformer) retain() {
	retainedTransforms.Lock()
	defer retainedTransforms.Unlock()
	if !t.retained {
		t.retained = true
		retainedTransforms.items = append(retainedTransforms.items, t)
	}
}

func NewNV12Transformer(width, height, rotation int, mirror bool, zoom int) (*NV12Transformer, error) {
	if width < 2 || height < 2 || width > 2560 || height > 1440 || width%16 != 0 || height%2 != 0 || rotation < 0 || rotation > 270 || rotation%90 != 0 || zoom < 100 || zoom > 400 {
		return nil, fmt.Errorf("invalid hardware camera transform")
	}
	var last error
	for _, path := range []string{"/dev/video51", "/dev/video50"} {
		t, err := openNV12Transform(path, width, height, rotation, mirror, zoom)
		if err == nil {
			return t, nil
		}
		last = errors.Join(last, err)
		if errors.Is(err, ErrQuarantined) {
			break
		}
	}
	return nil, last
}

func openNV12Transform(path string, width, height, rotation int, mirror bool, zoom int) (_ *NV12Transformer, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	t := &NV12Transformer{file: f, width: width, height: height, zoom: zoom, pending: -1, id: transformPoolID.Add(1)}
	defer func() {
		if err != nil {
			err = errors.Join(err, t.Close())
		}
	}()
	var caps Capability
	if err = ioctl(int(f.Fd()), QueryCap, unsafe.Pointer(&caps)); err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(string(caps.Driver[:])), "scaler") {
		return nil, fmt.Errorf("unexpected camera scaler")
	}
	for _, kind := range []uint32{Output, Capture} {
		format := Format{Type: kind}
		format.Set(uint32(width), uint32(height), NV12M, 2)
		le.PutUint32(format.Raw[16:], 1)
		if err = ioctl(int(f.Fd()), SFormat, unsafe.Pointer(&format)); err != nil {
			return nil, err
		}
		if format.Width() != uint32(width) || format.Height() != uint32(height) || format.PixelFormat() != NV12M || format.Planes() != 2 || format.Stride(0) != uint32(width) {
			return nil, fmt.Errorf("scaler changed camera NV12 layout")
		}
		t.stride = int(format.Stride(0))
		cw, ch := PreviewCrop(width, height, zoom)
		crop := Crop{Type: kind, Left: int32((width - cw) / 2 &^ 1), Top: int32((height - ch) / 2 &^ 1), Width: uint32(cw), Height: uint32(ch)}
		if kind == Capture {
			rw, rh := cw, ch
			if rotation%180 != 0 {
				rw, rh = rh, rw
			}
			dw, dh := width, (width*rh/rw)&^1
			if dh > height {
				dh = height
				dw = (height * rw / rh) &^ 1
			}
			crop = Crop{Type: kind, Left: int32((width - dw) / 2 &^ 1), Top: int32((height - dh) / 2 &^ 1), Width: uint32(dw), Height: uint32(dh)}
		}
		if err = ioctl(int(f.Fd()), 0x4014563c, unsafe.Pointer(&crop)); err != nil {
			return nil, err
		}
		got := Crop{Type: kind}
		if err = ioctl(int(f.Fd()), GetCrop, unsafe.Pointer(&got)); err != nil || got != crop {
			return nil, fmt.Errorf("camera crop changed: %v", err)
		}
	}
	m := int32(0)
	if mirror {
		m = 1
	}
	for _, c := range []Control{{ID: controlRotate, Value: int32(rotation)}, {ID: 0x00980914, Value: m}, {ID: controlCSCEquation, Value: 1}, {ID: controlCSCRange, Value: 0}} {
		if err = ioctl(int(f.Fd()), SetControl, unsafe.Pointer(&c)); err != nil {
			return nil, err
		}
	}
	// e418 caches scaling ratios during REQBUFS, including rotation. Both
	// formats, both crops and all controls must be set before queue creation.
	for _, kind := range []uint32{Output, Capture} {
		req := Request{Type: kind, Memory: MemoryDMABuf, Count: 1}
		if err = ioctl(int(f.Fd()), RequestBuffers, unsafe.Pointer(&req)); err != nil {
			return nil, err
		}
		if req.Count != 1 {
			return nil, fmt.Errorf("unexpected camera scaler queue count")
		}
	}
	t.fill, err = fimg2d.OpenFrameCopier()
	if err != nil {
		return nil, err
	}
	for i := range t.slots {
		for p := 0; p < 2; p++ {
			n := ((width + 15) / 16) * ((height + 15) / 16) * 256
			if p == 1 {
				n = ((n/2 + 255) / 256) * 256
			}
			n = (n + 256 + 4095) &^ 4095
			t.slots[i].length[p] = uint32(n)
			t.slots[i].fds[p], err = AllocateScalerDMABuf(n)
			if err != nil {
				return nil, err
			}
			black := byte(16)
			if p == 1 {
				black = 128
			}
			if err = t.fill.FillPlane(int(t.slots[i].fds[p].Fd()), n, black); err != nil {
				t.poisoned = true
				return nil, err
			}
		}
	}
	return t, nil
}

// e418 sc_vb2_queue_setup caches ratios; S_CROP alone leaves stale ratios.
// Recreate only empty scaler queues. Allocated DMA slots and MFC leases stay.
func (t *NV12Transformer) SetZoom(zoom int) error {
	return t.setZoom(zoom, func(request uintptr, value unsafe.Pointer) error {
		return ioctl(int(t.file.Fd()), request, value)
	})
}

func (t *NV12Transformer) setZoom(zoom int, call func(uintptr, unsafe.Pointer) error) error {
	if t.closed || t.poisoned {
		return ErrQuarantined
	}
	if zoom < 100 || zoom > 400 {
		return fmt.Errorf("invalid hardware camera zoom")
	}
	if t.input != nil || t.pending >= 0 {
		return fmt.Errorf("camera scaler still owns a frame")
	}
	if t.zoom == zoom {
		return nil
	}
	for _, kind := range []uint32{Capture, Output} {
		on := &t.capOn
		if kind == Output {
			on = &t.outOn
		}
		if *on {
			if err := call(StreamOff, unsafe.Pointer(&kind)); err != nil {
				return err
			}
			*on = false
		}
	}
	for _, kind := range []uint32{Output, Capture} {
		req := Request{Type: kind, Memory: MemoryDMABuf}
		if err := call(RequestBuffers, unsafe.Pointer(&req)); err != nil {
			return err
		}
		if req.Count != 0 {
			return fmt.Errorf("camera scaler queue not released")
		}
	}
	w, h := PreviewCrop(t.width, t.height, zoom)
	crop := Crop{Type: Output, Left: int32((t.width - w) / 2 &^ 1), Top: int32((t.height - h) / 2 &^ 1), Width: uint32(w), Height: uint32(h)}
	if err := call(0x4014563c, unsafe.Pointer(&crop)); err != nil {
		return err
	}
	got := Crop{Type: Output}
	if err := call(GetCrop, unsafe.Pointer(&got)); err != nil || got != crop {
		return fmt.Errorf("camera live crop changed: %v", err)
	}
	for _, kind := range []uint32{Output, Capture} {
		req := Request{Type: kind, Memory: MemoryDMABuf, Count: 1}
		if err := call(RequestBuffers, unsafe.Pointer(&req)); err != nil {
			return err
		}
		if req.Count != 1 {
			return fmt.Errorf("camera scaler queue changed")
		}
	}
	t.zoom = zoom
	return nil
}

func (t *NV12Transformer) Apply(im Image) (out Image, err error) {
	defer func() {
		if t.pending >= 0 && (errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR)) {
			err = fmt.Errorf("incomplete scaler DMA transaction: %v", err)
		}
	}()
	if t.closed || t.poisoned {
		return out, ErrQuarantined
	}
	if t.input != nil || t.pending >= 0 {
		return out, fmt.Errorf("camera scaler still owns a frame")
	}
	if !im.Lease.Live() || im.Width != t.width || im.Height != t.height || len(im.Lease.Planes) != 2 {
		return out, fmt.Errorf("camera hardware transform needs DMA input")
	}
	for _, p := range im.Lease.Planes {
		if p.Stride != uint32(t.stride) {
			return out, fmt.Errorf("camera transform stride mismatch")
		}
	}
	index := -1
	for i := range t.slots {
		if t.slots[i].busy.CompareAndSwap(false, true) {
			index = i
			break
		}
	}
	if index < 0 {
		return out, syscall.EAGAIN
	}
	if err := im.Lease.Retain(); err != nil {
		t.slots[index].busy.Store(false)
		return out, err
	}
	t.input = im.Lease
	t.pending = index
	s := &t.slots[index]
	for _, kind := range []uint32{Capture, Output} {
		p := make([]Plane, 2)
		for j := range p {
			if kind == Capture {
				p[j] = Plane{Memory: uint64(s.fds[j].Fd()), Length: s.length[j]}
			} else {
				v := im.Lease.Planes[j]
				rows := t.height
				if j == 1 {
					rows /= 2
				}
				p[j] = Plane{Memory: uint64(v.FD), Length: v.Length, Offset: v.Offset, Used: v.Offset + v.Stride*uint32(rows)}
			}
		}
		b := Buffer{Type: kind, Memory: MemoryDMABuf}
		if err := bufferIoctl(int(t.file.Fd()), QueueBuffer, &b, p); err != nil {
			return out, err
		}
	}
	for _, kind := range []uint32{Output, Capture} {
		on := &t.outOn
		if kind == Capture {
			on = &t.capOn
		}
		if !*on {
			if err := ioctl(int(t.file.Fd()), StreamOn, unsafe.Pointer(&kind)); err != nil {
				return out, err
			}
			*on = true
		}
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for _, kind := range []uint32{Output, Capture} {
		for {
			p := make([]Plane, 2)
			b := Buffer{Type: kind, Memory: MemoryDMABuf}
			err := bufferIoctl(int(t.file.Fd()), DequeueBuffer, &b, p)
			if err == syscall.EAGAIN {
				event := int16(1)
				if kind == Output {
					event = 4
				}
				if err = waitVideo(int(t.file.Fd()), event, deadline); err == nil {
					continue
				}
			}
			if err != nil {
				return out, err
			}
			if b.Index != 0 || b.Flags&0x40 != 0 || b.Length != 2 {
				return out, fmt.Errorf("invalid camera scaler completion")
			}
			for j, plane := range p {
				fd, length := uint64(s.fds[j].Fd()), s.length[j]
				if kind == Output {
					fd, length = uint64(im.Lease.Planes[j].FD), im.Lease.Planes[j].Length
				}
				if plane.Memory != fd || plane.Length != length {
					return out, fmt.Errorf("camera scaler DMA identity changed")
				}
			}
			break
		}
	}
	if err := t.input.Release(); err != nil {
		return out, err
	}
	t.input = nil
	t.pending = -1
	planes := []DMAPlane{{FD: int(s.fds[0].Fd()), Length: s.length[0], Stride: uint32(t.stride)}, {FD: int(s.fds[1].Fd()), Length: s.length[1], Stride: uint32(t.stride)}}
	l, err := NewFrameLease(1<<62|t.id<<16|uint64(index+1), t.id, t.width, t.height, im.PTS, im.Lease.Color, planes, func() error { s.busy.Store(false); return nil })
	if err != nil {
		s.busy.Store(false)
		return out, err
	}
	l.Sensor = im.Lease.Sensor
	l.CameraStages = im.Lease.CameraStages
	return Image{Width: t.width, Height: t.height, StrideY: t.stride, StrideUV: t.stride, PTS: im.PTS, Lease: l}, nil
}

func (t *NV12Transformer) Close() error {
	if t == nil || t.closed {
		return nil
	}
	if t.poisoned {
		t.retain()
		return ErrQuarantined
	}
	for _, kind := range []uint32{Capture, Output} {
		on := t.capOn
		if kind == Output {
			on = t.outOn
		}
		if on || t.pending >= 0 {
			if err := ioctl(int(t.file.Fd()), StreamOff, unsafe.Pointer(&kind)); err != nil {
				t.poisoned = true
				t.retain()
				return errors.Join(ErrQuarantined, err)
			}
		}
	}
	if t.input != nil {
		_ = t.input.Release()
		t.input = nil
	}
	if t.pending >= 0 {
		t.slots[t.pending].busy.Store(false)
		t.pending = -1
	}
	for i := range t.slots {
		if t.slots[i].busy.Load() {
			t.poisoned = true
			t.retain()
			return errors.Join(ErrQuarantined, fmt.Errorf("MFC still owns transformed camera frame"))
		}
	}
	var err error
	if t.fill != nil {
		if err = t.fill.Close(); err != nil {
			t.poisoned = true
			t.retain()
			return err
		}
	}
	err = errors.Join(err, t.file.Close())
	for i := range t.slots {
		for _, f := range t.slots[i].fds {
			if f != nil {
				err = errors.Join(err, f.Close())
			}
		}
	}
	t.closed = true
	return err
}

//go:build linux && (amd64 || arm64)

package gpumenu

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"time"

	"perimode/native/internal/media"
)

type PreviewTexture struct{ FD, Width, Height, Bytes, Slot int }

func (r *Renderer) nextSequence() uint32 {
	r.seq++
	if r.seq == 0 {
		r.seq++
	}
	return r.seq
}

// Called with r.mu held. Cache identity is the allocation/slot, not the fd
// number, which the source can reuse after a camera mode change.
func (r *Renderer) importFrame(l *media.FrameLease) (int, error) {
	return r.importBuffers(l.ID, l.Planes, false)
}

func (r *Renderer) importBuffers(identity uint64, planes []media.DMAPlane, writable bool) (int, error) {
	for i, id := range r.dmaCache {
		if id == identity {
			return i, nil
		}
	}
	i := r.nextDMA
	r.nextDMA = (i + 1) % len(r.dmaCache)
	head := [8]uint32{0x31494d47, r.nextSequence(), uint32(i), uint32(len(planes))}
	if writable {
		head[6] = 1
	}
	var fds []int
	for j, p := range planes {
		head[4+j] = p.Length
		fds = append(fds, p.FD)
	}
	if err := binary.Write(r.in, binary.LittleEndian, head); err != nil {
		return 0, err
	}
	n, err := syscall.SendmsgN(int(r.transferFD.Fd()), []byte{'D'}, syscall.UnixRights(fds...), nil, syscall.MSG_DONTWAIT)
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, fmt.Errorf("short GPU fd transfer")
	}
	if err = r.ack(head[1]); err != nil {
		return 0, err
	}
	r.dmaCache[i] = identity
	return i, nil
}

// The caller holds the input lease until this synchronous GPU completion. On
// uncertain completion retain an extra reference until the worker has exited.
func (r *Renderer) PreviewDMA(l *media.FrameLease, rotation, panelRotation int, mirror bool, zoom, slot, width, height int) (PreviewTexture, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result PreviewTexture
	if r.closed {
		return result, os.ErrClosed
	}
	if r.failed != nil {
		return result, r.failed
	}
	if !l.Live() || len(l.Planes) < 1 || len(l.Planes) > 2 || l.Width > 2560 || l.Height > 1440 || rotation < 0 || rotation > 270 || rotation%90 != 0 || panelRotation < 0 || panelRotation > 270 || panelRotation%90 != 0 || zoom < 100 || zoom > 400 || slot < 0 || slot > 1 || width < 2 || height < 2 || width > 1024 || height > 1024 || width%2 != 0 || height%2 != 0 {
		return result, fmt.Errorf("invalid GPU DMA Preview")
	}
	if err := l.Retain(); err != nil {
		return result, err
	}
	confirmed := false
	defer func() {
		if confirmed {
			_ = l.Release()
		} else {
			r.heldFrames = append(r.heldFrames, l)
		}
	}()
	deadline := time.Now().Add(250 * time.Millisecond)
	_ = r.in.SetWriteDeadline(deadline)
	_ = r.out.SetReadDeadline(deadline)
	index, err := r.importFrame(l)
	if err != nil {
		r.failed = err
		return result, err
	}
	m := uint32(0)
	if mirror {
		m = 1
	}
	cw, ch := media.PreviewCrop(l.Width, l.Height, zoom)
	head := [8]uint32{0x32504d47, r.nextSequence(), uint32(rotation) | uint32(panelRotation)<<16, m | uint32(slot)<<1, uint32(l.Width), uint32(l.Height), uint32(cw), uint32(ch)}
	p0 := l.Planes[0]
	p1 := p0
	if len(l.Planes) == 2 {
		p1 = l.Planes[1]
	} else {
		p1.Offset += uint32(l.Height) * p0.Stride
	}
	color := uint32(0)
	if l.Color == media.BT709Limited {
		color = 1
	}
	shape := [8]uint32{uint32(index), p0.Stride, p1.Stride, p0.Offset, p1.Offset, color, uint32(width), uint32(height)}
	err = binary.Write(r.in, binary.LittleEndian, head)
	if err == nil {
		err = binary.Write(r.in, binary.LittleEndian, shape)
	}
	if err == nil {
		err = r.ack(head[1])
	}
	if err != nil {
		r.failed = fmt.Errorf("Mali DMA Preview: %w", err)
		return result, r.failed
	}
	confirmed = true
	if panelRotation%180 != 0 {
		width, height = height, width
	}
	return PreviewTexture{FD: r.PreviewFD(slot), Width: width, Height: height, Bytes: PreviewBytes, Slot: slot}, nil
}

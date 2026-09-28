//go:build linux && (amd64 || arm64)

package fimcdma

import (
	"errors"
	"fmt"

	"perimode/native/internal/media"
	"sync"
	"unsafe"
)

// Pool is the allocation part of one already configured FIMC node. It never
// derives S_INPUT, loads sensor firmware, sets FPS or selects a sensor mode.
type Pool struct {
	mu          sync.Mutex
	Queue       *Queue
	failedNode  NodeIO
	allocations []*Allocation
	closed      bool
}

func ValidateNodeLayout(f media.Format) error { return ValidateTypedLayout(f, ShotMetadata) }
func ValidateTypedLayout(f media.Format, role MetadataRole) error {
	minimum, e := role.minimum()
	if e != nil {
		return e
	}
	if role == StreamMetadata && f.Type != media.Capture {
		return fmt.Errorf("stream result requires capture queue")
	}

	if (f.Type != media.Capture && f.Type != media.Output) || f.Width() == 0 || f.Height() == 0 || f.Width() > 8192 || f.Height() > 8192 || f.PixelFormat() == 0 || f.Planes() < 2 || f.Planes() > 3 {
		return fmt.Errorf("invalid FIMC node layout")
	}
	total := uint64(0)
	for i := 0; i < int(f.Planes()); i++ {
		n := f.PlaneSize(i)
		if n == 0 || n > MaxAllocation {
			return fmt.Errorf("invalid FIMC plane sizeimage")
		}
		total += uint64(n)
	}
	last := f.PlaneSize(int(f.Planes()) - 1)
	if int(last) < minimum || last > 128<<10 || total > MaxAllocation {
		return fmt.Errorf("no bounded final metadata plane")
	}
	return nil
}

// AllocatePool uses exactly the driver's returned sizeimage. It refuses a short
// metadata plane instead of borrowing the image plane or silently shrinking it.
// An error can return a non-nil Pool when cleanup itself failed. The caller must
// retain that Pool and retry Close before closing the borrowed node or allocator.
// A successfully returned Pool must be treated as the sole queue owner.
func AllocatePool(a *Allocator, n *PreparedNode, count uint32) (*Pool, error) {
	return AllocateTypedPool(a, n, count, ShotMetadata)
}
func AllocateTypedPool(a *Allocator, n *PreparedNode, count uint32, role MetadataRole) (p *Pool, err error) {
	return allocateTypedPool(a, n, count, role, nil)
}

// The ISP borrows each upstream RAW image plane but owns a separate shot plane.
// Queued DMA pins prevent the upstream slot being reused until ISP DQBUF.
func AllocateSharedRawPool(a *Allocator, n *PreparedNode, count uint32, source *Pool) (*Pool, error) {
	if source == nil || source.Queue == nil {
		return nil, fmt.Errorf("missing upstream RAW pool")
	}
	q := source.Queue
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.role != StreamMetadata || q.started || q.touched || q.closed || q.poison || uint32(len(q.slots)) != count {
		return nil, fmt.Errorf("RAW pool must be idle and have the same fixed slot count")
	}
	return allocateTypedPool(a, n, count, ShotMetadata, q)
}

func allocateTypedPool(a *Allocator, n *PreparedNode, count uint32, role MetadataRole, shared *Queue) (p *Pool, err error) {
	if a == nil || n == nil || count < 2 || count > 8 {
		return nil, fmt.Errorf("invalid native FIMC pool arguments")
	}
	f := media.Format{Type: n.kind}
	if n.kernelABI == Herolte481ABI && n.expected != nil {
		f = *n.expected // profile/S_FMT layout; this kernel has no G_FMT implementation
	} else if n.kernelABI != "" {
		return nil, fmt.Errorf("unknown prepared-node ABI")
	} else if err = n.ioctl(media.GFormat, unsafe.Pointer(&f)); err != nil {
		return nil, err
	}
	if f.Type != n.kind || int(f.Planes()) != n.planes || (n.expected != nil && (!SameLayout(*n.expected, f) || n.role != role)) {
		return nil, fmt.Errorf("FIMC layout changed")
	}
	if err = ValidateTypedLayout(f, role); err != nil {
		return nil, err
	}
	n.importsRequested = true
	req := media.Request{Type: n.kind, Memory: memoryDMABUF, Count: count}
	if err = n.ioctl(media.RequestBuffers, unsafe.Pointer(&req)); err != nil {
		return nil, err
	}
	p = &Pool{}
	owner := p
	defer func() {
		if err != nil {
			cleanup := owner.closeUnqueued(n)
			err = errors.Join(err, cleanup)
			if cleanup == nil {
				p = nil
			} else {
				owner.failedNode = n
				p = owner // Preserve failed setup ownership; never lose it.
			}
		}
	}()
	if req.Count < 2 || req.Count > 8 {
		return p, fmt.Errorf("unsafe returned FIMC buffer count")
	}
	if shared != nil && (req.Count != uint32(len(shared.slots)) || len(shared.extents) != n.planes) {
		return p, fmt.Errorf("shared RAW pool layout/count changed")
	}
	var rows [][]*Allocation
	for i := uint32(0); i < req.Count; i++ {
		var row []*Allocation
		for j := 0; j < n.planes; j++ {
			if shared != nil && j < n.planes-1 {
				if shared.extents[j] != f.PlaneSize(j) {
					return p, fmt.Errorf("shared RAW plane extent mismatch")
				}
				row = append(row, shared.slots[i].planes[j])
				continue
			}
			capacity := int(f.PlaneSize(j))
			if role == StreamMetadata && f.PixelFormat() == media.NV12M && j < 2 {
				// e418 MFC v9 linear input: macroblock extent plus 256 bytes.
				// Reserve padding once; FIMC still receives its exact sizeimage.
				padded := int((f.Width()+15)/16) * int((f.Height()+15)/16) * 256
				if j == 1 {
					padded = ((padded/2 + 255) / 256) * 256
				}
				capacity = max(capacity, padded+256)
			}
			b, e := a.Allocate(capacity)
			if e != nil {
				return p, e
			}
			owner.allocations = append(owner.allocations, b)
			row = append(row, b)
		}
		rows = append(rows, row)
	}
	extents := make([]uint32, n.planes)
	for i := range extents {
		extents[i] = f.PlaneSize(i)
	}
	owner.Queue, err = NewTypedQueue(n, rows, role, extents)
	if err != nil {
		return p, err
	}
	owner.Queue.sharedRaw = shared
	return owner, nil
}
func (p *Pool) closeUnqueued(n NodeIO) error {
	// No QBUF has occurred during construction, so there is no DMA ownership to
	// infer. Failed Release still surfaces and the graph retains the node fd.
	e := n.Release()
	for _, b := range p.allocations {
		e = errors.Join(e, b.Close())
	}
	return e
}
func (p *Pool) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	if p.failedNode != nil {
		if e := p.closeUnqueued(p.failedNode); e != nil {
			return e
		}
		p.failedNode = nil
		p.closed = true
		return nil
	}
	if p.Queue == nil {
		return fmt.Errorf("pool has no queue owner")
	}
	if e := p.Queue.Close(); e != nil {
		return e
	}
	var e error
	for _, b := range p.allocations {
		e = errors.Join(e, b.Close())
	}
	if e == nil {
		p.closed = true
	}
	return e
}

//go:build linux && (amd64 || arm64)

package fimcdma

import (
	"errors"

	"fmt"
	"perimode/native/internal/camera/fimcshot"
	"perimode/native/internal/media"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Plane is the imported fd/extent, not a CPU pointer passed to the kernel.
type Plane struct {
	FD     int
	Length uint32
}
type Completion struct {
	Index, Flags           uint32
	Lengths, Used, Offsets []uint32
}

// NodeIO is a configured node. S_INPUT/S_FMT/REQBUFS and native firmware/sensor
// startup belong to the graph, not to this queue. No guessed private ioctl is
// hidden here. Calls must obey the nonblocking V4L2 queue contract.
type NodeIO interface {
	Queue(uint32, []Plane) error
	Dequeue() (Completion, error)
	Start() error
	Stop() error
	Release() error
}

// MetadataRole is fixed before allocating a queue. A capture result must never
// be initialized or interpreted as a leader request merely because both use an
// extra DMA plane.
type MetadataRole uint8

const (
	ShotMetadata MetadataRole = iota
	StreamMetadata
)

func (r MetadataRole) minimum() (int, error) {
	switch r {
	case ShotMetadata:
		return fimcshot.Size, nil
	case StreamMetadata:
		return fimcshot.StreamHeaderSize, nil
	}
	return 0, fmt.Errorf("unknown FIMC metadata role")
}

type state uint8

const (
	idle state = iota
	driverOwned
	completed
)

type slot struct {
	planes        []*Allocation
	state         state
	serial        uint64
	lease         bool
	flags         uint32
	used, offsets []uint32
}
type Frame struct {
	Index         uint32
	serial, epoch uint64
	queue         *Queue
	Flags         uint32
}

var nextQueueID atomic.Uint64

type Queue struct {
	exported                         atomic.Int32
	exportDone                       chan struct{}
	id                               uint64
	mu                               sync.Mutex
	io                               NodeIO
	slots                            []slot
	started, touched, poison, closed bool
	epoch, next                      uint64
	role                             MetadataRole
	extents                          []uint32
	sharedRaw                        *Queue
}

// NewQueue retains the leader/request default for existing callers. Graph
// capture nodes must use NewTypedQueue(..., StreamMetadata, sizeimage).
func NewQueue(io NodeIO, buffers [][]*Allocation) (*Queue, error) {
	return NewTypedQueue(io, buffers, ShotMetadata, nil)
}

// extents are the NEGOTIATED sizeimage values, never the page-rounded allocation
// sizes. A nil extent list is only allowed for the legacy request-only helper.
func NewTypedQueue(io NodeIO, buffers [][]*Allocation, role MetadataRole, extents []uint32) (*Queue, error) {
	minimum, e := role.minimum()
	if e != nil {
		return nil, e
	}
	if role == StreamMetadata && len(extents) == 0 {
		return nil, fmt.Errorf("capture requires negotiated extents")
	}

	if io == nil || len(buffers) < 2 || len(buffers) > 8 {
		return nil, fmt.Errorf("configured node and 2..8 buffers required")
	}
	q := &Queue{io: io, epoch: 1, role: role, id: nextQueueID.Add(1), exportDone: make(chan struct{}, 1)}
	ownership.Lock()
	defer ownership.Unlock()
	seen := map[*Allocation]bool{}
	planes := len(buffers[0])
	if planes < 2 || planes > 3 {
		return nil, fmt.Errorf("FIMC queue requires image plane(s) and a distinct metadata plane")
	}
	if len(extents) != 0 && len(extents) != planes {
		return nil, fmt.Errorf("invalid negotiated extent count")
	}
	q.extents = append([]uint32(nil), extents...)
	if len(q.extents) == 0 {
		for _, a := range buffers[0] {
			if a == nil {
				return nil, fmt.Errorf("nil DMA allocation")
			}
			q.extents = append(q.extents, uint32(a.size))
		}
	}
	for _, b := range buffers {
		if len(b) != planes {
			return nil, fmt.Errorf("inconsistent FIMC plane count")
		}
		for j, a := range b {
			if a == nil || a.closed || a.pins != 0 || a.size <= 0 || seen[a] {
				return nil, fmt.Errorf("invalid or aliased DMA pool")
			}
			seen[a] = true
			if q.extents[j] == 0 || uint64(q.extents[j]) > uint64(a.size) {
				return nil, fmt.Errorf("negotiated extent outside DMA allocation")
			}
			if j == planes-1 && int(q.extents[j]) < minimum {
				return nil, fmt.Errorf("metadata extent shorter than selected ABI")
			}
		}
		q.slots = append(q.slots, slot{planes: append([]*Allocation(nil), b...), used: make([]uint32, planes), offsets: make([]uint32, planes)})
	}
	return q, nil
}
func (q *Queue) healthy() error {
	if q.closed {
		return fmt.Errorf("closed FIMC queue")
	}
	if q.poison {
		return ErrQuarantined
	}
	return nil
}

// Submit resets a COMPLETE source-derived request into a separate metadata plane
// and pins every import before QBUF. No zero buffer with only a marker is sent.
// Values are validated in a scratch request before any live mapping is modified.
func (q *Queue) Submit(index uint32, t *fimcshot.Template, g fimcshot.Group, c fimcshot.Controls, l fimcshot.Limits) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e := q.healthy(); e != nil {
		return e
	}
	if q.role != ShotMetadata {
		return fmt.Errorf("shot submission on capture queue")
	}
	if int(index) >= len(q.slots) {
		return fmt.Errorf("FIMC index outside pool")
	}
	s := &q.slots[index]
	if s.state != idle {
		return ErrBusy
	}
	var request [fimcshot.Size]byte
	v, e := t.Reset(request[:])
	if e != nil {
		return e
	}
	if e = v.SetGroup(g); e != nil {
		return e
	}
	if e = v.ApplyControls(c, l); e != nil {
		return e
	}
	return q.queueLocked(index, s, request[:])
}

// SubmitCapture lends an empty result plane to the driver. No shot template,
// controls or marker is written to camera2_stream capture nodes.
func (q *Queue) SubmitCapture(index uint32) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e := q.healthy(); e != nil {
		return e
	}
	if q.role != StreamMetadata {
		return fmt.Errorf("capture submission on shot queue")
	}
	if int(index) >= len(q.slots) {
		return fmt.Errorf("FIMC index outside pool")
	}
	s := &q.slots[index]
	if s.state != idle {
		return ErrBusy
	}
	return q.queueLocked(index, s, nil)
}

// SubmitForward submits a real upstream Bayer result to the next M2M leader.
// It preserves the upstream shot's dynamic metadata (not a fresh synthetic
// request) and changes only the destination node group. Pixel extents must match
// the negotiated downstream layout; no upscale, format conversion or padding
// is invented here. Validation finishes before any live DMA plane is touched.
func (q *Queue) SubmitForward(index uint32, pixels [][]byte, shot []byte, g fimcshot.Group) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e := q.healthy(); e != nil {
		return e
	}
	if q.role != ShotMetadata || int(index) >= len(q.slots) {
		return fmt.Errorf("invalid M2M request target")
	}
	s := &q.slots[index]
	if s.state != idle {
		return ErrBusy
	}
	if len(pixels) != len(s.planes)-1 {
		return fmt.Errorf("M2M image plane count mismatch")
	}
	for i, p := range pixels {
		if len(p) != int(q.extents[i]) {
			return fmt.Errorf("M2M image extent mismatch")
		}
	}
	if len(shot) != fimcshot.Size {
		return fmt.Errorf("M2M shot extent mismatch")
	}
	var request [fimcshot.Size]byte
	copy(request[:], shot)
	v, e := fimcshot.Bind(request[:])
	if e != nil {
		return e
	}
	if _, e = v.DynamicFrameCount(); e != nil {
		return e
	}
	ts, e := v.SensorTimestampNS()
	if e != nil || ts == 0 {
		return fmt.Errorf("M2M source has no sensor timestamp")
	}
	if e = v.SetGroup(g); e != nil {
		return e
	}
	return q.queuePixelsLocked(index, s, request[:], pixels)
}

// NextIdle is only a scheduling hint. Submit/SubmitForward recheck ownership
// under the same queue lock; callers cannot turn an in-flight buffer into idle.
func (q *Queue) NextIdle() (uint32, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e := q.healthy(); e != nil {
		return 0, e
	}
	ownership.Lock()
	defer ownership.Unlock()
	for i := range q.slots {
		ready := q.slots[i].state == idle
		for _, a := range q.slots[i].planes {
			ready = ready && !a.closed && a.pins == 0
		}
		if ready {
			return uint32(i), nil
		}
	}
	return 0, syscall.EAGAIN
}

func (q *Queue) QueuedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, s := range q.slots {
		if s.state == driverOwned {
			n++
		}
	}
	return n
}

// Immutable after construction. Shared imports use the same slot as the RAW
// capture, never an arbitrary idle slot with a different image fd.
func (q *Queue) SharesRawFrom(source *Queue) bool { return q.sharedRaw != nil && q.sharedRaw == source }
func (q *Queue) BufferCount() int                 { q.mu.Lock(); defer q.mu.Unlock(); return len(q.slots) }
func (q *Queue) MetadataRole() MetadataRole       { return q.role }

func (q *Queue) queueLocked(index uint32, s *slot, request []byte) error {
	return q.queuePixelsLocked(index, s, request, nil)
}
func (q *Queue) queuePixelsLocked(index uint32, s *slot, request []byte, pixels [][]byte) error {
	ownership.Lock()
	for _, a := range s.planes {
		if a.closed || a.pins != 0 {
			ownership.Unlock()
			return ErrBusy
		}
	}
	for _, a := range s.planes {
		a.pins++
	}
	// Uncached ION mappings; no Linux-4.x-only DMA_BUF_IOCTL_SYNC is invented for
	// the pinned 3.18 kernel. The graph must retain its vb2 cache prepare/finish.
	metadata := s.planes[len(s.planes)-1].data[:q.extents[len(q.extents)-1]]
	for i, p := range pixels {
		copy(s.planes[i].data[:q.extents[i]], p)
	}
	clear(metadata) // No stale stream validity/dynamic data survives requeue.
	copy(metadata, request)
	s.flags = 0
	clear(s.used)
	clear(s.offsets)
	ownership.Unlock()
	return q.enqueuePinnedLocked(index, s)
}

// Called with queue lock held and imports already pinned under ownership.
func (q *Queue) enqueuePinnedLocked(index uint32, s *slot) error {
	pp := make([]Plane, len(s.planes))
	for i, a := range s.planes {
		pp[i] = Plane{a.fd, uint32(a.size)}
	}
	s.lease = true
	s.state = driverOwned
	q.next++
	s.serial = q.next
	q.touched = true
	if e := q.io.Queue(index, pp); e != nil {
		if errors.Is(e, syscall.EAGAIN) {
			q.unpin(s)
			s.state = idle
			return e
		}
		// Even a partially failed vendor QBUF is retained until STREAMOFF, rather
		// than trusting a helper that frees imported memory during driver unwind.
		q.poison = true
		return errors.Join(e, ErrQuarantined)
	}
	return nil
}
func (q *Queue) unpin(s *slot) {
	ownership.Lock()
	defer ownership.Unlock()
	if s.lease {
		for _, a := range s.planes {
			a.pins--
		}
		s.lease = false
	}
}

// CanStartEmpty is an immutable capability from the verified leader S_INPUT/S_FMT
// handshake, never a caller-selected bypass for arbitrary unprimed queues.
func (q *Queue) CanStartEmpty() bool {
	n, ok := q.io.(*PreparedNode)
	return ok && n.emptyLeaderStart
}

func (q *Queue) Start() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e := q.healthy(); e != nil {
		return e
	}
	if q.started {
		return nil
	}
	n := 0
	for _, s := range q.slots {
		if s.state == driverOwned {
			n++
		}
	}
	if n == 0 && !q.CanStartEmpty() {
		return fmt.Errorf("no queued FIMC buffers")
	}
	q.started = true
	if e := q.io.Start(); e != nil {
		q.poison = true
		return errors.Join(e, ErrQuarantined)
	}
	return nil
}
func (q *Queue) Dequeue() (Frame, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if e := q.healthy(); e != nil {
		return Frame{}, e
	}
	if !q.started {
		return Frame{}, fmt.Errorf("FIMC queue not started")
	}
	c, e := q.io.Dequeue()
	if e != nil {
		if !errors.Is(e, syscall.EAGAIN) && !errors.Is(e, syscall.EINTR) {
			q.poison = true
			return Frame{}, errors.Join(e, ErrQuarantined)
		}
		return Frame{}, e
	}
	invalid := func() (Frame, error) {
		q.poison = true
		return Frame{}, fmt.Errorf("malformed/duplicate FIMC completion: %w", ErrQuarantined)
	}
	if int(c.Index) >= len(q.slots) {
		return invalid()
	}
	s := &q.slots[c.Index]
	if s.state != driverOwned || !s.lease || len(c.Lengths) != len(s.planes) || len(c.Used) != len(s.planes) || len(c.Offsets) != len(s.planes) {
		return invalid()
	}
	for i, a := range s.planes {
		if c.Lengths[i] != uint32(a.size) || c.Used[i] > c.Lengths[i] || c.Offsets[i] > c.Used[i] {
			return invalid()
		}
	}
	// Driver error flag is a returned (owned by CPU) bad frame, not permission to
	// guess pixels. The caller sees it and may drop/release the frame explicitly.
	q.unpin(s)
	copy(s.used, c.Used)
	copy(s.offsets, c.Offsets)
	s.flags = c.Flags
	s.state = completed
	return Frame{Index: c.Index, queue: q, serial: s.serial, epoch: q.epoch, Flags: c.Flags}, nil
}
func (q *Queue) frame(f Frame) (*slot, error) {
	if e := q.healthy(); e != nil {
		return nil, e
	}
	if f.queue != q || f.epoch != q.epoch || int(f.Index) >= len(q.slots) {
		return nil, fmt.Errorf("stale/foreign FIMC frame")
	}
	s := &q.slots[f.Index]
	if s.state != completed || s.serial != f.serial {
		return nil, fmt.Errorf("stale/already released FIMC frame")
	}
	return s, nil
}

// WithFrameCPU exposes borrowed planes only within fn. fn must not retain or
// reenter this package. Successful DQBUF is required; no polling a live DMA map.
func (q *Queue) WithFrameCPU(f Frame, fn func([][]byte) error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	s, e := q.frame(f)
	if e != nil {
		return e
	}
	if fn == nil {
		return fmt.Errorf("nil FIMC frame callback")
	}
	if s.flags&0x40 != 0 {
		return fmt.Errorf("FIMC driver returned an error frame")
	}
	ownership.Lock()
	defer ownership.Unlock()
	b, e := q.viewsLocked(s)
	if e != nil {
		return e
	}
	return fn(b)
}

// viewsLocked requires BOTH q.mu and ownership. No DMA data escapes those
// locks except through a borrowed callback with explicit no-reentry rules.
func (q *Queue) viewsLocked(s *slot) ([][]byte, error) {
	if s.flags&0x40 != 0 {
		return nil, fmt.Errorf("FIMC driver returned an error frame")
	}
	b := make([][]byte, len(s.planes))
	for i, a := range s.planes {
		if a.closed || a.pins != 0 {
			return nil, ErrBusy
		}
		// Some pinned FIMC nodes leave bytesused=0. In that case use only
		// negotiated sizeimage, NEVER allocation padding. Nonzero payload bounds
		// from DQBUF must be respected, including metadata data_offset.
		end := s.used[i]
		if end == 0 {
			end = q.extents[i]
		}
		if end > q.extents[i] || s.offsets[i] > end {
			return nil, fmt.Errorf("payload exceeds negotiated FIMC extent: plane=%d used=%d offset=%d sizeimage=%d allocation=%d", i, end, s.offsets[i], q.extents[i], a.size)
		}
		b[i] = a.data[s.offsets[i]:end]
	}
	metadata := b[len(b)-1]
	switch q.role {
	case ShotMetadata:
		if len(metadata) < fimcshot.Size {
			return nil, fmt.Errorf("short returned shot")
		}
		if _, e := fimcshot.Bind(metadata[:fimcshot.Size]); e != nil {
			return nil, e
		}
	case StreamMetadata:
		if _, e := fimcshot.ReadStream(metadata); e != nil {
			return nil, e
		}
	}
	return b, nil
}

// ForwardCompletedTo imports an already completed RAW plane when the pools were
// linked at construction, or copies for independent pools. It never exposes a source mapping to a nested
// WithFrameCPU callback (which would deadlock on the global ownership lock).
// The source remains held until ReleaseFrame; a failed QBUF quarantines ONLY
// destination imports. No queue fd is closed and no USB rebind is attempted.
func (q *Queue) ForwardCompletedTo(f Frame, dst *Queue, index uint32, shot []byte, g fimcshot.Group) error {
	if dst == nil || q == dst {
		return fmt.Errorf("distinct M2M queues required")
	}
	first, second := q, dst
	if first.id > second.id {
		first, second = second, first
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	second.mu.Lock()
	defer second.mu.Unlock()
	src, e := q.frame(f)
	if e != nil {
		return e
	}
	if e = dst.healthy(); e != nil {
		return e
	}
	if q.role != StreamMetadata || dst.role != ShotMetadata || int(index) >= len(dst.slots) {
		return fmt.Errorf("wrong M2M queue roles/index")
	}
	target := &dst.slots[index]
	shared := dst.SharesRawFrom(q)
	if shared && index != f.Index {
		return fmt.Errorf("shared RAW forwarding requires matching slot")
	}
	if target.state != idle {
		return ErrBusy
	}
	if len(shot) != fimcshot.Size {
		return fmt.Errorf("M2M shot extent mismatch")
	}
	var request [fimcshot.Size]byte
	copy(request[:], shot)
	v, e := fimcshot.Bind(request[:])
	if e != nil {
		return e
	}
	n, e := v.DynamicFrameCount()
	if e != nil {
		return e
	}
	ts, e := v.SensorTimestampNS()
	if e != nil || ts == 0 {
		return fmt.Errorf("missing M2M sensor timestamp")
	}
	if e = v.SetGroup(g); e != nil {
		return e
	}
	// Validate/copy under one ownership lock. No writer can pin an aliased
	// source in the interval between metadata validation and RAW copy.
	prepare := func() error {
		ownership.Lock()
		defer ownership.Unlock()
		pixels, e := q.viewsLocked(src)
		if e != nil {
			return e
		}
		stream, e := fimcshot.ReadStream(pixels[len(pixels)-1])
		if e != nil {
			return e
		}
		if stream.FrameCount != n {
			return fmt.Errorf("RAW/shot frame count mismatch")
		}
		if len(pixels) != len(target.planes) {
			return fmt.Errorf("M2M plane count mismatch")
		}
		for i, a := range target.planes {
			if a.closed || a.pins != 0 {
				return ErrBusy
			}
			if shared && i < len(pixels)-1 {
				if a != src.planes[i] || src.offsets[i] != 0 {
					return fmt.Errorf("shared RAW plane identity/offset changed")
				}
			} else {
				for _, s := range src.planes {
					if a == s {
						return fmt.Errorf("aliased M2M imports")
					}
				}
			}
			if i < len(pixels)-1 && len(pixels[i]) != int(dst.extents[i]) {
				return fmt.Errorf("M2M raw extent mismatch")
			}
		}
		if !shared {
			for i := 0; i < len(pixels)-1; i++ {
				copy(target.planes[i].data[:dst.extents[i]], pixels[i])
			}
		}
		meta := target.planes[len(target.planes)-1].data[:dst.extents[len(dst.extents)-1]]
		clear(meta)
		copy(meta, request[:])
		clear(target.used)
		clear(target.offsets)
		target.flags = 0
		for _, a := range target.planes {
			a.pins++
		}
		return nil
	}
	if e = prepare(); e != nil {
		return e
	}
	return dst.enqueuePinnedLocked(index, target)
}

func (q *Queue) ReleaseFrame(f Frame) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	s, e := q.frame(f)
	if e != nil {
		return e
	}
	s.state = idle
	// A downstream import may still pin this image. NextIdle and submission
	// both check those pins; ReleaseFrame retires only this queue's CPU handle.
	return nil
}

// Export holds image allocations across the synchronous graph callback. The
// queue's metadata stays private; NextIdle cannot recycle pinned image planes.
func (q *Queue) ExportFrame(f Frame, width, height int, pts int64, strides []uint32, color media.ColorDescription) (*media.FrameLease, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	s, err := q.frame(f)
	if err != nil {
		return nil, err
	}
	if len(strides) != len(s.planes)-1 {
		return nil, fmt.Errorf("DMA lease stride count mismatch")
	}
	ownership.Lock()
	defer ownership.Unlock()
	if _, err = q.viewsLocked(s); err != nil {
		return nil, err
	}
	allocations := append([]*Allocation(nil), s.planes[:len(strides)]...)
	planes := make([]media.DMAPlane, len(allocations))
	for i, a := range allocations {
		planes[i] = media.DMAPlane{FD: a.fd, Length: uint32(a.size), Offset: s.offsets[i], Stride: strides[i]}
	}
	release := func() error {
		ownership.Lock()
		defer ownership.Unlock()
		for _, a := range allocations {
			if a.pins <= 0 {
				return fmt.Errorf("lost exported DMA pin")
			}
		}
		for _, a := range allocations {
			a.pins--
		}
		q.exported.Add(-1)
		select {
		case q.exportDone <- struct{}{}:
		default:
		}
		return nil
	}
	l, err := media.NewFrameLease(1<<63|q.id<<32|uint64(f.Index+1), q.epoch, width, height, pts, color, planes, release)
	if err != nil {
		return nil, err
	}
	for _, a := range allocations {
		a.pins++
	}
	q.exported.Add(1)
	return l, nil
}
func (q *Queue) stop() error {
	if q.closed {
		return nil
	}
	if q.touched || q.started || q.poison {
		if e := q.io.Stop(); e != nil {
			q.poison = true
			return errors.Join(e, ErrQuarantined)
		}
	}
	for i := range q.slots {
		q.unpin(&q.slots[i])
		q.slots[i].state = idle
	}
	q.epoch++
	q.started = false
	q.touched = false
	q.poison = false
	return nil
}
func (q *Queue) Stop() error { q.mu.Lock(); defer q.mu.Unlock(); return q.stop() }

// Close does not unmap a single DMA plane while STREAMOFF ownership is unknown.
// Allocations are independently closed by the graph AFTER every node is stopped.
func (q *Queue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	if e := q.stop(); e != nil {
		return e
	}
	if q.exported.Load() != 0 {
		timer := time.NewTimer(500 * time.Millisecond)
		defer timer.Stop()
		for q.exported.Load() != 0 {
			select {
			case <-q.exportDone:
			case <-timer.C:
				return fmt.Errorf("exported FIMC frames still held: %w", ErrQuarantined)
			}
		}
	}
	if e := q.io.Release(); e != nil {
		q.poison = true
		return e
	}
	q.closed = true
	return nil
}

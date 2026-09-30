package camera

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"perimode/native/internal/camera/fimcdma"
	"perimode/native/internal/media"
)

var ErrCaptureBusy = errors.New("camera source still belongs to another sensor/mode session")

// SharedProvider owns one sensor, with at most one USB and one local-preview
// subscriber. Preview-only operation never opens MFC or a USB endpoint.
// All DMA reads happen inside the underlying Source.Drain callback. Subscribers
// receive private bounded copies; neither callback retains a sensor DMA mapping.
type SharedProvider struct {
	controls     map[CaptureKey]*controlEntry
	backend      Provider
	ctx          context.Context
	mu           sync.Mutex
	session      *captureSession
	poisoned     bool
	closed       bool
	closeTimeout time.Duration
}

type CaptureKey struct {
	Sensor Sensor
	Mode   Mode
}

func Key(s Settings) CaptureKey { return CaptureKey{s.Sensor, s.Mode} }

type captureSession struct {
	owner    *SharedProvider
	settings Settings
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	readers  [2]*sharedReader // guarded by owner.mu; 0=USB, 1=preview
	stopping bool
	err      error  // read only after done closes
	retained Source // keep a failed-stop owner reachable; never silently reopen over it
}

type sharedReader struct {
	ready              chan struct{}
	closeOnce          sync.Once
	closeErr           error
	session            *captureSession
	role               int
	mu                 sync.Mutex
	drainMu            sync.Mutex
	slots              [3]media.Image
	pending, reading   int
	nextPending        int
	hasNextPending     bool
	closed             bool
	err                error
	dropped, delivered uint64
	nextPreview        time.Time
}

type ReaderStats struct {
	Dropped, Delivered uint64
	Buffered, Leased   int
}

func NewSharedProvider(ctx context.Context, backend Provider) *SharedProvider {
	if ctx == nil {
		ctx = context.Background()
	}
	return &SharedProvider{backend: backend, ctx: ctx, closeTimeout: 2 * time.Second}
}
func (p *SharedProvider) Available(s Settings) error {
	if e := s.Validate(); e != nil {
		return e
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return os.ErrClosed
	}
	if p.poisoned {
		p.mu.Unlock()
		return ErrOwnership
	}
	backend := p.backend
	p.mu.Unlock()
	if backend == nil {
		return ErrSensorGraph
	}
	return backend.Available(s)
}
func (p *SharedProvider) Open(s Settings) (Source, error) { return p.subscribe(s, 0) }
func (p *SharedProvider) OpenPreview(s Settings) (Source, error) {
	if p.USBTrialFPS() != 0 {
		return nil, fmt.Errorf("PiP is disabled in the exclusive USB trial build")
	}
	return p.subscribe(s, 1)
}
func (p *SharedProvider) subscribe(s Settings, role int) (Source, error) {
	if e := p.Available(s); e != nil {
		return nil, e
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, os.ErrClosed
	}
	if p.poisoned {
		return nil, ErrOwnership
	}
	if e := p.ctx.Err(); e != nil {
		return nil, e
	}
	cs := p.session
	if cs != nil {
		select {
		case <-cs.done:
			if captureOwnershipUncertain(cs.err) {
				p.poisoned = true
				return nil, ErrOwnership
			}
			// Existing readers must observe the terminal result and release their leases.
			if cs.readers[0] != nil || cs.readers[1] != nil {
				return nil, ErrCaptureBusy
			}
			p.session = nil
			cs = nil
		default:
		}
	}
	if cs != nil && (cs.stopping || Key(cs.settings) != Key(s) || cs.readers[role] != nil) {
		return nil, ErrCaptureBusy
	}
	fresh := cs == nil
	if fresh {
		ctx, cancel := context.WithCancel(p.ctx)
		cs = &captureSession{owner: p, settings: s, ctx: ctx, cancel: cancel, done: make(chan struct{})}
		p.session = cs
	}
	r := &sharedReader{session: cs, role: role, pending: -1, reading: -1}
	cs.readers[role] = r
	if fresh {
		go cs.run()
	}
	return r, nil
}
func (s *captureSession) readersSnapshot() [2]*sharedReader {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	return s.readers
}
func (s *captureSession) run() {
	var source Source
	var result error
	defer func() {
		// Retire subscriber leases before closing the sensor's DMA pools.
		terminal := result
		if terminal == nil {
			terminal = context.Canceled
		}
		for _, r := range s.readersSnapshot() {
			if r != nil {
				r.fail(terminal)
				r.drainMu.Lock()
				r.drainMu.Unlock()
			}
		}
		var closeFailure error
		if source != nil {
			closeFailure = source.Close()
			if closeFailure != nil {
				result = errors.Join(result, closeFailure, ErrOwnership)
			}
		}
		s.owner.mu.Lock()
		if closeFailure != nil {
			s.retained = source
		}
		if captureOwnershipUncertain(result) {
			s.owner.poisoned = true
		}
		s.stopping = true
		if entry := s.owner.controls[Key(s.settings)]; entry != nil {
			entry.trigger = 0
			entry.selection = entry.selection.Persistent()
			entry.accepted = 0
			entry.attempted = 0
		}
		readers := s.readers
		s.err = result
		s.owner.mu.Unlock()
		terminal = result
		if terminal == nil {
			terminal = context.Canceled
		}
		for _, r := range readers {
			if r != nil {
				r.fail(terminal)
			}
		}
		close(s.done)
	}()
	if s.ctx.Err() != nil {
		return
	}
	var descriptor *ControlDescriptor
	if _, ok := s.owner.backend.(ControlDescriptorProvider); ok {
		d, e := s.owner.DescribeControls(s.settings)
		if e != nil {
			result = e
			return
		}
		s.owner.mu.Lock()
		entry := s.owner.controlLocked(Key(s.settings))
		entry.accepted = 0
		entry.attempted = 0
		entry.sensor = media.SensorResult{}
		entry.sensorAt = time.Time{}
		_, e = entry.selection.resolve(d)
		s.owner.mu.Unlock()
		if e != nil {
			result = fmt.Errorf("saved camera controls: %w", e)
			return
		}
		descriptor = &d
	}
	source, result = s.owner.backend.Open(s.settings)
	if result != nil {
		return
	}
	if source == nil {
		result = fmt.Errorf("camera backend returned nil source without error")
		return
	}
	waiter, eventWait := source.(interface{ Wait(context.Context) error })
	last := time.Now()
	var pts int64
	havePTS := false
	stalePTS := 0
	for s.ctx.Err() == nil {
		if result = s.applyControls(source, descriptor); result != nil {
			return
		}
		_, result = source.Drain(func(im media.Image) error {
			if e := s.ctx.Err(); e != nil {
				return e
			}
			if e := validateImage(im, int(s.settings.Mode.Width), int(s.settings.Mode.Height)); e != nil {
				return e
			}
			if havePTS && im.PTS <= pts {
				stalePTS++
				if stalePTS == 1 {
					log.Printf("S7 camera dropped stale timestamp: current=%d previous=%d", im.PTS, pts)
				}
				if stalePTS >= 8 {
					return fmt.Errorf("shared capture: eight nonmonotonic sensor timestamps")
				}
				return nil
			}
			stalePTS = 0
			pts = im.PTS
			havePTS = true
			last = time.Now()
			if descriptor != nil && im.Lease != nil {
				s.owner.mu.Lock()
				entry := s.owner.controlLocked(Key(s.settings))
				entry.sensor, entry.sensorAt = im.Lease.Sensor, last
				s.owner.mu.Unlock()
			}
			for _, r := range s.readersSnapshot() {
				if r != nil {
					r.offer(im, last)
				}
			}
			return nil
		})
		if result != nil && !wouldBlock(result) {
			if s.ctx.Err() != nil && errors.Is(result, context.Canceled) {
				result = nil
			}
			return
		}
		result = nil
		if time.Since(last) > 3*time.Second {
			result = fmt.Errorf("shared camera sensor produced no new frame for 3 seconds")
			return
		}
		if eventWait {
			result = waiter.Wait(s.ctx)
		} else {
			result = waitCaptureTick(s.ctx)
		}
		if result != nil {
			if errors.Is(result, context.Canceled) {
				result = nil
			}
			return
		}
	}
}

func waitCaptureTick(ctx context.Context) error {
	timer := time.NewTimer(2 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (r *sharedReader) fail(e error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = e
	r.clearPendingLocked()
	r.signalLocked()
}

func (r *sharedReader) signalLocked() {
	select {
	case r.ready <- struct{}{}:
	default:
	}
}

// The subscriber wakes on an actual new frame. The mailbox still contains
// only the newest frame; notifications never carry or copy image data.
func (r *sharedReader) Ready() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ready == nil {
		r.ready = make(chan struct{}, 1)
	}
	if r.pending >= 0 || r.err != nil || r.closed {
		r.signalLocked()
	}
	return r.ready
}

func (r *sharedReader) releaseSlotLocked(index int) {
	if index >= 0 && r.slots[index].Lease != nil {
		if err := r.slots[index].Lease.Release(); err != nil {
			r.err = errors.Join(r.err, err)
		}
		r.slots[index] = media.Image{}
	}
}
func (r *sharedReader) popPendingLocked() int {
	i := r.pending
	if r.hasNextPending {
		r.pending, r.hasNextPending = r.nextPending, false
	} else {
		r.pending = -1
	}
	return i
}
func (r *sharedReader) clearPendingLocked() {
	for r.pending >= 0 {
		r.releaseSlotLocked(r.popPendingLocked())
	}
}
func (r *sharedReader) offer(im media.Image, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.err != nil {
		return
	}
	// Preview is independent of encoded FPS. Its scaling/rotation/color work is
	// performed by Mali; native subscribers retain the same DMA allocation.
	if r.role == 1 && r.session != nil && r.session.settings.Mode.FPS > 30 && now.Before(r.nextPreview) {
		r.dropped++
		return
	}
	// Two pending USB frames absorb a high-speed completion burst. Preview and
	// normal-rate capture retain their one-latest-frame policy. Never block DMA.
	ordered := r.role == 0 && r.session != nil && r.session.settings.Mode.FPS > 60
	if r.pending >= 0 && (!ordered || r.hasNextPending) {
		r.dropped++
		r.releaseSlotLocked(r.popPendingLocked())
	}
	index := 0
	for index == r.reading || index == r.pending {
		index++
	}
	w, h := im.Width, im.Height
	slot := &r.slots[index]
	if im.Lease != nil {
		if err := im.Lease.Retain(); err != nil {
			r.err = err
			return
		}
		*slot = im
		r.enqueueLocked(index)
		r.signalLocked()
		if r.role == 1 {
			r.advancePreviewLocked(now)
		}
		return
	}
	if len(slot.Y) != w*h {
		data := make([]byte, w*h*3/2)
		*slot = media.Image{Width: w, Height: h, StrideY: w, StrideUV: w, Y: data[:w*h], UV: data[w*h:]}
	}
	if im.StrideY == w {
		copy(slot.Y, im.Y[:w*h])
	} else {
		for y := 0; y < h; y++ {
			copy(slot.Y[y*w:(y+1)*w], im.Y[y*im.StrideY:y*im.StrideY+w])
		}
	}
	if im.StrideUV == w {
		copy(slot.UV, im.UV[:w*h/2])
	} else {
		for y := 0; y < h/2; y++ {
			copy(slot.UV[y*w:(y+1)*w], im.UV[y*im.StrideUV:y*im.StrideUV+w])
		}
	}
	if r.role == 1 {
		r.advancePreviewLocked(now)
	}
	slot.PTS = im.PTS
	r.enqueueLocked(index)
	r.signalLocked()
}

func (r *sharedReader) advancePreviewLocked(now time.Time) {
	const period = time.Second / 30
	// Keep the cadence phase when a rounded source period misses one deadline.
	// Reset after a real pause instead of emitting a catch-up burst.
	if r.nextPreview.IsZero() || now.Sub(r.nextPreview) >= period {
		r.nextPreview = now.Add(period)
	} else {
		r.nextPreview = r.nextPreview.Add(period)
	}
}
func (r *sharedReader) enqueueLocked(index int) {
	if r.pending < 0 {
		r.pending = index
	} else {
		r.nextPending, r.hasNextPending = index, true
	}
}
func (r *sharedReader) Stats() ReaderStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := ReaderStats{Dropped: r.dropped, Delivered: r.delivered}
	if r.pending >= 0 {
		v.Buffered = 1
	}
	if r.hasNextPending {
		v.Buffered++
	}
	if r.reading >= 0 {
		v.Leased = 1
	}
	return v
}
func (r *sharedReader) Drain(emit func(media.Image) error) (int, error) {
	if emit == nil {
		return 0, fmt.Errorf("nil camera subscriber callback")
	}
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, os.ErrClosed
	}
	if r.err != nil {
		e := r.err
		r.mu.Unlock()
		return 0, e
	}
	if r.pending < 0 {
		r.mu.Unlock()
		return 0, nil
	}
	i := r.popPendingLocked()
	r.reading = i
	im := r.slots[i]
	r.mu.Unlock()
	// Release the subscriber reference even when its callback panics.
	defer func() { r.mu.Lock(); r.releaseSlotLocked(i); r.reading = -1; r.mu.Unlock() }()
	if e := emit(im); e != nil {
		return 0, e
	}
	r.mu.Lock()
	r.delivered++
	if r.pending >= 0 {
		r.signalLocked()
	}
	r.mu.Unlock()
	return 1, nil
}
func (r *sharedReader) Close() error {
	r.closeOnce.Do(func() { r.closeErr = r.close() })
	return r.closeErr
}
func (r *sharedReader) close() error {
	r.drainMu.Lock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		r.drainMu.Unlock()
		return nil
	}
	r.closed = true
	r.signalLocked()
	r.clearPendingLocked()
	r.mu.Unlock()
	r.drainMu.Unlock()
	p, s := r.session.owner, r.session
	p.mu.Lock()
	if s.readers[r.role] == r {
		s.readers[r.role] = nil
	}
	last := s.readers[0] == nil && s.readers[1] == nil
	if last {
		s.stopping = true
		s.cancel()
	}
	p.mu.Unlock()
	if !last {
		return nil
	}
	select {
	case <-s.done:
		if captureOwnershipUncertain(s.err) {
			p.mu.Lock()
			p.poisoned = true
			p.mu.Unlock()
		}
		return s.err
	case <-time.After(p.closeTimeout):
		p.mu.Lock()
		p.poisoned = true
		p.mu.Unlock()
		return fmt.Errorf("shared sensor teardown timeout: %w", ErrOwnership)
	}
}
func (p *SharedProvider) Close() error {
	p.mu.Lock()
	p.closed = true
	s := p.session
	if s != nil {
		s.stopping = true
		s.cancel()
	}
	p.mu.Unlock()
	if s == nil {
		return nil
	}
	select {
	case <-s.done:
		return s.err
	case <-time.After(p.closeTimeout):
		p.mu.Lock()
		p.poisoned = true
		p.mu.Unlock()
		return fmt.Errorf("shared sensor shutdown timeout: %w", ErrOwnership)
	}
}

func captureOwnershipUncertain(err error) bool {
	return errors.Is(err, ErrOwnership) || errors.Is(err, media.ErrQuarantined) || errors.Is(err, fimcdma.ErrQuarantined)
}

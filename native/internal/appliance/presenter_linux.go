//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"perimode/native/internal/media"
	"perimode/native/pkg/monitor"
)

const monitorPictureBytes = 1280 * 720 * 3 / 2
const monitorPresenterMaxAge = 33334 * time.Microsecond
const menuBackdropMaxAge = time.Second

type presentationFrame struct {
	dma        media.Image
	pixels     []byte
	generation uint32
	pts        int64
	queued     time.Time
	slot       int
}

func (f *presentationFrame) image() media.Image {
	if f.dma.Lease != nil {
		return f.dma
	}
	return media.Image{Width: 1280, Height: 720, StrideY: 1280, StrideUV: 1280,
		Y: f.pixels[:1280*720], UV: f.pixels[1280*720:], PTS: f.pts}
}

type PresentationStats struct {
	DMAFrames     uint64 `json:"dma_frames"`
	CopiedBytes   uint64 `json:"cpu_copied_bytes"`
	LeaseError    string `json:"lease_error,omitempty"`
	Offered       uint64 `json:"offered"`
	Replaced      uint64 `json:"replaced_pending"`
	Taken         uint64 `json:"taken"`
	Invalidated   uint64 `json:"invalidated_by_session"`
	Expired       uint64 `json:"expired_before_present"`
	Pending       bool   `json:"pending"`
	PendingFrames int    `json:"pending_frames"`
	Active        bool   `json:"presenter_owns_frame"`
	ResidentBytes int    `json:"owned_nv12_bytes"`
	Closed        bool   `json:"closed"`
}

// Native DMA has one presenter-owned and two ordered pending leases. A full
// mailbox replaces its oldest pending frame; age remains bounded independently.
// Native production requires DMA from the first frame. Raw reference backends
// allocate CPU storage lazily; compressed H.264 pictures are never coalesced.
type frameMailbox struct {
	mu              sync.Mutex
	slots           [3]presentationFrame
	pending, active int
	nextPending     int
	hasNextPending  bool
	newest          int
	notify          chan struct{}
	done            chan struct{}
	closed          bool
	nativeDMA       bool
	stats           PresentationStats
}

func newFrameMailbox() *frameMailbox {
	q := &frameMailbox{pending: -1, active: -1, newest: -1, notify: make(chan struct{}, 1), done: make(chan struct{})}
	for i := range q.slots {
		q.slots[i] = presentationFrame{slot: i}
	}
	return q
}
func (q *frameMailbox) publish(im media.Image, gen uint32, now time.Time) error {
	if gen == 0 || im.PTS < 0 || !monitor.ValidMode(uint32(im.Width), uint32(im.Height)) || im.StrideY < im.Width || im.StrideUV < im.Width || im.StrideY > 16384 || im.StrideUV > 16384 ||
		len(im.Y) < (im.Height-1)*im.StrideY+im.Width || len(im.UV) < (im.Height/2-1)*im.StrideUV+im.Width {
		return fmt.Errorf("invalid decoded image for bounded presenter")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return os.ErrClosed
	}
	ordered := q.nativeDMA || im.Lease != nil
	replacing := q.pending >= 0 && (!ordered || q.hasNextPending)
	i := q.pending
	if i < 0 || (ordered && !replacing) {
		i = 0
		for i == q.active || i == q.pending {
			i++
		}
	}
	f := &q.slots[i]
	if im.Lease != nil {
		if err := im.Lease.Retain(); err != nil {
			return err
		}
		q.releasePixelsLocked(f)
		f.dma = im
		f.pixels = nil
		q.nativeDMA = true
		q.stats.DMAFrames++
	} else if q.nativeDMA || im.Width != 1280 || im.Height != 720 {
		return fmt.Errorf("monitor DMA stream lost its lease; CPU copy refused")
	} else {
		if len(f.pixels) != monitorPictureBytes {
			f.pixels = make([]byte, monitorPictureBytes)
		}
		// AMediaCodec returns dense NV12; copy entire planes instead of 1080
		// row operations. Padded/native-MFC inputs retain the exact row path.
		if im.StrideY == 1280 {
			copy(f.pixels[:1280*720], im.Y[:1280*720])
		} else {
			for y := 0; y < 720; y++ {
				copy(f.pixels[y*1280:(y+1)*1280], im.Y[y*im.StrideY:y*im.StrideY+1280])
			}
		}
		if im.StrideUV == 1280 {
			copy(f.pixels[1280*720:], im.UV[:1280*360])
		} else {
			for y := 0; y < 360; y++ {
				copy(f.pixels[1280*720+y*1280:1280*720+(y+1)*1280], im.UV[y*im.StrideUV:y*im.StrideUV+1280])
			}
		}
		q.stats.CopiedBytes += monitorPictureBytes
	}
	f.generation, f.pts, f.queued = gen, im.PTS, now
	if replacing {
		q.stats.Replaced++
		if ordered {
			q.pending = q.nextPending
			q.hasNextPending = false
		}
	}
	if ordered && q.pending >= 0 {
		q.nextPending = i
		q.hasNextPending = true
	} else {
		q.pending = i
	}
	q.newest = i
	q.stats.Offered++
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return nil
}

// snapshotLatest is used only when opening the menu. The bounded copy lets the
// blur renderer release the mailbox immediately and never read framebuffer RAM.
func presentationFresh(queued, now time.Time) bool {
	return !queued.IsZero() && !now.Before(queued) && now.Sub(queued) <= monitorPresenterMaxAge
}

func (q *frameMailbox) snapshotLatest(generation uint32) (media.Image, bool) {
	return q.snapshotLatestAt(generation, time.Now())
}
func (q *frameMailbox) snapshotLatestAt(generation uint32, now time.Time) (media.Image, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.nativeDMA {
		return media.Image{}, false
	} // The menu uses the retained RGB scanout, not an NV12 copy.
	if q.closed || q.newest < 0 {
		return media.Image{}, false
	}
	f := &q.slots[q.newest]
	if generation == 0 || f.generation != generation || f.pts < 0 || f.queued.IsZero() || now.Before(f.queued) || now.Sub(f.queued) > menuBackdropMaxAge {
		return media.Image{}, false
	}
	pixels := append([]byte(nil), f.pixels...)
	return media.Image{Width: 1280, Height: 720, StrideY: 1280, StrideUV: 1280,
		Y: pixels[:1280*720], UV: pixels[1280*720:], PTS: f.pts}, true
}

// Single-consumer API. Native DMA keeps the acquired frame in order. The CPU
// reference path may replace it before any pixels have been used.
func (q *frameMailbox) latest(held *presentationFrame) *presentationFrame {
	q.mu.Lock()
	defer q.mu.Unlock()
	if held != nil && (q.active != held.slot || held != &q.slots[held.slot]) {
		panic("presenter owner mismatch")
	}
	if q.closed || q.pending < 0 {
		return held
	}
	if held != nil && q.nativeDMA {
		return held
	}
	if q.active >= 0 && held == nil {
		panic("second presenter consumer")
	}
	if held != nil {
		q.releasePixelsLocked(held)
	}
	q.active = q.pending
	if q.hasNextPending {
		q.pending = q.nextPending
		q.hasNextPending = false
	} else {
		q.pending = -1
	}
	q.stats.Taken++
	return &q.slots[q.active]
}
func (q *frameMailbox) release(f *presentationFrame, invalid bool) {
	if f == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active != f.slot || f != &q.slots[f.slot] {
		panic("presenter released unowned frame")
	}
	q.active = -1
	q.releasePixelsLocked(f)
	if invalid {
		q.stats.Invalidated++
	}
}
func (q *frameMailbox) releaseExpired(f *presentationFrame) {
	if f == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active != f.slot || f != &q.slots[f.slot] {
		panic("presenter expired unowned frame")
	}
	q.active = -1
	q.releasePixelsLocked(f)
	q.stats.Expired++
}
func (q *frameMailbox) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		if q.pending >= 0 {
			q.releasePixelsLocked(&q.slots[q.pending])
		}
		if q.hasNextPending {
			q.releasePixelsLocked(&q.slots[q.nextPending])
		}
		q.hasNextPending = false
		q.pending = -1
		close(q.done)
	}
}

// Caller holds UI.presentationMu, so an active slot cannot be inside VPP.
// Keep slot ownership until the presentation worker sees generation zero.
func (q *frameMailbox) invalidateAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.slots {
		q.slots[i].generation = 0
		q.releasePixelsLocked(&q.slots[i])
	}
	q.pending = -1
	q.hasNextPending = false
	q.newest = -1
}
func (q *frameMailbox) snapshot() PresentationStats {
	q.mu.Lock()
	defer q.mu.Unlock()
	v := q.stats
	v.Pending = q.pending >= 0
	if v.Pending {
		v.PendingFrames = 1
	}
	if q.hasNextPending {
		v.PendingFrames++
	}
	v.Active = q.active >= 0
	v.Closed = q.closed
	for i := range q.slots {
		v.ResidentBytes += len(q.slots[i].pixels)
	}
	return v
}

func (q *frameMailbox) releasePixelsLocked(f *presentationFrame) {
	if f.dma.Lease != nil {
		if err := f.dma.Lease.Release(); err != nil {
			q.stats.LeaseError = err.Error()
		}
		f.dma = media.Image{}
	}
}

func (u *UI) offerMonitorFrame(im media.Image, gen uint32) error {
	u.state.observeSyntheticDecoded(im, gen)
	if u.presentFrames != nil {
		return u.presentFrames.publish(im, gen, time.Now())
	}
	// Direct unit-test callers need no asynchronous worker. Production always
	// creates the mailbox before any workers or USB diagnostic callbacks start.
	if !u.presentationMu.TryLock() {
		return nil
	}
	defer u.presentationMu.Unlock()
	_, _, page := u.state.Current()
	if u.screen == nil || (page != "" && !u.screen.HasGlass()) {
		return nil
	}
	return u.presentMonitorFrame(im, gen)
}
func (u *UI) presentMonitorFrame(im media.Image, gen uint32) error {
	start := time.Now()
	presented, err := u.screen.PresentFrame(im)
	if err != nil {
		return err
	}
	if presented {
		u.state.mu.Lock()
		defer u.state.mu.Unlock()
		u.state.Blitted++
		if u.state.syntheticCounterOn.Load() && !u.state.syntheticCounter.observe(im, gen, time.Now(), true) {
			u.state.syntheticCounterOn.Store(false)
		}
		// A late old-generation copy must not confirm the newly reconnected host.
		if u.state.Generation == gen && u.state.Settings.Enabled && u.state.Consumer && !u.state.ThermalPaused {
			if im.PTS >= 0 {
				u.state.metrics.blitted(gen, uint64(im.PTS), start, time.Now())
			}
			u.state.LastBlit = time.Now()
			u.state.link.HadReply = true
		}
	}
	return nil
}
func (u *UI) presentationWorker(ctx context.Context) {
	q := u.presentFrames
	if q == nil {
		return
	}
	defer q.close()
	var held *presentationFrame
	defer func() { q.release(held, false) }()
	for {
		if ctx.Err() != nil {
			return
		}
		held = q.latest(held)
		if held == nil {
			select {
			case <-ctx.Done():
				return
			case <-q.done:
				return
			case <-q.notify:
			}
			continue
		}
		// This dedicated worker may sleep on the bounded presenter owner.
		// Input/USB never wait here; a 5 ms retry timer only added wakeups and lag.
		if !u.presentationMu.LockContext(ctx) {
			return
		}
		held = q.latest(held)
		if held == nil {
			u.presentationMu.Unlock()
			continue
		}
		now := time.Now()
		fresh := presentationFresh(held.queued, now)
		u.state.mu.Lock()
		valid := held.generation == u.state.Generation && u.state.Settings.Enabled && u.state.Consumer && !u.state.ThermalPaused
		liveGen := u.state.Generation
		page := u.state.Menu
		u.state.mu.Unlock()
		if u.screen != nil {
			u.screen.SetVideoGeneration(liveGen)
		}
		var err error
		if valid && fresh && u.screen != nil && (page == "" || u.screen.HasGlass()) {
			err = u.presentMonitorFrame(held.image(), held.generation)
		}
		u.presentationMu.Unlock()
		if fresh {
			q.release(held, !valid)
		} else {
			q.releaseExpired(held)
		}
		held = nil
		if err != nil && !errors.Is(err, os.ErrClosed) {
			u.state.Fault("monitor-presenter", err)
			u.state.SetConsumer(false)
			return
		}
	}
}
func (u *UI) liveGlassLines() []string {
	lines := []string{"MONITOR / LIVE GLASS", "LIVE BACKDROP / LOCAL BLUR ON S7"}
	if u.screen != nil {
		s := u.screen.LiveGlassStatus()
		lines = append(lines, "BACKEND: "+s.Backend, fmt.Sprintf("BACKGROUND: %d / REPLACED: %d", s.Committed, s.Coalesced),
			fmt.Sprintf("COMPOSE: %.2f MS / MAX %.2f MS", float64(s.LastComposeUS)/1000, float64(s.MaxComposeUS)/1000),
			fmt.Sprintf("SHARP UI CACHE BUILDS: %d", s.ForegroundBuilds), fmt.Sprintf("MENU MEMORY: %.2f MIB", float64(s.ResidentBytes)/(1<<20)))
		if s.LastError != "" {
			lines = append(lines, "G2D: "+s.LastError)
		}
	}
	if u.presentFrames != nil {
		p := u.presentFrames.snapshot()
		lines = append(lines, fmt.Sprintf("DECODED HANDOFF: %d / REPLACED: %d", p.Offered, p.Replaced), fmt.Sprintf("PENDING DMA FRAMES: %d / MAX 2", p.PendingFrames))
	}
	return append(lines, "COUNTERS ARE NOT PHYSICAL PANEL FPS", "BACK: MONITOR")
}

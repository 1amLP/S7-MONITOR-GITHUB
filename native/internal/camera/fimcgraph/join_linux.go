//go:build linux && (amd64 || arm64)

// Package fimcgraph connects already configured native FIMC queues. It never
// guesses sensor profiles, opens Android libraries, or resets the USB gadget.
package fimcgraph

import (
	"errors"
	"fmt"
	"perimode/native/internal/camera/fimcdma"
	"perimode/native/internal/camera/fimcshot"
	"syscall"
	"time"
)

var ErrStale = errors.New("stale FIMC result/session")
var ErrDuplicate = errors.New("duplicate FIMC result")

type joinEntry struct {
	used, hasShot, hasCapture bool
	consumed                  bool
	count                     uint32
	born                      time.Time
	frame                     fimcdma.Frame
	shot                      [fimcshot.Size]byte
}
type JoinStats struct{ Matched, Dropped, Expired, Rejected uint64 }

// Joiner is single-owner. DMA pixels are not retained as Go slices. Only a
// completed queue handle is held, together with a copied 32-KiB request result.
// A frame slot number is never used as a sensor request number.
type Joiner struct {
	entries   []joinEntry
	epoch     uint64
	ttl       time.Duration
	release   func(fimcdma.Frame) error
	floor     uint32
	haveFloor bool
	stats     JoinStats
}

func NewJoiner(capacity int, epoch uint64, ttl time.Duration, release func(fimcdma.Frame) error) (*Joiner, error) {
	if capacity < 2 || capacity > 8 || epoch == 0 || ttl < time.Millisecond || ttl > 2*time.Second || release == nil {
		return nil, fmt.Errorf("invalid bounded frame joiner")
	}
	return &Joiner{entries: make([]joinEntry, capacity), epoch: epoch, ttl: ttl, release: release}, nil
}
func after(a, b uint32) bool       { return int32(a-b) > 0 }
func (j *Joiner) Stats() JoinStats { return j.stats }
func (j *Joiner) Epoch() uint64    { return j.epoch }
func (j *Joiner) drop(e *joinEntry) error {
	if e.hasCapture {
		if err := j.release(e.frame); err != nil {
			return err
		}
	}
	*e = joinEntry{}
	j.stats.Dropped++
	return nil
}
func (j *Joiner) advance(n uint32) {
	if !j.haveFloor || after(n, j.floor) {
		j.floor = n
		j.haveFloor = true
	}
}
func (j *Joiner) expire(now time.Time) error {
	if now.IsZero() {
		return fmt.Errorf("missing local monotonic clock")
	}
	cutoff, have := j.floor, j.haveFloor
	// Establish the complete watermark before retiring entries. Otherwise an
	// older record visited early could escape a later expiry and be delivered
	// after a newer frame had already been retired.
	for i := range j.entries {
		e := &j.entries[i]
		if e.used && !now.Before(e.born) && now.Sub(e.born) >= j.ttl && (!have || after(e.count, cutoff)) {
			cutoff = e.count
			have = true
		}
	}
	if !have {
		return nil
	}
	for i := range j.entries {
		e := &j.entries[i]
		if e.used && !after(e.count, cutoff) {
			if err := j.drop(e); err != nil {
				return err
			}
			j.stats.Expired++
		}
	}
	j.advance(cutoff)
	return nil
}

// entry returns a live bounded record. On error, an incoming capture handle is
// still owned by the CALLER; no failed insertion silently consumes it.
func (j *Joiner) entry(epoch uint64, n uint32, now time.Time) (*joinEntry, error) {
	if epoch != j.epoch || n == 0 || now.IsZero() {
		j.stats.Rejected++
		return nil, ErrStale
	}
	if err := j.expire(now); err != nil {
		return nil, err
	}
	if j.haveFloor && !after(n, j.floor) {
		j.stats.Rejected++
		return nil, ErrStale
	}
	var empty, oldest *joinEntry
	for i := range j.entries {
		e := &j.entries[i]
		if e.used && e.count == n {
			return e, nil
		}
		if !e.used {
			if empty == nil {
				empty = e
			}
			continue
		}
		if oldest == nil || after(oldest.count, e.count) {
			oldest = e
		}
	}
	if empty == nil {
		if !after(n, oldest.count) {
			j.stats.Rejected++
			return nil, ErrStale
		}
		old := oldest.count
		if err := j.drop(oldest); err != nil {
			return nil, err
		}
		j.advance(old)
		empty = oldest
	}
	*empty = joinEntry{used: true, count: n, born: now}
	return empty, nil
}
func (j *Joiner) PutShot(epoch uint64, shot []byte, now time.Time) error {
	v, err := fimcshot.Bind(shot)
	if err != nil {
		return err
	}
	n, err := v.DynamicFrameCount()
	if err != nil {
		return err
	}
	ts, err := v.SensorTimestampNS()
	if err != nil || ts == 0 {
		return fmt.Errorf("completed shot has no sensor timestamp")
	}
	e, err := j.entry(epoch, n, now)
	if err != nil {
		return err
	}
	if e.hasShot {
		j.stats.Rejected++
		return ErrDuplicate
	}
	copy(e.shot[:], shot)
	e.hasShot = true
	return nil
}
func (j *Joiner) PutCapture(epoch uint64, f fimcdma.Frame, stream []byte, now time.Time) error {
	s, err := fimcshot.ReadStream(stream)
	if err != nil {
		return err
	}
	e, err := j.entry(epoch, s.FrameCount, now)
	if err != nil {
		return err
	}
	if e.hasCapture {
		j.stats.Rejected++
		return ErrDuplicate
	}
	e.frame = f
	e.hasCapture = true
	return nil
}

// DeliverLatest coalesces independent RAW/NV12 sensor frames, never compressed
// H.264 reference pictures. EAGAIN keeps the selected pair held for retry.
// fn may forward to an idle downstream queue, but must not reenter this joiner.
func (j *Joiner) DeliverLatest(now time.Time, fn func(fimcdma.Frame, []byte) error) (bool, error) {
	return j.deliver(now, fn, true)
}

func (j *Joiner) DeliverNext(now time.Time, fn func(fimcdma.Frame, []byte) error) (bool, error) {
	return j.deliver(now, fn, false)
}

func (j *Joiner) deliver(now time.Time, fn func(fimcdma.Frame, []byte) error, latest bool) (bool, error) {
	if fn == nil {
		return false, fmt.Errorf("nil paired-frame consumer")
	}
	if err := j.expire(now); err != nil {
		return false, err
	}
	var best *joinEntry
	for i := range j.entries {
		e := &j.entries[i]
		if e.used && (!latest || e.hasShot && e.hasCapture) && (best == nil || latest && after(e.count, best.count) || !latest && after(best.count, e.count)) {
			best = e
		}
	}
	if best == nil || !best.hasShot || !best.hasCapture {
		return false, nil
	}
	shown := false
	if !best.consumed {
		if err := fn(best.frame, best.shot[:]); err != nil {
			if !errors.Is(err, fimcdma.ErrQuarantined) && (errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR)) {
				return false, nil
			}
			return false, err
		}
		best.consumed = true
		j.stats.Matched++
		shown = true
	}
	n := best.count
	if err := j.drop(best); err != nil {
		return shown, err
	}
	j.stats.Dropped--
	j.advance(n)
	// All older pairs can now be retired. Failed release retains the record;
	// the caller must stop/recover the graph, not free its underlying DMA pool.
	for i := range j.entries {
		e := &j.entries[i]
		if e.used && !after(e.count, j.floor) {
			if err := j.drop(e); err != nil {
				return shown, err
			}
		}
	}
	return shown, nil
}
func (j *Joiner) Pending() int {
	n := 0
	for _, e := range j.entries {
		if e.used {
			n++
		}
	}
	return n
}

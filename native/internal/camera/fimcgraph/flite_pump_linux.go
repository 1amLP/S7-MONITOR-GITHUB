//go:build linux && (amd64 || arm64)

package fimcgraph

import (
	"errors"
	"fmt"
	"perimode/native/internal/camera/fimcdma"
	"perimode/native/internal/media"
	"os"
	"sync"
)

// OTF FLITE buffers maintain sensor progress; the verified hardware profile must
// route sensor -> 3AA on-the-fly. This is NOT a Bayer source for an M2M 3AA path.
// Recycled buffers never become webcam images; only downstream MCSC does that.
type primedFlite struct {
	q       Queue
	request Request
}

func (p *primedFlite) fill() error {
	for n := 0; n < p.q.BufferCount(); n++ {
		i, e := p.q.NextIdle()
		if retry(e) {
			return nil
		}
		if e != nil {
			return e
		}
		r := &p.request
		if e = p.q.Submit(i, r.Template, r.Group, r.Controls, r.Limits); retry(e) {
			return nil
		} else if e != nil {
			return e
		}
		r.Controls.Trigger = 0 // edge consumed once, never reissued for every buffer
	}
	return nil
}
func (p *primedFlite) Start() error {
	if e := p.fill(); e != nil {
		return e
	}
	return p.q.Start()
}
func (p *primedFlite) Stop() error { return p.q.Stop() }

// FlitePump borrows its already configured queue and sensor. It neither opens a
// guessed node nor selects a guessed profile. SetStreaming and Pump are serialized.
type FlitePump struct {
	mu                  sync.Mutex
	queue               *primedFlite
	gate                *FliteSwitch
	on, closing, closed bool
	completed, dropped  uint64
	bad                 uint32
}

func NewOTFFlitePump(q Queue, sensor SensorSwitch, request Request, verifiedOTF bool) (*FlitePump, error) {
	if !verifiedOTF || q == nil || sensor == nil || q.MetadataRole() != fimcdma.ShotMetadata || q.BufferCount() < 2 || q.BufferCount() > 8 {
		return nil, fmt.Errorf("verified sensor-to-3AA OTF profile and bounded FLITE shot queue required")
	}
	r, e := freezeRequest(request)
	if e != nil {
		return nil, e
	}
	p := &primedFlite{q: q, request: r}
	gate, e := NewFliteSwitch(p, sensor)
	if e != nil {
		return nil, e
	}
	return &FlitePump{queue: p, gate: gate}, nil
}
func (p *FlitePump) SetStreaming(on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || on && p.closing {
		return os.ErrClosed
	}
	e := p.gate.SetStreaming(on)
	p.on = on && e == nil
	if e != nil {
		p.closing = true
	}
	return e
}

// Pump does not wait for hardware: no more than BufferCount DQBUF calls. It
// never maps or copies unused FLITE Bayer pixels just to recycle the pool.
func (p *FlitePump) Pump() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.on || p.closed || p.closing {
		return 0, os.ErrClosed
	}
	fail := func(n int, e error) (int, error) { p.closing = true; return n, e }
	if e := p.queue.fill(); e != nil {
		return fail(0, e)
	}
	done := 0
	for n := 0; n < p.queue.q.BufferCount(); n++ {
		f, e := p.queue.q.Dequeue()
		if retry(e) {
			break
		}
		if e != nil {
			return fail(done, e)
		}
		// V4L2_BUF_FLAG_ERROR is a bad completed frame, NOT unresolved DMA ownership.
		if f.Flags&0x40 != 0 {
			p.bad++
			p.dropped++
		} else {
			p.bad = 0
			p.completed++
		}
		if e = p.queue.q.ReleaseFrame(f); e != nil {
			return fail(done, e)
		}
		done++
		if p.bad >= 8 {
			return fail(done, fmt.Errorf("repeated FLITE buffer errors"))
		}
	}
	if e := p.queue.fill(); e != nil {
		return fail(done, e)
	}
	return done, nil
}
func (p *FlitePump) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closing = true
	p.on = false
	if e := p.gate.Close(); e != nil {
		return e
	}
	p.closed = true
	return nil
}
func (p *FlitePump) Counters() (uint64, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.completed, p.dropped
}

// PreparedFlite owns ION allocations and queue imports; configured node and
// sensor descriptors remain borrowed. A failed constructor returns the owner
// when cleanup is incomplete, exactly like PreparedGraph. Never discard it.
type PreparedFlite struct {
	mu              sync.Mutex
	pool            *fimcdma.Pool
	pump            *FlitePump
	closing, closed bool
}

func PrepareOTFFlite(a *fimcdma.Allocator, node *fimcdma.PreparedNode, sensor SensorSwitch, request Request, buffers uint32, verifiedOTF bool) (out *PreparedFlite, err error) {
	if a == nil || node == nil || sensor == nil || !verifiedOTF || buffers < 2 || buffers > 8 {
		return nil, fmt.Errorf("complete calibrated OTF FLITE preparation required")
	}
	f, role, e := node.ConfiguredLayout()
	if e != nil {
		return nil, e
	}
	if f.Type != media.Capture || f.Planes() != 2 || role != fimcdma.ShotMetadata {
		return nil, fmt.Errorf("physical FLITE Bayer + shot layout required")
	}
	switch f.PixelFormat() {
	case 0x30314742, 0x32314742, 0x32525942:
	default:
		return nil, fmt.Errorf("FLITE Bayer format required")
	}
	if _, e = freezeRequest(request); e != nil {
		return nil, e
	}
	owner := &PreparedFlite{}
	defer func() {
		if err != nil {
			cleanup := owner.Close()
			err = errors.Join(err, cleanup)
			if cleanup == nil {
				out = nil
			} else {
				out = owner
			}
		}
	}()
	owner.pool, err = fimcdma.AllocateTypedPool(a, node, buffers, role)
	if err != nil {
		return owner, err
	}
	owner.pump, err = NewOTFFlitePump(owner.pool.Queue, sensor, request, true)
	return owner, err
}
func (p *PreparedFlite) SetStreaming(on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.pump == nil || on && p.closing {
		return os.ErrClosed
	}
	return p.pump.SetStreaming(on)
}
func (p *PreparedFlite) Pump() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.closing || p.pump == nil {
		return 0, os.ErrClosed
	}
	return p.pump.Pump()
}
func (p *PreparedFlite) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closing = true
	if p.pump != nil {
		if e := p.pump.Close(); e != nil {
			return errors.Join(e, fimcdma.ErrQuarantined)
		}
		p.pump = nil
	}
	if p.pool != nil {
		if e := p.pool.Close(); e != nil {
			return errors.Join(e, fimcdma.ErrQuarantined)
		}
		p.pool = nil
	}
	p.closed = true
	return nil
}

var _ SensorSwitch = (*PreparedFlite)(nil)

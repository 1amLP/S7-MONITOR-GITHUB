//go:build linux && (amd64 || arm64)

package fimcgraph

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"perimode/native/internal/camera/fimcdma"
	"perimode/native/internal/camera/fimcshot"
	"perimode/native/internal/media"
)

// USB and Preview may each retain an active and a pending frame. Keep two
// additional MCSC buffers available to the hardware pipeline.
const ConsumerCaptureBuffers = 6

// PreparedConfig refers to nodes that have completed ConfigureNode. It does not
// replace per-module sensor/companion initialization. Node FDs, the allocator,
// and the configured sensor remain borrowed until this graph closes successfully.
type PreparedConfig struct {
	RawLeader, RawCapture, ISPLeader, ISPCapture *fimcdma.PreparedNode
	Sensor                                       SensorSwitch
	RawRequest                                   Request
	ISPGroup                                     fimcshot.Group
	Buffers                                      uint32
}

type managedSession interface {
	Start() error
	Drain(func(media.Image) error) (int, error)
	Close() error
}

// PreparedGraph owns all four queue pools and the session. After failed setup or
// shutdown it remains a retryable owner, not just an error that loses DMA handles.
// It deliberately has no UDC, gadget reset, Android or shared-media teardown API.
type PreparedGraph struct {
	mu                        sync.Mutex
	session                   managedSession
	pools                     []io.Closer
	started, stopping, closed bool
}

func (c PreparedConfig) validate() ([4]media.Format, error) {
	var formats [4]media.Format
	nodes := [4]*fimcdma.PreparedNode{c.RawLeader, c.RawCapture, c.ISPLeader, c.ISPCapture}
	if c.Sensor == nil || c.Buffers < 2 || c.Buffers > 8 {
		return formats, fmt.Errorf("configured sensor and 2..8 buffers required")
	}
	for i, n := range nodes {
		f, role, e := n.ConfiguredLayout()
		if e != nil {
			return formats, e
		}
		expectRole := fimcdma.ShotMetadata
		expectType := media.Output
		if i%2 == 1 {
			expectRole = fimcdma.StreamMetadata
			expectType = media.Capture
		}
		if role != expectRole || f.Type != expectType {
			return formats, fmt.Errorf("graph node %d has wrong role/type", i)
		}
		for j := 0; j < i; j++ {
			if n.SharesDescriptor(nodes[j]) {
				return formats, fmt.Errorf("duplicate graph node descriptor")
			}
		}
		formats[i] = f
	}
	if e := fimcdma.VerifyRawLink(formats[1], formats[2]); e != nil {
		return formats, e
	}
	// Validate both node groups and all controls before requesting any imports.
	var scratch [fimcshot.Size]byte
	v, e := c.RawRequest.Template.Reset(scratch[:])
	if e != nil {
		return formats, e
	}
	if e = v.SetGroup(c.RawRequest.Group); e != nil {
		return formats, e
	}
	if e = v.ApplyControls(c.RawRequest.Controls, c.RawRequest.Limits); e != nil {
		return formats, e
	}
	if e = v.SetGroup(c.ISPGroup); e != nil {
		return formats, e
	}
	if !c.RawRequest.Group.Leader.Requested || !c.ISPGroup.Leader.Requested {
		return formats, fmt.Errorf("requested graph leaders required")
	}
	if e := validateNV12Format(formats[3]); e != nil {
		return formats, e
	}
	return formats, nil
}

// PrepareConfiguredGraph performs real REQBUFS/ION/import construction and wires
// the existing 3AA -> ISP -> MCSC code; it does not start streaming. On error it
// returns a non-nil graph ONLY when cleanup failed. Retain it and retry Close;
// closing the borrowed FDs/allocator in that state is not safe.
func PrepareConfiguredGraph(a *fimcdma.Allocator, c PreparedConfig) (out *PreparedGraph, err error) {
	if a == nil {
		return nil, fmt.Errorf("nil ION allocator")
	}
	formats, e := c.validate()
	if e != nil {
		return nil, e
	}
	owner := &PreparedGraph{}
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
	nodes := [4]*fimcdma.PreparedNode{c.RawLeader, c.RawCapture, c.ISPLeader, c.ISPCapture}
	var pools [4]*fimcdma.Pool
	for i, n := range nodes {
		role := fimcdma.ShotMetadata
		if i%2 == 1 {
			role = fimcdma.StreamMetadata
		}
		var p *fimcdma.Pool
		var e error
		if i == 2 {
			p, e = fimcdma.AllocateSharedRawPool(a, n, c.Buffers, pools[1])
		} else {
			count := c.Buffers
			if i == 3 {
				count = max(count, ConsumerCaptureBuffers)
			}
			p, e = fimcdma.AllocateTypedPool(a, n, count, role)
		}
		if p != nil {
			owner.pools = append(owner.pools, p)
			pools[i] = p
		}
		if e != nil {
			return owner, fmt.Errorf("allocate FIMC node %d: %w", i, e)
		}
	}
	raw, e := NewStage(pools[0].Queue, pools[1].Queue, &c.RawRequest)
	if e != nil {
		return owner, e
	}
	isp, e := NewStage(pools[2].Queue, pools[3].Queue, nil)
	if e != nil {
		return owner, e
	}
	image, e := NV12Consumer(pools[3].Queue, formats[3])
	if e != nil {
		return owner, e
	}
	session, e := NewSession(raw, isp, c.Sensor, DMAForwarder(pools[1].Queue, pools[2].Queue, c.ISPGroup), image)
	if e != nil {
		return owner, e
	}
	owner.session = session
	return owner, nil
}
func (g *PreparedGraph) Start() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.stopping {
		return os.ErrClosed
	}
	if g.started {
		return nil
	}
	if g.session == nil {
		return fmt.Errorf("partially prepared graph cannot start")
	}
	if e := g.session.Start(); e != nil {
		// Session.Start attempts a stop too. Retry through the same owner; never
		// let callers lose a failed-stop handle hidden in a construction error.
		return errors.Join(e, g.closeLocked())
	}
	g.started = true
	return nil
}
func (g *PreparedGraph) Drain(fn func(media.Image) error) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.started || g.stopping || g.closed {
		return 0, os.ErrClosed
	}
	return g.session.Drain(fn)
}
func (g *PreparedGraph) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closeLocked()
}
func (g *PreparedGraph) closeLocked() error {
	if g.closed {
		return nil
	}
	g.stopping = true
	if g.session != nil {
		if e := g.session.Close(); e != nil {
			return errors.Join(e, fimcdma.ErrQuarantined)
		}
		g.session = nil
		g.started = false
	}
	// Independent stopped queues may release in reverse order. A failed one is
	// retained and retried; successful resources are never closed twice.
	var errs []error
	for i := len(g.pools) - 1; i >= 0; i-- {
		if g.pools[i] == nil {
			continue
		}
		if e := g.pools[i].Close(); e != nil {
			errs = append(errs, e)
		} else {
			g.pools[i] = nil
		}
	}
	if e := errors.Join(errs...); e != nil {
		return errors.Join(e, fimcdma.ErrQuarantined)
	}
	g.pools = nil
	g.closed = true
	return nil
}

// UpdateControls uses the same lifecycle lock as Drain/Close, without closing
// the source or disconnecting USB. No method is offered for guessing profiles.
func (g *PreparedGraph) UpdateControls(c fimcshot.Controls) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.stopping || g.session == nil {
		return os.ErrClosed
	}
	s, ok := g.session.(interface{ UpdateControls(fimcshot.Controls) error })
	if !ok {
		return fmt.Errorf("prepared session has no request control path")
	}
	return s.UpdateControls(c)
}

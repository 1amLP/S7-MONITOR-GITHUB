//go:build linux && (amd64 || arm64)

package fimcgraph

import (
	"errors"
	"fmt"
	"perimode/native/internal/camera/fimcdma"
	"perimode/native/internal/camera/fimcshot"
	"log"
	"syscall"
	"time"
)

// Queue is satisfied by the real native DMABUF queue. Configuration and pool
// ownership remain outside the stage; callers retain them through failed Stop.
type Queue interface {
	MetadataRole() fimcdma.MetadataRole
	BufferCount() int
	NextIdle() (uint32, error)
	Submit(uint32, *fimcshot.Template, fimcshot.Group, fimcshot.Controls, fimcshot.Limits) error
	SubmitCapture(uint32) error
	Start() error
	Stop() error
	Dequeue() (fimcdma.Frame, error)
	WithFrameCPU(fimcdma.Frame, func([][]byte) error) error
	ReleaseFrame(fimcdma.Frame) error
}

var _ Queue = (*fimcdma.Queue)(nil)

type Request struct {
	Template *fimcshot.Template
	Group    fimcshot.Group
	Controls fimcshot.Controls
	Limits   fimcshot.Limits
}
type Stage struct {
	Leader, Capture                 Queue
	automatic                       *Request
	join                            *Joiner
	leaderOn, captureOn             bool
	epoch                           uint64
	bad                             uint32
	badCapture                      uint32
	observedShots, observedCaptures uint32
}

// NewStage does not submit anything. automatic is used only for an OTF sensor
// leader whose image input is unused by the driver. M2M ISP leaders pass nil:
// they never receive fabricated Bayer input. The pinned Samsung ISP can start
// empty before 3AA; other leaders wait for a real upstream Bayer result.
func NewStage(leader, capture Queue, automatic *Request) (*Stage, error) {
	if leader == nil || capture == nil || leader.MetadataRole() != fimcdma.ShotMetadata || capture.MetadataRole() != fimcdma.StreamMetadata {
		return nil, fmt.Errorf("FIMC leader/result role mismatch")
	}
	if leader.BufferCount() < 2 || leader.BufferCount() > 8 || capture.BufferCount() < 2 || capture.BufferCount() > 8 {
		return nil, fmt.Errorf("unbounded FIMC stage")
	}
	s := &Stage{Leader: leader, Capture: capture, epoch: 1}
	if automatic != nil {
		request, e := freezeRequest(*automatic)
		if e != nil {
			return nil, e
		}
		s.automatic = &request
	}
	if e := s.resetJoin(); e != nil {
		return nil, e
	}
	return s, nil
}
func (s *Stage) resetJoin() error {
	var e error
	ttl := 250 * time.Millisecond
	if s.automatic != nil && s.automatic.Limits.FrameDurationNS > 0 {
		ttl = min(ttl, 2*time.Duration(s.automatic.Limits.FrameDurationNS))
	}
	s.join, e = NewJoiner(s.Capture.BufferCount(), s.epoch, ttl, s.Capture.ReleaseFrame)
	return e
}
func retry(err error) bool {
	return !errors.Is(err, fimcdma.ErrQuarantined) && (errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR))
}
func (s *Stage) fill() error {
	if err := s.fillCapture(); err != nil {
		return err
	}
	return s.fillAutomatic()
}
func (s *Stage) fillCapture() error {
	for n := 0; n < s.Capture.BufferCount(); n++ {
		i, e := s.Capture.NextIdle()
		if retry(e) {
			break
		}
		if e != nil {
			return e
		}
		if e = s.Capture.SubmitCapture(i); retry(e) {
			break
		} else if e != nil {
			return e
		}
	}
	return nil
}
func (s *Stage) fillAutomatic() error {
	if r := s.automatic; r != nil {
		for n := 0; n < s.Leader.BufferCount(); n++ {
			if cap, ok := s.Capture.(interface{ QueuedCount() int }); ok {
				if leader, ok := s.Leader.(interface{ QueuedCount() int }); ok && cap.QueuedCount() <= leader.QueuedCount() {
					break
				}
			}
			i, e := s.Leader.NextIdle()
			if retry(e) {
				break
			}
			if e != nil {
				return e
			}
			if e = s.Leader.Submit(i, r.Template, r.Group, r.Controls, r.Limits); retry(e) {
				break
			} else if e != nil {
				return e
			}
			// AF START/CANCEL is an edge, not a setting for every queued frame.
			// Submit owns a value copy; in-flight requests are not modified.
			r.Controls.Trigger = fimcshot.FocusIdle
		}
	}
	return nil
}
func (s *Stage) PrimeAndStartCapture() error {
	early := false
	if q, ok := s.Leader.(interface{ CanStartEmpty() bool }); ok && s.automatic != nil {
		early = q.CanStartEmpty()
	}
	var e error
	if early {
		e = s.fillCapture()
	} else {
		e = s.fill()
	}
	if e != nil {
		return e
	}
	if !s.captureOn {
		if e := s.Capture.Start(); e != nil {
			return e
		}
		s.captureOn = true
	}
	if s.automatic != nil {
		if e := s.StartLeader(); e != nil {
			return e
		}
		if early {
			return s.fillAutomatic()
		}
	}
	return nil
}
func (s *Stage) StartLeader() error {
	if s.leaderOn {
		return nil
	}
	if e := s.Leader.Start(); e != nil {
		return e
	}
	s.leaderOn = true
	return nil
}

// Pump visits each hardware queue at most BufferCount times. It does not spin
// until a device produces data and does not hide unknown driver errors.
func (s *Stage) Pump(now time.Time) (int, error) {
	if !s.captureOn {
		return 0, fmt.Errorf("FIMC stage capture not started")
	}
	if e := s.fill(); e != nil {
		return 0, e
	}
	completed := 0
	if s.leaderOn {
		for n := 0; n < s.Leader.BufferCount(); n++ {
			f, e := s.Leader.Dequeue()
			if retry(e) {
				break
			}
			if e != nil {
				return completed, e
			}
			// Never call release from inside WithFrameCPU: its ownership lock is held.
			var snapshot [fimcshot.Size]byte
			read := s.Leader.WithFrameCPU(f, func(p [][]byte) error {
				if len(p) < 2 || len(p[len(p)-1]) < len(snapshot) {
					return fmt.Errorf("short completed shot plane")
				}
				copy(snapshot[:], p[len(p)-1][:len(snapshot)])
				return nil
			})
			if e = s.Leader.ReleaseFrame(f); e != nil {
				return completed, e
			}
			if read == nil {
				read = s.join.PutShot(s.epoch, snapshot[:], now)
			}
			if read != nil {
				s.bad++
				if s.bad == 1 {
					log.Printf("S7 camera stage automatic=%t rejected shot: %v", s.automatic != nil, read)
				}
				if s.bad >= 8 {
					return completed, fmt.Errorf("repeated invalid FIMC shot: %w", read)
				}
			} else {
				s.bad = 0
			}
			if s.observedShots < 2 {
				v, err := fimcshot.Bind(snapshot[:])
				n, _ := v.DynamicFrameCount()
				ts, _ := v.SensorTimestampNS()
				log.Printf("S7 camera stage automatic=%t shot frame=%d timestamp=%d bind=%v join=%v", s.automatic != nil, n, ts, err, read)
				s.observedShots++
			}
			completed++
		}
	}
	for n := 0; n < s.Capture.BufferCount(); n++ {
		f, e := s.Capture.Dequeue()
		if retry(e) {
			break
		}
		if e != nil {
			return completed, e
		}
		var prefix [fimcshot.StreamHeaderSize]byte
		read := s.Capture.WithFrameCPU(f, func(p [][]byte) error {
			if len(p) < 2 || len(p[len(p)-1]) < len(prefix) {
				return fmt.Errorf("short capture stream plane")
			}
			copy(prefix[:], p[len(p)-1][:len(prefix)])
			return nil
		})
		if read == nil {
			read = s.join.PutCapture(s.epoch, f, prefix[:], now)
		}
		if s.observedCaptures < 2 {
			log.Printf("S7 camera stage automatic=%t capture header=%x join=%v", s.automatic != nil, prefix, read)
			s.observedCaptures++
		}
		if read != nil {
			if e = s.Capture.ReleaseFrame(f); e != nil {
				return completed, e
			}
			// Stale/duplicate completions are dropped, not used as evidence that the
			// source was resized or that another camera should be selected.
			if !errors.Is(read, ErrStale) && !errors.Is(read, ErrDuplicate) {
				s.badCapture++
				if s.badCapture == 1 {
					log.Printf("S7 camera stage automatic=%t rejected capture: %v", s.automatic != nil, read)
				}
				if s.badCapture >= 8 {
					return completed, fmt.Errorf("repeated invalid FIMC capture: %w", read)
				}
			}
		} else {
			s.badCapture = 0
		}
		completed++
	}
	return completed, s.fill()
}
func (s *Stage) DeliverLatest(now time.Time, fn func(fimcdma.Frame, []byte) error) (bool, error) {
	return s.join.DeliverLatest(now, fn)
}
func (s *Stage) DeliverNext(now time.Time, fn func(fimcdma.Frame, []byte) error) (bool, error) {
	return s.join.DeliverNext(now, fn)
}
func (s *Stage) Stats() JoinStats { return s.join.Stats() }

// Stop is retriable. A failed STREAMOFF leaves all queue owners retained. Only
// after both succeed are cached frame handles invalidated and an epoch advanced.
func (s *Stage) Stop() error {
	e := errors.Join(s.Leader.Stop(), s.Capture.Stop())
	if e != nil {
		return e
	}
	s.leaderOn = false
	s.captureOn = false
	s.bad = 0
	s.badCapture = 0
	s.epoch++
	return s.resetJoin()
}

func freezeRequest(r Request) (Request, error) {
	if r.Template == nil {
		return Request{}, fmt.Errorf("missing FIMC request template")
	}
	template := *r.Template // Deep copy: Template contains a fixed-size byte array.
	r.Template = &template
	var b [fimcshot.Size]byte
	v, e := r.Template.Reset(b[:])
	if e != nil {
		return Request{}, e
	}
	if e = v.SetGroup(r.Group); e != nil {
		return Request{}, e
	}
	if e = v.ApplyControls(r.Controls, r.Limits); e != nil {
		return Request{}, e
	}
	return r, nil
}

// Caller serializes this with Pump through Session.mu. Validation is atomic;
// failure does not modify controls or pending one-shot focus action.
func (s *Stage) updateControls(c fimcshot.Controls) error {
	if s.automatic == nil {
		return fmt.Errorf("controls require the sensor-side request stage")
	}
	r := *s.automatic
	// START/CANCEL is an edge. Keep it until the first successful QBUF, even
	// when an unrelated control revision arrives while all slots are occupied.
	if r.Controls.Trigger != fimcshot.FocusIdle {
		if c.Trigger != fimcshot.FocusIdle || c.Focus != r.Controls.Focus {
			return syscall.EAGAIN
		}
		c.Trigger = r.Controls.Trigger
	}
	r.Controls = c
	next, e := freezeRequest(r)
	if e != nil {
		return e
	}
	s.automatic = &next
	return nil
}

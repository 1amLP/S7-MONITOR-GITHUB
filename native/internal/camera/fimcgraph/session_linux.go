//go:build linux && (amd64 || arm64)

package fimcgraph

import (
	"errors"
	"fmt"
	"perimode/native/internal/camera/fimcdma"
	"perimode/native/internal/camera/fimcshot"
	"perimode/native/internal/media"
	"sync"
	"syscall"
	"time"
)

// SensorSwitch must refer to the selected, configured sensor. No default node,
// fake camera, Android fallback, or process-wide USB control is available here.
type SensorSwitch interface{ SetStreaming(bool) error }
type Forward func(fimcdma.Frame, []byte) error

type Session struct {
	mu                              sync.Mutex
	raw, isp                        *Stage
	sensor                          SensorSwitch
	forward                         Forward
	image                           func(fimcdma.Frame, []byte, func(media.Image) error) error
	started, sensorTouched, stopped bool
	closing                         bool
	ispHasInput                     bool
}

// NewSession connects 3AA capture -> ISP M2M -> MCSC capture. It does NOT prepare
// sensor firmware/setfiles/profiles. No data flows until explicitly started.
func NewSession(raw, isp *Stage, sensor SensorSwitch, forward Forward, image func(fimcdma.Frame, []byte, func(media.Image) error) error) (*Session, error) {
	if raw == nil || isp == nil || raw == isp || sensor == nil || forward == nil || image == nil || raw.automatic == nil || isp.automatic != nil {
		return nil, fmt.Errorf("incomplete 3AA/M2M/MCSC session")
	}
	return &Session{raw: raw, isp: isp, sensor: sensor, forward: forward, image: image}, nil
}
func (s *Session) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.closing {
		return fmt.Errorf("session closing or closed")
	}
	if s.started {
		return nil
	}
	// A downstream capture is ready before the sensor/3AA can produce RAW.
	// Never submit fabricated Bayer buffers to start the ISP.
	if e := s.isp.PrimeAndStartCapture(); e != nil {
		return errors.Join(e, s.stopLocked())
	}
	// The pinned Samsung group manager rejects 3AA STREAMON until every M2M
	// group is started. Its ISP permits empty STREAMON after REQBUFS. Other
	// queues retain the deferred-start path until real RAW has been forwarded.
	if q, ok := s.isp.Leader.(interface{ CanStartEmpty() bool }); ok && q.CanStartEmpty() {
		if e := s.isp.StartLeader(); e != nil {
			return errors.Join(e, s.stopLocked())
		}
	}
	if e := s.raw.PrimeAndStartCapture(); e != nil {
		return errors.Join(e, s.stopLocked())
	}
	s.sensorTouched = true // Even a failed start may have reached the sensor.
	if e := s.sensor.SetStreaming(true); e != nil {
		return errors.Join(e, s.stopLocked())
	}
	s.started = true
	return nil
}
func (s *Session) Drain(present func(media.Image) error) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.stopped {
		return 0, fmt.Errorf("session not streaming")
	}
	if present == nil {
		return 0, fmt.Errorf("nil frame consumer")
	}
	// A prepared OTF FLITE source must recycle physical capture buffers too.
	// Bounded Pump failure is camera-local; never continue the downstream graph
	// with a permanently stalled physical queue.
	if sensor, ok := s.sensor.(interface{ Pump() (int, error) }); ok {
		if _, e := sensor.Pump(); e != nil {
			return 0, errors.Join(e, s.stopLocked())
		}
	}
	// A prior RAW may already be queued even if STREAMON returned EAGAIN.
	// Retry only STREAMON, never copy/queue that same frame again.
	if ready, e := s.startISP(); !ready || e != nil {
		return 0, e
	}
	now := time.Now()
	if _, e := s.isp.Pump(now); e != nil {
		return 0, e
	}
	if _, e := s.raw.Pump(now); e != nil {
		return 0, e
	}
	var e error
	for n := 0; n < s.raw.Capture.BufferCount(); n++ {
		if cap, ok := s.isp.Capture.(interface{ QueuedCount() int }); ok {
			if leader, ok := s.isp.Leader.(interface{ QueuedCount() int }); ok && cap.QueuedCount() <= leader.QueuedCount() {
				break
			}
		}
		var forwarded bool
		forwarded, e = s.raw.DeliverNext(now, func(f fimcdma.Frame, b []byte) error {
			if e := s.forward(f, b); e != nil {
				return e
			}
			s.ispHasInput = true
			return nil
		})
		if e != nil || !forwarded {
			break
		}
	}
	if e != nil {
		return 0, e
	}
	if ready, e := s.startISP(); !ready || e != nil {
		return 0, e
	}
	if _, e = s.isp.Pump(now); e != nil {
		return 0, e
	}
	deliver, limit := s.isp.DeliverLatest, 1
	if s.raw.automatic.Limits.FrameDurationNS < 1_000_000_000/60 {
		deliver, limit = s.isp.DeliverNext, 2
	}
	n := 0
	for n < limit {
		shown, err := deliver(now, func(f fimcdma.Frame, b []byte) error {
			return s.image(f, b, func(im media.Image) error {
				if im.Lease != nil {
					raw, isp := s.raw.Stats(), s.isp.Stats()
					im.Lease.CameraStages = media.CameraStages{RawMatched: raw.Matched, RawDropped: raw.Dropped, RawExpired: raw.Expired, RawRejected: raw.Rejected, ISPMatched: isp.Matched, ISPDropped: isp.Dropped, ISPExpired: isp.Expired, ISPRejected: isp.Rejected}
				}
				return present(im)
			})
		})
		if shown {
			n++
		}
		if err != nil || !shown {
			return n, err
		}
	}
	return n, nil
}

// STREAMON success/failure is independent from the upstream frame lease.
// A fatal start error stops the sensor immediately; failed stops keep ownership.
func (s *Session) startISP() (bool, error) {
	if !s.ispHasInput || s.isp.leaderOn {
		return true, nil
	}
	e := s.isp.StartLeader()
	if retry(e) {
		return false, nil
	}
	if e != nil {
		return false, errors.Join(e, s.stopLocked())
	}
	return true, nil
}
func (s *Session) stopLocked() error {
	if s.stopped {
		return nil
	}
	// Once stop is attempted, no further draining or restart can be allowed,
	// even if a vendor ioctl leaves DMA active. Only Close may retry cleanup.
	s.closing = true
	s.started = false
	if s.sensorTouched {
		if e := s.sensor.SetStreaming(false); e != nil {
			return fmt.Errorf("sensor stop unconfirmed; retain graph: %w", e)
		}
		s.sensorTouched = false
	}
	e := errors.Join(s.raw.Stop(), s.isp.Stop())
	if e != nil {
		return e
	}
	s.started = false
	s.stopped = true
	s.ispHasInput = false
	return nil
}

// Close stops streaming but does not free borrowed node pools. The graph owner
// must call pool.Close only after this method succeeds, including failed Start.
func (s *Session) Close() error { s.mu.Lock(); defer s.mu.Unlock(); return s.stopLocked() }

// DMAForwarder preserves a fixed shared RAW slot until downstream completion.
func DMAForwarder(src, dst *fimcdma.Queue, g fimcshot.Group) Forward {
	return func(f fimcdma.Frame, b []byte) error {
		if src == nil || dst == nil {
			return fmt.Errorf("nil M2M queue")
		}
		i := f.Index
		if !dst.SharesRawFrom(src) {
			var e error
			i, e = dst.NextIdle()
			if e != nil {
				return e
			}
		}
		e := src.ForwardCompletedTo(f, dst, i, b, g)
		if errors.Is(e, fimcdma.ErrBusy) {
			return syscall.EAGAIN
		}
		return e
	}
}

// NV12Consumer reads ONLY the linear format negotiated with MCSC. The metadata
// plane is excluded from video data. No RGB565 conversion or claimed upscaling.
func NV12Consumer(q *fimcdma.Queue, format media.Format) (func(fimcdma.Frame, []byte, func(media.Image) error) error, error) {
	if q == nil || q.MetadataRole() != fimcdma.StreamMetadata {
		return nil, fmt.Errorf("MCSC stream result queue required")
	}
	if e := validateNV12Format(format); e != nil {
		return nil, e
	}
	planes := format.Planes()
	w, h := int(format.Width()), int(format.Height())
	ys := int(format.Stride(0))
	us := int(format.Stride(1))
	if planes == 2 {
		us = ys
	}
	if ys < w || us < w || ys > 32768 || us > 32768 {
		return nil, fmt.Errorf("invalid MCSC strides")
	}
	return func(f fimcdma.Frame, b []byte, fn func(media.Image) error) error {
		if fn == nil {
			return fmt.Errorf("nil NV12 consumer")
		}
		v, e := fimcshot.Bind(b)
		if e != nil {
			return e
		}
		n, e := v.DynamicFrameCount()
		if e != nil {
			return e
		}
		ts, e := v.SensorTimestampNS()
		if e != nil || ts == 0 || ts > uint64(1<<63-1) {
			return fmt.Errorf("invalid sensor timestamp")
		}
		var im media.Image
		e = q.WithFrameCPU(f, func(p [][]byte) error {
			if len(p) != int(planes) {
				return fmt.Errorf("MCSC plane count changed")
			}
			stream, e := fimcshot.ReadStream(p[len(p)-1])
			if e != nil {
				return e
			}
			if stream.FrameCount != n {
				return fmt.Errorf("MCSC result/request mismatch")
			}
			y, uv := p[0], p[1]
			if planes == 2 {
				if len(y) < ys*h {
					return fmt.Errorf("short packed luma")
				}
				uv = y[ys*h:]
				y = y[:ys*h]
			}
			if len(y) < ys*(h-1)+w || len(uv) < us*(h/2-1)+w {
				return fmt.Errorf("short linear NV12 frame")
			}
			// Existing native camera/MFC transport PTS is measured in microseconds.
			im = media.Image{Width: w, Height: h, Y: y, UV: uv, StrideY: ys, StrideUV: us, PTS: int64(ts / 1000)}
			return nil
		})
		if e != nil {
			return e
		}
		strides := []uint32{uint32(ys)}
		if planes == 3 {
			strides = append(strides, uint32(us))
		}
		im.Lease, e = q.ExportFrame(f, w, h, im.PTS, strides, media.BT601Limited)
		if e != nil {
			return e
		}
		defer im.Lease.Release()
		im.Lease.StorageWidth = ys
		exposure, duration, iso, valid := v.SensorResult()
		im.Lease.Sensor = media.SensorResult{FrameCount: n, Available: valid, ISO: iso, ExposureNS: exposure, FrameDurationNS: duration}
		return fn(im)
	}, nil
}

// Shared by pre-allocation configuration validation and the actual consumer.
func validateNV12Format(format media.Format) error {
	if format.Type != media.Capture || format.Width() == 0 || format.Width() > 4096 || format.Width()%2 != 0 || format.Height() == 0 || format.Height() > 4096 || format.Height()%2 != 0 {
		return fmt.Errorf("invalid MCSC dimensions")
	}
	planes := format.Planes()
	if !((format.PixelFormat() == media.NV12M && planes == 3) || (format.PixelFormat() == media.NV12 && planes == 2)) {
		return fmt.Errorf("only linear NV12 plus separate stream metadata is supported")
	}
	if e := fimcdma.ValidateTypedLayout(format, fimcdma.StreamMetadata); e != nil {
		return e
	}
	y := uint64(format.Stride(0))
	uv := y
	if planes == 3 {
		uv = uint64(format.Stride(1))
	}
	w, h := uint64(format.Width()), uint64(format.Height())
	if y < w || uv < w || y > 32768 || uv > 32768 {
		return fmt.Errorf("invalid MCSC strides")
	}
	if planes == 3 {
		if y*h > uint64(format.PlaneSize(0)) || uv*(h/2) > uint64(format.PlaneSize(1)) {
			return fmt.Errorf("short MCSC image plane")
		}
	} else if y*h+uv*(h/2) > uint64(format.PlaneSize(0)) {
		return fmt.Errorf("short contiguous NV12 plane")
	}
	return nil
}

// UpdateControls affects future requests only. Already queued frames retain
// their snapshots; sensor/format/USB state are not restarted by this operation.
func (s *Session) UpdateControls(c fimcshot.Controls) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.closing {
		return fmt.Errorf("session closing or closed")
	}
	return s.raw.updateControls(c)
}

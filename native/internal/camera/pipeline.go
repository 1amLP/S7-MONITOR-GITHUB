package camera

import (
	"context"
	"errors"
	"fmt"
	"perimode/native/internal/media"
	"os"
	"time"
)

// Source owns sensor DMA buffers. The Image is only valid within emit. Submit
// MUST copy or synchronously import it before emit returns and the source QBUFs.
type Source interface {
	Drain(func(media.Image) error) (int, error)
	Close() error
}
type Encoder interface {
	Submit(media.Image) error
	Drain(func(media.Encoded) error) (int, error)
	ForceIDR() error
	Close() error
}

// Sink.Submit synchronously copies or accepts the entire AU. EAGAIN/EINTR
// mean no acceptance. A partial transfer must report a non-retryable error.
type Sink interface{ Submit(media.Encoded) error }
type Counters struct {
	SourceSensor                                                                       media.SensorResult
	SourceStages                                                                       media.CameraStages
	SourceSequenceGaps                                                                 uint64
	Captured, Submitted, Encoded, USBQueued, RawDropped, EncodedDropped, KeyRequests   uint64
	EncoderSkipped, PeakAccessUnitBytes, EncodedBytes, USBBytes                        uint64
	PendingFrames                                                                      uint32
	SensorFPSMilli, EncoderFPSMilli                                                    uint64
	LastCapturePTS, LastEncodedPTS                                                     int64
	EncoderBusy, USBBackpressure, USBRetryAttempts, USBRetryRecovered, USBRetryExpired uint64
	MaxUSBRetryMicros, MaxEncoderResidenceMicros                                       uint64
	USBPendingFrames, PeakPendingFrames                                                uint32
}
type Pipeline struct {
	retry                                                   outputRetry
	lastStep                                                time.Time
	contract                                                *modeContract
	source                                                  Source
	encoder                                                 Encoder
	sink                                                    Sink
	stats                                                   Counters
	needKey, keyRequested, haveCapture, haveEncoded, closed bool
	started, lastProgress, lastKey                          time.Time
	closeErr                                                error
}

func NewPipeline(source Source, encoder Encoder, sink Sink, now time.Time) (*Pipeline, error) {
	if source == nil || encoder == nil || sink == nil {
		return nil, fmt.Errorf("camera pipeline needs source, encoder and sink")
	}
	return &Pipeline{source: source, encoder: encoder, sink: sink, needKey: true, started: now, lastProgress: now}, nil
}
func (p *Pipeline) Stats() Counters { return p.stats }

// Step is nonblocking by contract. It never waits on USB while holding a sensor
// frame, and never buffers an unbounded chain of interdependent H.264 pictures.
func (p *Pipeline) Step(ctx context.Context, now time.Time) error {
	if p.closed {
		return os.ErrClosed
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if now.Before(p.started) || (!p.lastStep.IsZero() && now.Before(p.lastStep)) {
		return fmt.Errorf("camera pipeline local clock regressed")
	}
	p.lastStep = now
	if m := p.contract; m != nil {
		if err := m.checkResidence(now); err != nil {
			return err
		}
	}
	if now.Sub(p.lastProgress) > 3*time.Second {
		return fmt.Errorf("camera pipeline stalled for 3 seconds; no frames accepted by USB queue")
	}
	if blocked, err := p.retryUSB(now); err != nil {
		return fmt.Errorf("camera USB retry: %w", err)
	} else if blocked {
		return nil
	}
	if p.needKey && (p.lastKey.IsZero() ||
		(!p.keyRequested && now.Sub(p.lastKey) >= 100*time.Millisecond) ||
		now.Sub(p.lastKey) >= 250*time.Millisecond) {
		if e := p.encoder.ForceIDR(); e != nil {
			return fmt.Errorf("camera force IDR: %w", e)
		}
		p.stats.KeyRequests++
		p.keyRequested = true
		p.lastKey = now
	}
	encodedCallbacks := 0
	_, e := p.encoder.Drain(func(packet media.Encoded) error {
		encodedCallbacks++
		if p.retry.active || encodedCallbacks > 256 {
			return fmt.Errorf("encoder ignored bounded drain")
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if packet.PTS < 0 || (p.haveEncoded && packet.PTS <= p.stats.LastEncodedPTS) {
			return fmt.Errorf("nonmonotonic encoded camera timestamp")
		}
		if m := p.contract; m != nil {
			if e := m.validatePacket(packet); e != nil {
				return e
			}
			skipped, residence, e := m.completeAt(packet.PTS, now)
			if e != nil {
				return e
			}
			if residence > p.stats.MaxEncoderResidenceMicros {
				p.stats.MaxEncoderResidenceMicros = residence
			}
			p.stats.EncoderSkipped += skipped
			p.stats.PendingFrames = uint32(m.count)
			if !p.haveEncoded {
				m.firstEncoded = packet.PTS
			}
			p.stats.EncoderFPSMilli = measuredRateMilli(p.stats.Encoded+1, m.firstEncoded, packet.PTS)
		}
		p.haveEncoded = true
		p.stats.LastEncodedPTS = packet.PTS
		p.stats.Encoded++
		p.stats.EncodedBytes += uint64(len(packet.Data))
		if uint64(len(packet.Data)) > p.stats.PeakAccessUnitBytes {
			p.stats.PeakAccessUnitBytes = uint64(len(packet.Data))
		}
		if p.needKey && !packet.Key {
			p.stats.EncodedDropped++
			return nil
		}
		if e := p.sink.Submit(packet); e != nil {
			if !wouldBlock(e) {
				return e
			}
			p.stats.USBBackpressure++
			if err := p.retry.retain(packet, now); err != nil {
				return err
			}
			p.stats.USBPendingFrames = 1
			return errPauseEncodedDrain
		}
		p.acceptedUSB(packet, now)
		return nil
	})
	if e != nil && !wouldBlock(e) && !onlyQueueStatus(e, func(err error) bool { return err == errPauseEncodedDrain }) {
		return fmt.Errorf("camera encode/output: %w", e)
	}
	// Hold off raw submissions AND encoded drain until this complete AU can be
	// sent. SharedProvider continues capturing into its one-latest-frame slot.
	if p.retry.active {
		return nil
	}
	captureCallbacks := 0
	_, e = p.source.Drain(func(im media.Image) error {
		captureCallbacks++
		if captureCallbacks > 256 {
			return fmt.Errorf("source ignored bounded capture drain")
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if im.PTS < 0 || (p.haveCapture && im.PTS <= p.stats.LastCapturePTS) {
			return fmt.Errorf("nonmonotonic sensor timestamp")
		}
		if m := p.contract; m != nil {
			if e := m.validateImage(im); e != nil {
				return e
			}
			if !p.haveCapture {
				m.firstCapture = im.PTS
			}
			p.stats.SensorFPSMilli = measuredRateMilli(p.stats.Captured+1, m.firstCapture, im.PTS)
		}
		p.haveCapture = true
		if im.Lease != nil {
			previous, next := p.stats.SourceSensor.FrameCount, im.Lease.Sensor.FrameCount
			if previous != 0 && next != 0 {
				if step := int32(next - previous); step > 1 {
					p.stats.SourceSequenceGaps += uint64(step - 1)
				}
			}
			p.stats.SourceSensor = im.Lease.Sensor
			p.stats.SourceStages = im.Lease.CameraStages
		}
		p.stats.LastCapturePTS = im.PTS
		p.stats.Captured++
		if m := p.contract; m != nil && m.count == len(m.pending) {
			p.stats.RawDropped++
			return nil
		}
		if e := p.encoder.Submit(im); e != nil {
			if wouldBlock(e) {
				p.stats.EncoderBusy++
				p.stats.RawDropped++
				return nil
			}
			return e
		}
		p.stats.Submitted++
		if m := p.contract; m != nil {
			m.pending[m.count] = im.PTS
			m.pendingAt[m.count] = now
			m.count++
			p.stats.PendingFrames = uint32(m.count)
			if p.stats.PendingFrames > p.stats.PeakPendingFrames {
				p.stats.PeakPendingFrames = p.stats.PendingFrames
			}
		}
		return nil
	})
	if e != nil && !wouldBlock(e) {
		return fmt.Errorf("camera capture/input: %w", e)
	}
	return nil
}

// Teardown is camera-local: no gadget, UDC, monitor or audio API is exposed here.
// On unsafe STREAMOFF failure, the backend must quarantine maps and return error.
func (p *Pipeline) Close() error {
	if p == nil {
		return nil
	}
	if p.closed {
		return p.closeErr
	}
	p.closed = true
	p.retry.clear()
	p.retry.bytes = nil
	p.stats.USBPendingFrames = 0
	// MFC may still read shared camera DMA planes. Retire it before the source.
	b := p.encoder.Close()
	a := p.source.Close()
	p.closeErr = errors.Join(a, b)
	return p.closeErr
}

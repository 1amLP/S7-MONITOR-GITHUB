//go:build linux && (amd64 || arm64)

package mediacodec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"perimode/native/internal/media"
)

// APIs intentionally match the existing monitor/camera runtime interfaces.
// Workers use fixed Exynos component names; no createDecoderByType/FFmpeg fallback.
type Decoder struct {
	mu                   sync.Mutex
	s                    *session
	closed               bool
	closeErr, errorState error
	lastIn, lastOut      uint64
	haveIn, haveOut      bool
}

func OpenDecoder(ctx context.Context, first []byte, pts uint64, fps uint32) (*Decoder, error) {
	if e := media.Validate720pKey(first); e != nil {
		return nil, e
	}
	if (fps != 30 && fps != 60) || pts > 1<<63-1 {
		return nil, fmt.Errorf("unsupported MediaCodec monitor mode/timestamp")
	}
	s, e := nativeStart(ctx, roleDecode)
	if e != nil {
		return nil, e
	}
	return openDecoder(s, first, pts, fps)
}
func openDecoder(s *session, first []byte, pts uint64, fps uint32) (*Decoder, error) {
	d := &Decoder{s: s}
	config, picture, e := decoderConfiguration(first)
	if e != nil {
		return nil, errors.Join(e, d.Close())
	}
	_, _, e = s.call(header{op: opOpen, flags: roleDecode, width: 1280, height: 720, fps: fps}, config, 3*time.Second)
	if e == nil {
		// Startup may have no input slot yet. Only this first complete access unit is
		// retained. Normal Submit stays nonblocking at the MediaCodec API boundary.
		end := time.Now().Add(time.Second)
		for {
			e = d.Submit(picture, pts)
			if !errors.Is(e, syscall.EAGAIN) || time.Now().After(end) {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	if e != nil {
		return nil, errors.Join(e, d.Close())
	}
	return d, nil
}
func (d *Decoder) Submit(data []byte, pts uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return os.ErrClosed
	}
	if d.errorState != nil {
		return d.errorState
	}
	if len(data) < 4 || len(data) > 1<<20 || pts > 1<<63-1 || (d.haveIn && pts <= d.lastIn) {
		return fmt.Errorf("invalid MediaCodec AU/timestamp")
	}
	if _, e := media.SplitAnnexB(data); e != nil {
		return e
	}
	_, _, e := d.s.call(header{op: opSubmit, pts: pts}, data, 300*time.Millisecond)
	if e == nil {
		d.lastIn = pts
		d.haveIn = true
	} else if !errors.Is(e, syscall.EAGAIN) {
		d.errorState = e
	}
	return e
}
func (d *Decoder) Drain(emit func(media.Image) error) (int, error)       { return d.drain(emit, false) }
func (d *Decoder) DrainLatest(emit func(media.Image) error) (int, error) { return d.drain(emit, true) }
func (d *Decoder) drain(emit func(media.Image) error, latest bool) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, os.ErrClosed
	}
	if d.errorState != nil {
		return 0, d.errorState
	}
	if emit == nil {
		return 0, fmt.Errorf("nil MediaCodec image consumer")
	}
	var im media.Image
	count := 0
	for i := 0; i < 4; i++ {
		h, b, e := d.s.call(header{op: opDrain}, nil, 300*time.Millisecond)
		if errors.Is(e, syscall.EAGAIN) {
			break
		}
		if e != nil {
			d.errorState = e
			return count, e
		}
		if h.width != 1280 || h.height != 720 || h.size != 1280*720*3/2 || h.count != 1 || h.flags&2 != 0 ||
			(d.haveOut && h.pts <= d.lastOut) || !d.haveIn || h.pts > d.lastIn {
			e = fmt.Errorf("MediaCodec decoded geometry/timestamp mismatch")
			d.errorState = e
			return count, e
		}
		d.lastOut = h.pts
		d.haveOut = true
		count++
		// call returns session-owned Go storage, not the mapped worker slot.
		// The next successful drain overwrites it with the newer picture, which
		// is exactly DrainLatest's contract. EAGAIN/CLOSE have no payload and
		// cannot overwrite it; a retained slice keeps storage alive after Close.
		// emit is synchronous. Consumers keeping pixels beyond another drain
		// must copy them (the presenter already does so into its owned mailbox).
		im = media.Image{Width: 1280, Height: 720, StrideY: 1280, StrideUV: 1280, Y: b[:1280*720], UV: b[1280*720:], PTS: int64(h.pts)}
		if !latest {
			if e = emit(im); e != nil {
				return count, e
			}
		}
	}
	if latest && count > 0 {
		return count, emit(im)
	}
	return count, nil
}
func (d *Decoder) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.closed {
		d.closed = true
		d.closeErr = d.s.close()
	}
	return d.closeErr
}

type Encoder struct {
	mu                   sync.Mutex
	s                    *session
	cfg                  media.EncodeSettings
	assembler            *media.AccessUnitAssembler
	closed               bool
	closeErr, errorState error
	lastIn, lastOut      int64
	haveIn, haveOut      bool
}

func OpenEncoder(ctx context.Context, c media.EncodeSettings) (*Encoder, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	if c.FPS > 60 || c.GOP%c.FPS != 0 {
		return nil, fmt.Errorf("MediaCodec high-FPS/GOP not admitted")
	}
	s, e := nativeStart(ctx, roleEncode)
	if e != nil {
		return nil, e
	}
	return openEncoder(s, c)
}
func openEncoder(s *session, c media.EncodeSettings) (*Encoder, error) {
	return openEncoderFlags(s, c, 0)
}
func openEncoderFlags(s *session, c media.EncodeSettings, flags uint32) (*Encoder, error) {
	assembler, e := media.NewAccessUnitAssembler(c)
	if e != nil {
		return nil, errors.Join(e, s.close())
	}
	enc := &Encoder{s: s, cfg: c, assembler: assembler}
	_, _, e = s.call(header{op: opOpen, flags: roleEncode | flags, width: c.Width, height: c.Height, fps: c.FPS, bitrate: c.Bitrate, gop: c.GOP}, nil, 3*time.Second)
	if e != nil {
		return nil, errors.Join(e, enc.Close())
	}
	return enc, nil
}
func (e *Encoder) Submit(im media.Image) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return os.ErrClosed
	}
	if e.errorState != nil {
		return e.errorState
	}
	w, h := int(e.cfg.Width), int(e.cfg.Height)
	if im.Width != w || im.Height != h || im.StrideY < w || im.StrideY > 8192 || im.StrideUV < w || im.StrideUV > 8192 ||
		len(im.Y) < (h-1)*im.StrideY+w || len(im.UV) < (h/2-1)*im.StrideUV+w || im.PTS < 0 || (e.haveIn && im.PTS <= e.lastIn) {
		return fmt.Errorf("invalid MediaCodec NV12/timestamp")
	}
	err := e.s.submitPixels(im, 300*time.Millisecond)
	if err == nil {
		e.lastIn = im.PTS
		e.haveIn = true
	} else if !errors.Is(err, syscall.EAGAIN) {
		e.errorState = err
	}
	return err
}
func (e *Encoder) Drain(emit func(media.Encoded) error) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return 0, os.ErrClosed
	}
	if e.errorState != nil {
		return 0, e.errorState
	}
	if emit == nil {
		return 0, fmt.Errorf("nil MediaCodec packet consumer")
	}
	count := 0
	for i := 0; i < 4; i++ {
		h, b, err := e.s.call(header{op: opDrain}, nil, 300*time.Millisecond)
		if errors.Is(err, syscall.EAGAIN) {
			break
		}
		if err != nil {
			e.errorState = err
			return count, err
		}
		packet, picture, err := e.assembler.Push(b, int64(h.pts))
		if err == nil && picture {
			if h.flags&2 != 0 || h.count != 1 || !e.haveIn || packet.PTS > e.lastIn || (e.haveOut && packet.PTS <= e.lastOut) {
				err = fmt.Errorf("MediaCodec encoded timestamp/flags mismatch")
			}
		}
		if err != nil {
			e.errorState = err
			return count, err
		}
		if !picture {
			continue
		}
		e.lastOut = packet.PTS
		e.haveOut = true
		count++
		// Stop immediately when the downstream queue says to stop. No dependent AU
		// is discarded by another dequeue on the next loop iteration.
		if err = emit(packet); err != nil {
			return count, err
		}
	}
	return count, nil
}
func (e *Encoder) ForceIDR() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return os.ErrClosed
	}
	if e.errorState != nil {
		return e.errorState
	}
	_, _, err := e.s.call(header{op: opIDR}, nil, 300*time.Millisecond)
	if err != nil {
		e.errorState = err
	}
	return err
}
func (e *Encoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.closed {
		e.closed = true
		e.closeErr = e.s.close()
	}
	return e.closeErr
}

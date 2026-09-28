//go:build linux && (amd64 || arm64)

// Package audio implements native ALSA PCM, with no Android audio service.
package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"perimode/native/internal/linuxio"
)

const (
	hwRefine    = 0xc2604110
	hwParams    = 0xc2604111
	swParams    = 0xc0884113
	prepare     = 0x4140
	start       = 0x4142
	drop        = 0x4143
	writeFrames = 0x40184150
	readFrames  = 0x80184151
)

var le = binary.LittleEndian

// LP64 snd_pcm_hw_params. Arrays of uint64 guarantee union-compatible alignment.
type HWParams [76]uint64
type SWParams [17]uint64

func (h *HWParams) bytes() []byte { return unsafe.Slice((*byte)(unsafe.Pointer(h)), 608) }
func (s *SWParams) bytes() []byte { return unsafe.Slice((*byte)(unsafe.Pointer(s)), 136) }

type Transfer struct {
	Result int64
	Buffer uint64
	Frames uint64
}
type PCMConfig struct{ Channels, Rate, Period, Periods uint32 }

func (c PCMConfig) Validate() error {
	if (c.Channels != 1 && c.Channels != 2) || c.Rate != 48000 || c.Period < 120 || c.Period > 960 || c.Periods < 3 || c.Periods > 8 {
		return fmt.Errorf("unsupported native PCM configuration")
	}
	return nil
}
func NewHW(c PCMConfig) (HWParams, error) {
	var h HWParams
	if err := c.Validate(); err != nil {
		return h, err
	}
	p := h.bytes()
	for i := 0; i < 3; i++ {
		for j := 0; j < 8; j++ {
			le.PutUint32(p[4+i*32+j*4:], ^uint32(0))
		}
	}
	for i := 0; i < 12; i++ {
		le.PutUint32(p[260+i*12+4:], ^uint32(0))
	}
	// RW_INTERLEAVED, S16_LE, STD subformat. No negotiated resampling.
	for index, value := range []uint32{3, 2, 0} {
		clear(p[4+index*32 : 4+(index+1)*32])
		le.PutUint32(p[4+index*32+int(value/32)*4:], 1<<(value%32))
	}
	exact := func(param int, v uint32) {
		off := 260 + (param-8)*12
		le.PutUint32(p[off:], v)
		le.PutUint32(p[off+4:], v)
		le.PutUint32(p[off+8:], 4)
	}
	for param, val := range map[int]uint32{8: 16, 9: c.Channels * 16, 10: c.Channels, 11: c.Rate, 13: c.Period, 15: c.Periods, 17: c.Period * c.Periods} {
		exact(param, val)
	}
	le.PutUint32(p[0:], 1)
	le.PutUint32(p[512:], ^uint32(0))
	return h, nil
}
func ValidateHW(h *HWParams, c PCMConfig) error {
	p := h.bytes()
	for i, v := range []uint32{3, 2, 0} {
		for j := 0; j < 8; j++ {
			want := uint32(0)
			if j == int(v/32) {
				want = 1 << (v % 32)
			}
			if le.Uint32(p[4+i*32+j*4:]) != want {
				return fmt.Errorf("ALSA changed PCM access/format")
			}
		}
	}
	for param, v := range map[int]uint32{8: 16, 9: c.Channels * 16, 10: c.Channels, 11: c.Rate, 13: c.Period, 15: c.Periods, 17: c.Period * c.Periods} {
		off := 260 + (param-8)*12
		if le.Uint32(p[off:]) != v || le.Uint32(p[off+4:]) != v || le.Uint32(p[off+8:])&0xb != 0 {
			return fmt.Errorf("ALSA changed PCM parameter %d", param)
		}
	}
	return nil
}
func NewSW(c PCMConfig) SWParams {
	var sw SWParams
	p := sw.bytes()
	startPeriods := c.Periods / 2
	if startPeriods > 4 {
		startPeriods = 4
	}
	le.PutUint32(p[4:], 1)
	le.PutUint64(p[16:], uint64(c.Period))
	le.PutUint64(p[24:], 1)
	le.PutUint64(p[32:], uint64(c.Period*startPeriods))
	le.PutUint64(p[40:], uint64(c.Period*c.Periods))
	boundary := uint64(c.Period * c.Periods)
	for boundary <= (1<<62)/2 {
		boundary *= 2
	}
	le.PutUint64(p[64:], boundary)
	return sw
}

type PCM struct {
	file    *os.File
	config  PCMConfig
	capture bool
	closed  bool
}

func OpenPCM(card, device int, capture bool, c PCMConfig) (p *PCM, err error) {
	if card < 0 || card > 15 || device < 0 || device > 31 {
		return nil, fmt.Errorf("PCM address rejected")
	}
	h, e := NewHW(c)
	if e != nil {
		return nil, e
	}
	suffix := "p"
	if capture {
		suffix = "c"
	}
	f, e := os.OpenFile(fmt.Sprintf("/dev/snd/pcmC%dD%d%s", card, device, suffix), os.O_RDWR|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	p = &PCM{file: f, config: c, capture: capture}
	owner := p
	defer func() {
		if err != nil {
			err = errors.Join(err, owner.Close())
		}
	}()
	for _, req := range []uintptr{hwRefine, hwParams} {
		if err = p.call(req, unsafe.Pointer(&h)); err != nil {
			return nil, fmt.Errorf("ALSA setup %#x: %w", req, err)
		}
		if err = ValidateHW(&h, c); err != nil {
			return nil, err
		}
	}
	sw := NewSW(c)
	if err = p.call(swParams, unsafe.Pointer(&sw)); err != nil {
		return nil, err
	}
	if err = p.Recover(); err != nil {
		return nil, err
	}
	return p, nil
}
func (p *PCM) call(req uintptr, ptr unsafe.Pointer) error {
	return linuxio.Ioctl(int(p.file.Fd()), req, ptr)
}
func (p *PCM) Recover() error {
	if p.closed {
		return os.ErrClosed
	}
	if e := p.call(prepare, nil); e != nil {
		return e
	}
	if p.capture {
		return p.call(start, nil)
	}
	return nil
}
func (p *PCM) Transfer(data []byte) (int, error) {
	if p.closed {
		return 0, os.ErrClosed
	}
	fb := int(p.config.Channels) * 2
	if len(data) == 0 || len(data)%fb != 0 || len(data) > 1<<20 {
		return 0, fmt.Errorf("invalid PCM block")
	}
	x := Transfer{Buffer: uint64(uintptr(unsafe.Pointer(&data[0]))), Frames: uint64(len(data) / fb)}
	req := uintptr(writeFrames)
	if p.capture {
		req = readFrames
	}
	err := p.call(req, unsafe.Pointer(&x))
	runtime.KeepAlive(data)
	if err != nil {
		return 0, err
	}
	if x.Result < 0 {
		return 0, syscall.Errno(-x.Result)
	}
	if uint64(x.Result) > x.Frames {
		return 0, fmt.Errorf("ALSA returned excessive frames")
	}
	return int(x.Result) * fb, nil
}

// TransferContext returns on EPIPE/ESTRPIPE. The owner discards its queue before
// re-preparing both ends; stale audio is never replayed after an underrun.
func (p *PCM) TransferContext(ctx context.Context, data []byte) (int, error) {
	pos := 0
	deadline := time.Now().Add(500 * time.Millisecond)
	for pos < len(data) {
		if err := ctx.Err(); err != nil {
			return pos, err
		}
		n, e := p.Transfer(data[pos:])
		pos += n
		if e != nil && e != syscall.EAGAIN && e != syscall.EINTR {
			return pos, e
		}
		if n > 0 {
			deadline = time.Now().Add(500 * time.Millisecond)
		}
		if n == 0 {
			if time.Now().After(deadline) {
				return pos, fmt.Errorf("PCM made no progress for 500ms")
			}
			select {
			case <-ctx.Done():
				return pos, ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
	}
	return pos, nil
}

// Prime starts playback with silence and leaves room for a delayed producer.
// These frames are never counted as captured or delivered audio.
func (p *PCM) Prime(ctx context.Context, frames int) error {
	if p == nil || p.capture || frames < 1 || frames > int(p.config.Period*p.config.Periods)/2 {
		return fmt.Errorf("invalid playback prime")
	}
	silence := make([]byte, frames*int(p.config.Channels)*2)
	_, err := p.TransferContext(ctx, silence)
	return err
}
func (p *PCM) Close() error {
	if p == nil || p.closed {
		return nil
	}
	e := p.call(drop, nil)
	ce := p.file.Close()
	p.closed = true
	if e == syscall.EBADFD {
		e = nil
	}
	return errors.Join(e, ce)
}

// Delay returns queued playback frames according to ALSA, not a wall-clock guess.
func (p *PCM) Delay() (int64, error) {
	if p.closed || p.capture {
		return 0, fmt.Errorf("playback delay unavailable")
	}
	var value int64
	err := p.call(0x80084121, unsafe.Pointer(&value))
	return value, err
}

// Drop releases pending samples before PREPARE. A failed DROP must not be
// followed by reusing stale buffers. EBADFD means the PCM is already stopped.
func (p *PCM) Drop() error {
	if p == nil || p.closed {
		return os.ErrClosed
	}
	e := p.call(drop, nil)
	if errors.Is(e, syscall.EBADFD) {
		return nil
	}
	return e
}

//go:build linux && (amd64 || arm64)

package media

// Native V4L2 stateful encoder. No MediaCodec/Camera2 or software H.264 fallback.
// This owns the MFC queues only; it does not pretend to configure a FIMC-IS sensor.
import (
	"errors"
	"fmt"
	"perimode/native/pkg/cameramode"
	"os"
	"syscall"
	"unsafe"
)

const (
	SetControl     = 0xc008561c
	QueryControl   = 0xc0445624
	SetParameters  = 0xc0cc5616
	cidBitrate     = 0x009909cf
	cidBFrames     = 0x009909ca
	cidGOP         = 0x009909cb
	cidH264Profile = 0x00990a6b
	cidH264Level   = 0x00990a67
)

type QueryCtrl struct {
	ID, Type                        uint32
	Name                            [32]byte
	Minimum, Maximum, Step, Default int32
	Flags                           uint32
	Reserved                        [2]uint32
}
type StreamParameters struct {
	Type uint32
	Raw  [200]byte
}

type EncodeSettings struct{ Width, Height, FPS, Bitrate, GOP uint32 }

func (c EncodeSettings) Validate() error {
	valid := (cameramode.Mode{Width: c.Width, Height: c.Height, FPS: c.FPS}).Known()
	if !valid || c.Bitrate < 1_000_000 || c.Bitrate > 60_000_000 || c.GOP < 1 || c.GOP > c.FPS*5 {
		return fmt.Errorf("unsupported native encode settings: %+v", c)
	}
	return nil
}

// V4L2 enum values; choose a High-profile level by coded macroblock extent,
// processing rate and bitrate, not width alone. See CAMERA_MATRIX_RU.md.
func (c EncodeSettings) Level() int32 {
	blocks := uint64((c.Width+15)/16) * uint64((c.Height+15)/16)
	rate := blocks * uint64(c.FPS)
	levels := []struct {
		value       int32
		frame, rate uint64
		bitrate     uint32
	}{
		{9, 3600, 108000, 17_500_000},    // 3.1
		{11, 8192, 245760, 25_000_000},   // 4.0
		{13, 8704, 522240, 62_500_000},   // 4.2
		{14, 22080, 589824, 168_750_000}, // 5.0
		{15, 36864, 983040, 300_000_000}, // 5.1
	}
	for _, l := range levels {
		if blocks <= l.frame && rate <= l.rate && c.Bitrate <= l.bitrate {
			return l.value
		}
	}
	return 15 // Validate rejects all unsupported geometries/rates before use.
}

type Encoded struct {
	Data []byte
	PTS  int64
	Key  bool
}
type Encoder struct {
	queue         Decoder // shared, tested MMAP and STREAMOFF ownership rules
	uploads       []nv12Upload
	outputPlanes  [2]Plane
	capturePlanes [1]Plane
	settings      EncodeSettings
	raw           Format
	assembler     *AccessUnitAssembler
	tags          encoderTags
	lastPTS       uint64
	havePTS       bool
	stopped       bool
	closeErr      error
}

func EncoderPath(ds []Device) (string, error) {
	for _, d := range ds {
		if d.Error == "" && safeMFC(d) && Has(d.CaptureFormats, "H264") &&
			(Has(d.OutputFormats, "NM12") || Has(d.OutputFormats, "NV12")) {
			return d.Path, nil
		}
	}
	return "", fmt.Errorf("no native MFC H264 encoder with linear NV12 input")
}
func safeMFC(d Device) bool {
	// Reuse the decoder's identity check, but reverse raw/coded queue directions.
	x := d
	x.OutputFormats = []string{"H264"}
	x.CaptureFormats = []string{"NV12"}
	_, err := DecoderPath([]Device{x})
	return err == nil
}
func OpenEncoder(path string, c EncodeSettings) (enc *Encoder, err error) {
	if IsQuarantined() {
		return nil, ErrQuarantined
	}
	if err = c.Validate(); err != nil {
		return nil, err
	}
	if err = requireEncoderKernel(); err != nil {
		return nil, err
	}
	if err = VerifyABI(); err != nil {
		return nil, err
	}
	dev := Probe(path)
	if _, err = EncoderPath([]Device{dev}); err != nil {
		return nil, err
	}
	f, e := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	enc = &Encoder{settings: c}
	enc.queue.file = f
	enc.queue.memory = MemoryDMABuf
	owner := enc
	defer func() {
		if err != nil {
			err = errors.Join(err, owner.Close())
		}
	}()
	coded := Format{Type: Capture}
	coded.Set(c.Width, c.Height, H264, 1)
	coded.SetPlaneSize(0, 4<<20)
	if err = ioctl(enc.queue.fd(), SFormat, unsafe.Pointer(&coded)); err != nil {
		return nil, fmt.Errorf("encoder coded S_FMT: %w", err)
	}
	if coded.PixelFormat() != H264 || coded.Planes() != 1 {
		return nil, fmt.Errorf("encoder changed compressed format")
	}
	enc.raw = Format{Type: Output}
	enc.raw.Set(c.Width, c.Height, NV12M, 2)
	if !Has(dev.OutputFormats, "NM12") {
		enc.raw.Set(c.Width, c.Height, NV12, 1)
	}
	if err = ioctl(enc.queue.fd(), SFormat, unsafe.Pointer(&enc.raw)); err != nil {
		return nil, fmt.Errorf("encoder raw S_FMT: %w", err)
	}
	// S_FMT in the vendor encoder does not populate all negotiated strides.
	// G_FMT supplies the actual allocation layout before importing ION pools.
	enc.raw = Format{Type: Output}
	if err = ioctl(enc.queue.fd(), GFormat, unsafe.Pointer(&enc.raw)); err != nil {
		return nil, fmt.Errorf("encoder raw G_FMT: %w", err)
	}
	if err = validateRawFormat(enc.raw, c); err != nil {
		return nil, err
	}
	if err = configureEncoder(func(req uintptr, ptr unsafe.Pointer) error {
		return ioctl(enc.queue.fd(), req, ptr)
	}, c); err != nil {
		return nil, err
	}
	if err = queryEncoderValue(func(req uintptr, ptr unsafe.Pointer) error { return ioctl(enc.queue.fd(), req, ptr) }, Control{ID: cidMFCFrameTag, Value: 1}); err != nil {
		return nil, err
	}
	req := Request{Type: Output, Memory: MemoryDMABuf, Count: 3}
	if err = ioctl(enc.queue.fd(), RequestBuffers, unsafe.Pointer(&req)); err != nil {
		return nil, err
	}
	if req.Count < 2 || req.Count > 8 {
		return nil, fmt.Errorf("unbounded encoder input pool")
	}
	enc.queue.output = make([]mapping, req.Count)
	if enc.queue.capture, err = enc.queue.allocateDMA(Capture, 4, []uint32{coded.PlaneSize(0)}); err != nil {
		return nil, err
	}
	for i := range enc.queue.capture {
		if err = enc.queue.queueCapture(i); err != nil {
			return nil, err
		}
	}
	// Start the coded queue first. OUTPUT starts only after the first raw frame is queued.
	t := Capture
	if err = ioctl(enc.queue.fd(), StreamOn, unsafe.Pointer(&t)); err != nil {
		return nil, err
	}
	enc.queue.capOn = true
	return enc, nil
}
func validateRawFormat(f Format, c EncodeSettings) error {
	if f.Width() != c.Width || f.Height() != c.Height ||
		!((f.PixelFormat() == NV12M && f.Planes() == 2) || (f.PixelFormat() == NV12 && f.Planes() == 1)) {
		return fmt.Errorf("native encoder changed raw geometry or format")
	}
	if f.Stride(0) < c.Width || f.Stride(0) > 16384 || (f.Planes() == 2 && f.Stride(1) != 0 && (f.Stride(1) < c.Width || f.Stride(1) > 16384)) {
		return fmt.Errorf("native encoder raw stride invalid")
	}
	return nil
}

func (e *Encoder) Submit(im Image) error {
	if e == nil || e.stopped {
		return os.ErrClosed
	}
	if im.PTS < 0 || (e.havePTS && uint64(im.PTS) <= e.lastPTS) {
		return fmt.Errorf("nonmonotonic camera PTS")
	}
	l := im.Lease
	if !l.Live() || l.Width != int(e.settings.Width) || l.Height != int(e.settings.Height) || l.PTS != im.PTS || len(l.Planes) != int(e.raw.Planes()) {
		return fmt.Errorf("native MFC requires a live matching DMA frame; CPU upload disabled")
	}
	for j, p := range l.Planes {
		stride := e.raw.Stride(j)
		if stride == 0 {
			stride = e.raw.Stride(0)
		}
		if p.Offset != 0 || p.Length < e.raw.PlaneSize(j) || p.Stride != stride {
			return fmt.Errorf("camera DMA layout differs from negotiated MFC input")
		}
	}
	for i := range e.queue.output {
		m := &e.queue.output[i]
		if m.queued {
			continue
		}
		if err := l.Retain(); err != nil {
			return err
		}
		tag, err := e.tags.reserve(im.PTS)
		if err != nil {
			return errors.Join(err, l.Release())
		}
		control := Control{ID: cidMFCFrameTag, Value: tag}
		if err = ioctl(e.queue.fd(), SetControl, unsafe.Pointer(&control)); err != nil {
			_, _ = e.tags.take(tag)
			return errors.Join(fmt.Errorf("MFC set input frame tag: %w", err), l.Release())
		}
		m.borrowed = l
		planes := make([]Plane, len(l.Planes))
		for j, p := range l.Planes {
			planes[j] = Plane{Memory: uint64(p.FD), Length: p.Length, Used: e.raw.PlaneSize(j)}
		}
		b := Buffer{Index: uint32(i), Type: Output, Memory: e.queue.memoryType(), Seconds: im.PTS / 1_000_000, Microseconds: im.PTS % 1_000_000}
		m.queued = true
		if err := bufferIoctl(e.queue.fd(), QueueBuffer, &b, planes); err != nil {
			if err == syscall.EAGAIN {
				m.queued = false
				m.borrowed = nil
				_, _ = e.tags.take(tag)
				return errors.Join(err, l.Release())
			}
			return err // Retain a partially submitted import until STREAMOFF.
		}
		m.queued = true
		e.lastPTS, e.havePTS = uint64(im.PTS), true
		if !e.queue.outOn {
			t := Output
			if err := ioctl(e.queue.fd(), StreamOn, unsafe.Pointer(&t)); err != nil {
				return err
			}
			e.queue.outOn = true
		}
		return nil
	}
	return syscall.EAGAIN
}
func (e *Encoder) ForceIDR() error {
	if e == nil || e.stopped {
		return os.ErrClosed
	}
	return forceEncoderKey(func(req uintptr, ptr unsafe.Pointer) error { return ioctl(e.queue.fd(), req, ptr) })
}
func (e *Encoder) Drain(emit func(Encoded) error) (int, error) {
	if e == nil || e.stopped {
		return 0, os.ErrClosed
	}
	if emit == nil {
		return 0, fmt.Errorf("nil encoder consumer")
	}
	// Pipeline drains before submitting its first raw frame. Samsung rejects
	// OUTPUT DQBUF with EINVAL until that frame has caused OUTPUT STREAMON.
	if !e.queue.outOn {
		return 0, nil
	}
	for i := 0; i < len(e.queue.output); i++ {
		b := Buffer{Type: Output, Memory: e.queue.memoryType()}
		clear(e.outputPlanes[:])
		p := e.outputPlanes[:int(e.raw.Planes())]
		err := bufferIoctl(e.queue.fd(), DequeueBuffer, &b, p)
		if err == syscall.EAGAIN {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("MFC encoder OUTPUT DQBUF: %w", err)
		}
		if int(b.Index) >= len(e.queue.output) || !e.queue.output[b.Index].queued {
			return 0, fmt.Errorf("invalid encoder input completion")
		}
		if err := e.queue.confirmUserPlanes(&e.queue.output[b.Index], p); err != nil {
			return 0, err
		}
		m := &e.queue.output[b.Index]
		m.queued = false
		if m.borrowed != nil {
			err := m.borrowed.Release()
			m.borrowed = nil
			if err != nil {
				return 0, err
			}
		}
	}
	count := 0
	for i := 0; i < len(e.queue.capture); i++ {
		b := Buffer{Type: Capture, Memory: e.queue.memoryType()}
		clear(e.capturePlanes[:])
		p := e.capturePlanes[:]
		err := bufferIoctl(e.queue.fd(), DequeueBuffer, &b, p)
		if err == syscall.EAGAIN {
			return count, nil
		}
		if err != nil {
			return count, fmt.Errorf("MFC encoder CAPTURE DQBUF: %w", err)
		}
		if int(b.Index) >= len(e.queue.capture) || !e.queue.capture[b.Index].queued {
			return count, fmt.Errorf("invalid encoder output completion")
		}
		m := &e.queue.capture[b.Index]
		if err := e.queue.confirmUserPlanes(m, p); err != nil {
			return count, err
		}
		m.queued = false
		if b.Flags&0x40 != 0 || p[0].Offset > p[0].Used || p[0].Used > uint32(len(m.planes[0])) {
			return count, errors.Join(fmt.Errorf("bad encoded payload"), e.queue.queueCapture(int(b.Index)))
		}
		data := m.planes[0][p[0].Offset:p[0].Used]
		// Output storage must not escape: emit receives an owned copy before QBUF.
		// Parse headers before querying a picture tag: sequence-header buffers
		// have no frame tag. Samsung does not copy timeval to encoded CAPTURE.
		packet, hasFrame, err := e.packet(data, 0)
		if err == nil && hasFrame {
			control := Control{ID: cidMFCFrameTag}
			if err = ioctl(e.queue.fd(), GetControl, unsafe.Pointer(&control)); err != nil {
				err = fmt.Errorf("MFC get encoded frame tag: %w", err)
			} else {
				packet.PTS, err = e.tags.take(control.Value)
			}
			if err == nil {
				err = emit(packet)
				count++
			}
		}
		qerr := e.queue.queueCapture(int(b.Index))
		if err != nil || qerr != nil {
			return count, errors.Join(err, qerr)
		}
	}
	return count, nil
}
func (e *Encoder) Close() error {
	if e == nil {
		return nil
	}
	if e.stopped {
		return e.closeErr
	}
	// Terminal for this Encoder even if queue.Close quarantines mappings.
	// Retrying Submit must not touch storage after a partial cleanup.
	e.stopped = true
	e.closeErr = e.queue.Close()
	return e.closeErr
}

// Driver timestamps have a bounded microsecond component. Validate BEFORE
// multiplication so overflow cannot become another submitted frame's PTS.
func encoderTimestamp(seconds, microseconds int64) (int64, error) {
	if seconds < 0 || seconds > (1<<63-1-999999)/1000000 ||
		microseconds < 0 || microseconds >= 1000000 {
		return 0, fmt.Errorf("invalid MFC encoder timestamp")
	}
	return seconds*1000000 + microseconds, nil
}

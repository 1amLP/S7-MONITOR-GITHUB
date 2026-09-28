//go:build linux && (amd64 || arm64)

package media

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	BGR32              uint32 = 0x34524742
	RGB32              uint32 = 0x34424752
	controlRotate      uint32 = 0x00980922
	controlCSCEquation uint32 = 0x00982065
	controlCSCRange    uint32 = 0x00982066
	scalerCapabilities        = 0x04003000
)

type ScalerStats struct {
	CopiedBytes uint64 `json:"cpu_copied_bytes"`
	Device      string `json:"device"`
	Frames      uint64 `json:"frames"`
	LastUS      int64  `json:"last_us"`
	MeanUS      int64  `json:"mean_us"`
	MaxUS       int64  `json:"max_us"`
	TargetBytes int    `json:"target_bytes"`
}

type RGBScaler struct {
	sourceWidth, sourceHeight int
	sourceColor               ColorDescription
	file                      *os.File
	device                    string
	sourceY, sourceUV         []byte
	targetBytes               int
	targetStride              int
	outOn, capOn              bool
	outRequested              bool
	capRequested              bool
	closed                    bool
	unsafeOwnership           bool
	sourceLease               *FrameLease
	copiedBytes               uint64
	queuedTarget              []byte
	targetDMABuf              *os.File
	captureMemory             uint32
	ioctlForTest              func(int, uintptr, unsafe.Pointer) error
	bufferForTest             func(uintptr, *Buffer, []Plane) error
	frames                    uint64
	lastUS, totalUS           int64
	maxUS                     int64
}

var ErrScalerQuarantined = errors.New("scaler ownership not returned; no reopen before reboot")

var scalerQuarantineMu sync.Mutex
var quarantinedScalers []*RGBScaler

func scalerQuarantined() bool {
	scalerQuarantineMu.Lock()
	defer scalerQuarantineMu.Unlock()
	return len(quarantinedScalers) != 0
}

func (s *RGBScaler) quarantine(err error) error {
	// Retain the file and mappings: even a file finalizer could release a device
	// whose DMA ownership could not be retired explicitly.
	if !s.unsafeOwnership {
		s.unsafeOwnership = true
		scalerQuarantineMu.Lock()
		quarantinedScalers = append(quarantinedScalers, s)
		scalerQuarantineMu.Unlock()
	}
	return errors.Join(ErrScalerQuarantined, err)
}

func (s *RGBScaler) captureMode() uint32 {
	if s.captureMemory == MemoryDMABuf {
		return MemoryDMABuf
	}
	return MemoryUserPtr
}

func (s *RGBScaler) ioctlCall(fd int, req uintptr, p unsafe.Pointer) error {
	if s.ioctlForTest != nil {
		return s.ioctlForTest(fd, req, p)
	}
	return ioctl(fd, req, p)
}

func (s *RGBScaler) bufferCall(req uintptr, b *Buffer, planes []Plane) error {
	if s.bufferForTest != nil {
		b.Length = uint32(len(planes))
		return s.bufferForTest(req, b, planes)
	}
	return bufferIoctl(int(s.file.Fd()), req, b, planes)
}

func scalerFormat(t, width, height, pixel uint32, planes byte) Format {
	f := Format{Type: t}
	f.Set(width, height, pixel, planes)
	// The host encoder produces BT.709 limited-range NV12.
	le.PutUint32(f.Raw[16:], 3)
	return f
}

func openRGBScaler(path string, width, height, rotation int, pixel uint32) (s *RGBScaler, err error) {
	return openRGBScalerMode(path, width, height, rotation, pixel, MemoryUserPtr)
}

func openRGBScalerMode(path string, width, height, rotation int, pixel uint32, captureMemory uint32) (s *RGBScaler, err error) {
	return openRGBScalerSource(path, width, height, rotation, pixel, captureMemory, 1280, 720, BT709Limited, false, false, 100)
}

func openRGBScalerSource(path string, width, height, rotation int, pixel uint32, captureMemory uint32, sw, sh int, color ColorDescription, flipX, flipY bool, zoom int) (s *RGBScaler, err error) {
	if scalerQuarantined() {
		return nil, ErrScalerQuarantined
	}
	if captureMemory != MemoryUserPtr && captureMemory != MemoryDMABuf {
		return nil, fmt.Errorf("invalid scaler capture memory mode")
	}
	if width != 1440 || height != 2560 || (rotation != 0 && rotation != 90 && rotation != 180 && rotation != 270) || (pixel != BGR32 && pixel != RGB32) {
		return nil, fmt.Errorf("unsupported S7 scaler geometry")
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	s = &RGBScaler{file: f, device: path, captureMemory: captureMemory, sourceWidth: sw, sourceHeight: sh, sourceColor: color}
	owned := s
	defer func() {
		if err != nil {
			err = errors.Join(err, owned.Close())
			s = nil
		}
	}()
	var capability Capability
	if err = ioctl(int(f.Fd()), QueryCap, unsafe.Pointer(&capability)); err != nil {
		return nil, fmt.Errorf("scaler QUERYCAP: %w", err)
	}
	driver := strings.TrimRight(string(capability.Driver[:]), "\x00")
	caps := capability.Capabilities
	if caps&0x80000000 != 0 {
		caps = capability.DeviceCaps
	}
	if !strings.Contains(strings.ToLower(driver), "scaler") || caps&scalerCapabilities != scalerCapabilities {
		return nil, fmt.Errorf("unexpected scaler identity/capabilities %q 0x%x", driver, caps)
	}
	source := scalerFormat(Output, uint32(sw), uint32(sh), NV12M, 2)
	csc := int32(3)
	if color == BT601Limited {
		csc = 1
		le.PutUint32(source.Raw[16:], 1)
	}
	if err = ioctl(int(f.Fd()), SFormat, unsafe.Pointer(&source)); err != nil {
		return nil, fmt.Errorf("scaler S_FMT NV12M: %w", err)
	}
	if source.Width() != uint32(sw) || source.Height() != uint32(sh) || source.PixelFormat() != NV12M || source.Planes() != 2 || source.PlaneSize(0) != uint32(sw*sh) || source.PlaneSize(1) != uint32(sw*sh/2) {
		return nil, fmt.Errorf("scaler changed NV12M layout")
	}
	target := scalerFormat(Capture, uint32(width), uint32(height), pixel, 1)
	if err = ioctl(int(f.Fd()), SFormat, unsafe.Pointer(&target)); err != nil {
		return nil, fmt.Errorf("scaler S_FMT RGB: %w", err)
	}
	if target.Width() != uint32(width) || target.Height() != uint32(height) || target.PixelFormat() != pixel || target.Planes() != 1 || target.Stride(0) != uint32(width*4) || target.PlaneSize(0) != uint32(width*height*4) {
		return nil, fmt.Errorf("scaler changed RGB layout")
	}
	if rotation == 0 || rotation == 180 {
		// e418 scaler S_CROP on CAPTURE sets the destination window. Keep a
		// 16:9 image in portrait; the GPU clears the surrounding bars.
		crop := struct {
			Type          uint32
			Left, Top     int32
			Width, Height uint32
		}{Type: Capture, Top: 875, Width: 1440, Height: 810}
		if err = ioctl(int(f.Fd()), 0x4014563c, unsafe.Pointer(&crop)); err != nil {
			return nil, fmt.Errorf("scaler portrait crop: %w", err)
		}
		got := crop
		if err = ioctl(int(f.Fd()), 0xc014563b, unsafe.Pointer(&got)); err != nil || got != crop {
			return nil, fmt.Errorf("scaler portrait crop readback: %v", err)
		}
	}
	controls := []Control{{ID: controlRotate, Value: int32(rotation)}, {ID: controlCSCEquation, Value: csc}, {ID: controlCSCRange, Value: 0}}
	if flipX {
		controls = append(controls, Control{ID: 0x00980914, Value: 1})
	}
	if flipY {
		controls = append(controls, Control{ID: 0x00980915, Value: 1})
	}
	for _, control := range controls {
		if err = ioctl(int(f.Fd()), SetControl, unsafe.Pointer(&control)); err != nil {
			return nil, fmt.Errorf("scaler control 0x%x: %w", control.ID, err)
		}
	}
	if zoom != 100 {
		cw, ch := PreviewCrop(sw, sh, zoom)
		crop := struct {
			Type          uint32
			Left, Top     int32
			Width, Height uint32
		}{Output, int32((sw - cw) / 2 &^ 1), int32((sh - ch) / 2 &^ 1), uint32(cw), uint32(ch)}
		if err = ioctl(int(f.Fd()), 0x4014563c, unsafe.Pointer(&crop)); err != nil {
			return nil, err
		}
	}
	for _, t := range []uint32{Output, Capture} {
		memory := captureMemory
		r := Request{Count: 1, Type: t, Memory: memory}
		if err = ioctl(int(f.Fd()), RequestBuffers, unsafe.Pointer(&r)); err != nil {
			return nil, fmt.Errorf("scaler REQBUFS %d: %w", t, err)
		}
		if t == Output {
			s.outRequested = true
		} else {
			s.capRequested = true
		}
		if r.Count != 1 {
			return nil, fmt.Errorf("scaler returned %d buffers", r.Count)
		}
	}
	if captureMemory == MemoryUserPtr {
		if s.sourceY, err = scalerMapping(int(source.PlaneSize(0))); err != nil {
			return nil, err
		}
		if s.sourceUV, err = scalerMapping(int(source.PlaneSize(1))); err != nil {
			return nil, err
		}
	}
	s.targetBytes = int(target.PlaneSize(0))
	s.targetStride = int(target.Stride(0))
	return s, nil
}

func OpenRGBScaler(paths []string, width, height, rotation int, pixel uint32) (*RGBScaler, error) {
	return openRGBScalerCandidates(paths, width, height, rotation, pixel, MemoryUserPtr)
}

// Both queues import persistent DMA-BUFs. No NV12 staging allocation is made.
func OpenRGBScalerDMABuf(paths []string, width, height, rotation int, pixel uint32) (*RGBScaler, error) {
	return openRGBScalerCandidates(paths, width, height, rotation, pixel, MemoryDMABuf)
}

// Diagnostic-only reconstruction of a committed camera DMA frame. No pixel
// upload, CPU transform or persistent extra process is introduced by capture.
func OpenCameraRGBSnapshot(im Image, rotation int, flipX, flipY bool, zoom int) (*RGBScaler, error) {
	l := im.Lease
	if !l.Live() || l.Color != BT601Limited || len(l.Planes) != 2 || im.Width != l.Width || im.Height != l.Height || zoom < 100 || zoom > 400 || !(im.Width == 1280 && im.Height == 720 || im.Width == 1920 && im.Height == 1080 || im.Width == 2560 && im.Height == 1440) {
		return nil, fmt.Errorf("unsupported camera snapshot DMA layout")
	}
	return openRGBSnapshot(im, rotation, flipX, flipY, zoom)
}

func validateMonitorSnapshot(im Image) error {
	l := im.Lease
	if !l.Live() || l.Color != BT709Limited || len(l.Planes) != 2 || im.Width != l.Width || im.Height != l.Height || !(im.Width == 1280 && im.Height == 720 || im.Width == 2560 && im.Height == 1440) {
		return fmt.Errorf("unsupported monitor snapshot DMA layout")
	}
	return nil
}

func OpenMonitorRGBSnapshot(im Image, rotation int) (*RGBScaler, error) {
	if err := validateMonitorSnapshot(im); err != nil {
		return nil, err
	}
	return openRGBSnapshot(im, rotation, false, false, 100)
}

func openRGBSnapshot(im Image, rotation int, flipX, flipY bool, zoom int) (*RGBScaler, error) {
	var err error
	for _, path := range []string{"/dev/video50", "/dev/video51"} {
		s, e := openRGBScalerSource(path, 1440, 2560, rotation, BGR32, MemoryDMABuf, im.Width, im.Height, im.Lease.Color, flipX, flipY, zoom)
		if e == nil {
			return s, nil
		}
		err = errors.Join(err, e)
		if errors.Is(e, ErrScalerQuarantined) {
			break
		}
	}
	return nil, err
}

func openRGBScalerCandidates(paths []string, width, height, rotation int, pixel uint32, captureMemory uint32) (*RGBScaler, error) {
	if len(paths) == 0 || len(paths) > 4 {
		return nil, fmt.Errorf("invalid scaler candidate list")
	}
	var err error
	for _, path := range paths {
		if path != "/dev/video50" && path != "/dev/video51" {
			err = errors.Join(err, fmt.Errorf("unapproved scaler path %q", path))
			continue
		}
		s, e := openRGBScalerMode(path, width, height, rotation, pixel, captureMemory)
		if e == nil {
			return s, nil
		}
		err = errors.Join(err, fmt.Errorf("%s: %w", path, e))
		if errors.Is(e, ErrScalerQuarantined) {
			return nil, err
		}
	}
	return nil, err
}

func scalerMapping(size int) ([]byte, error) {
	if size <= 0 || size > 32<<20 {
		return nil, fmt.Errorf("invalid scaler mapping size")
	}
	b, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("scaler anonymous mapping: %w", err)
	}
	return b, nil
}

func scalerAddress(b []byte) uint64 {
	if len(b) == 0 {
		return 0
	}
	return uint64(uintptr(unsafe.Pointer(&b[0])))
}

func (s *RGBScaler) queue(t uint32, buffers [][]byte, used []uint32) error {
	if len(buffers) != len(used) || len(buffers) < 1 || len(buffers) > 2 {
		return fmt.Errorf("invalid scaler queue planes")
	}
	p := make([]Plane, len(buffers))
	for i, data := range buffers {
		if len(data) == 0 || used[i] > uint32(len(data)) {
			return fmt.Errorf("invalid scaler plane extent")
		}
		p[i] = Plane{Used: used[i], Length: uint32(len(data)), Memory: scalerAddress(data)}
	}
	b := Buffer{Index: 0, Type: t, Memory: MemoryUserPtr}
	if err := s.bufferCall(QueueBuffer, &b, p); err != nil {
		return fmt.Errorf("scaler QBUF %d: %w", t, err)
	}
	return nil
}

func (s *RGBScaler) dequeue(t uint32, buffers [][]byte, deadline time.Time) error {
	for {
		p := make([]Plane, len(buffers))
		b := Buffer{Type: t, Memory: MemoryUserPtr}
		err := s.bufferCall(DequeueBuffer, &b, p)
		if err == syscall.EAGAIN && time.Now().Before(deadline) {
			if e := waitVideo(int(s.file.Fd()), 1|4, deadline); e != nil {
				return e
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("scaler DQBUF %d: %w", t, err)
		}
		if b.Index != 0 || b.Flags&0x40 != 0 || len(p) != len(buffers) {
			return fmt.Errorf("invalid scaler completion type=%d index=%d flags=0x%x", t, b.Index, b.Flags)
		}
		for i := range p {
			if p[i].Memory != scalerAddress(buffers[i]) || p[i].Length != uint32(len(buffers[i])) || p[i].Offset > p[i].Used || p[i].Used > p[i].Length {
				return fmt.Errorf("invalid scaler completion plane %d", i)
			}
		}
		return nil
	}
}

func (s *RGBScaler) queueCaptureDMABuf(fd, bytes int) error {
	p := []Plane{{Length: uint32(bytes), Memory: uint64(uint32(fd))}}
	b := Buffer{Index: 0, Type: Capture, Memory: MemoryDMABuf}
	if err := s.bufferCall(QueueBuffer, &b, p); err != nil {
		return fmt.Errorf("scaler QBUF DMA-BUF CAPTURE: %w", err)
	}
	return nil
}

func (s *RGBScaler) dequeueCaptureDMABuf(fd, bytes int, deadline time.Time) error {
	for {
		p := []Plane{{}}
		b := Buffer{Type: Capture, Memory: MemoryDMABuf}
		err := s.bufferCall(DequeueBuffer, &b, p)
		if err == syscall.EAGAIN && time.Now().Before(deadline) {
			if e := waitVideo(int(s.file.Fd()), 1, deadline); e != nil {
				return e
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("scaler DQBUF DMA-BUF CAPTURE: %w", err)
		}
		if b.Index != 0 || b.Memory != MemoryDMABuf || b.Length != 1 || b.Flags&0x40 != 0 ||
			p[0].Memory != uint64(uint32(fd)) || p[0].Length != uint32(bytes) ||
			p[0].Offset > p[0].Used || p[0].Used > p[0].Length {
			return fmt.Errorf("invalid scaler DMA-BUF completion index=%d memory=%d flags=0x%x fd=%d bytes=%d", b.Index, b.Memory, b.Flags, int32(p[0].Memory), p[0].Length)
		}
		return nil
	}
}

func (s *RGBScaler) TransformTo(im Image, target []byte) error {
	return s.transform(im, target, -1, 0, MemoryUserPtr)
}

// TransformToDMABuf returns only after DQBUF gives the target back to userspace.
// A duplicate fd stays owned by the scaler until that point or a successful STREAMOFF.
func (s *RGBScaler) TransformToDMABuf(im Image, targetFD, targetBytes int) error {
	return s.transform(im, nil, targetFD, targetBytes, MemoryDMABuf)
}

func (s *RGBScaler) transform(im Image, target []byte, targetFD, targetBytes int, mode uint32) error {
	if s == nil || s.closed || s.file == nil {
		return os.ErrClosed
	}
	if s.unsafeOwnership {
		return ErrScalerQuarantined
	}
	if s.queuedTarget != nil || s.targetDMABuf != nil || s.sourceLease != nil {
		return fmt.Errorf("scaler still owns a queued target; close required")
	}
	if s.captureMode() != mode {
		return fmt.Errorf("scaler capture mode mismatch")
	}
	sw, sh, color := s.sourceWidth, s.sourceHeight, s.sourceColor
	if sw == 0 {
		sw, sh, color = 1280, 720, BT709Limited
	}
	if im.Width != sw || im.Height != sh || im.StrideY < sw || im.StrideUV < sw {
		return fmt.Errorf("invalid scaler source image")
	}
	if mode == MemoryUserPtr && (len(im.Y) < 719*im.StrideY+1280 || len(im.UV) < 359*im.StrideUV+1280) {
		return fmt.Errorf("invalid mapped scaler source")
	}
	if mode == MemoryUserPtr && (len(target) != s.targetBytes || len(target) == 0) {
		return fmt.Errorf("invalid direct scaler target")
	}
	if mode == MemoryDMABuf && (targetFD < 0 || targetBytes != s.targetBytes || targetBytes <= 0 || targetBytes > 32<<20) {
		return fmt.Errorf("invalid scaler DMA-BUF target")
	}
	if mode == MemoryDMABuf {
		var st syscall.Stat_t
		if err := syscall.Fstat(targetFD, &st); err != nil {
			return fmt.Errorf("inspect scaler DMA-BUF: %w", err)
		}
		if st.Size > 0 && st.Size < int64(targetBytes) {
			return fmt.Errorf("scaler DMA-BUF is shorter than capture frame")
		}
	}
	if mode == MemoryDMABuf {
		if !im.Lease.Live() || len(im.Lease.Planes) != 2 || im.Lease.Color != color {
			return fmt.Errorf("scaler DMA colorspace differs from configuration")
		}
		for _, p := range im.Lease.Planes {
			if p.Stride != uint32(sw) {
				return fmt.Errorf("monitor DMA stride mismatch")
			}
		}
		if err := im.Lease.Retain(); err != nil {
			return err
		}
		s.sourceLease = im.Lease
	} else {
		for y := 0; y < 720; y++ {
			copy(s.sourceY[y*1280:(y+1)*1280], im.Y[y*im.StrideY:y*im.StrideY+1280])
		}
		for y := 0; y < 360; y++ {
			copy(s.sourceUV[y*1280:(y+1)*1280], im.UV[y*im.StrideUV:y*im.StrideUV+1280])
		}
		s.copiedBytes += 1280 * 720 * 3 / 2
	}
	started := time.Now()
	if mode == MemoryDMABuf {
		dup, err := syscall.Dup(targetFD)
		if err != nil {
			return fmt.Errorf("retain scaler DMA-BUF: %w", err)
		}
		s.targetDMABuf = os.NewFile(uintptr(dup), "s7-scaler-target")
		if err = s.queueCaptureDMABuf(dup, targetBytes); err != nil {
			_ = s.targetDMABuf.Close()
			s.targetDMABuf = nil
			return err
		}
	} else {
		if err := s.queue(Capture, [][]byte{target}, []uint32{0}); err != nil {
			return err
		}
		s.queuedTarget = target
	}
	var inputErr error
	if mode == MemoryDMABuf {
		inputErr = s.queueDMAInput()
	} else {
		inputErr = s.queue(Output, [][]byte{s.sourceY, s.sourceUV}, []uint32{uint32(len(s.sourceY)), uint32(len(s.sourceUV))})
	}
	if inputErr != nil {
		return inputErr
	}
	if !s.outOn {
		t := Output
		if err := s.ioctlCall(int(s.file.Fd()), StreamOn, unsafe.Pointer(&t)); err != nil {
			return fmt.Errorf("scaler STREAMON OUTPUT: %w", err)
		}
		s.outOn = true
	}
	if !s.capOn {
		t := Capture
		if err := s.ioctlCall(int(s.file.Fd()), StreamOn, unsafe.Pointer(&t)); err != nil {
			return fmt.Errorf("scaler STREAMON CAPTURE: %w", err)
		}
		s.capOn = true
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	if mode == MemoryDMABuf {
		inputErr = s.dequeueDMAInput(deadline)
	} else {
		inputErr = s.dequeue(Output, [][]byte{s.sourceY, s.sourceUV}, deadline)
	}
	if inputErr != nil {
		return inputErr
	}
	if mode == MemoryDMABuf {
		if err := s.dequeueCaptureDMABuf(int(s.targetDMABuf.Fd()), targetBytes, deadline); err != nil {
			return err
		}
		completed := s.targetDMABuf
		s.targetDMABuf = nil
		if err := completed.Close(); err != nil {
			return fmt.Errorf("release scaler DMA-BUF fd: %w", err)
		}
	} else {
		if err := s.dequeue(Capture, [][]byte{target}, deadline); err != nil {
			return err
		}
	}
	s.queuedTarget = nil
	if s.sourceLease != nil {
		err := s.sourceLease.Release()
		s.sourceLease = nil
		if err != nil {
			return err
		}
	}
	us := time.Since(started).Microseconds()
	s.frames++
	s.lastUS = us
	s.totalUS += us
	if us > s.maxUS {
		s.maxUS = us
	}
	return nil
}

func (s *RGBScaler) Stats() ScalerStats {
	if s == nil {
		return ScalerStats{}
	}
	mean := int64(0)
	if s.frames != 0 {
		mean = s.totalUS / int64(s.frames)
	}
	return ScalerStats{Device: s.device, Frames: s.frames, LastUS: s.lastUS, MeanUS: mean, MaxUS: s.maxUS, TargetBytes: s.targetBytes, CopiedBytes: s.copiedBytes}
}

func (s *RGBScaler) TargetStride() int { return s.targetStride }
func (s *RGBScaler) TargetBytes() int  { return s.targetBytes }

func (s *RGBScaler) Close() error {
	if s == nil || s.closed {
		return nil
	}
	if s.unsafeOwnership {
		return ErrScalerQuarantined
	}
	var err error
	for _, x := range []struct {
		t  uint32
		on *bool
	}{{Capture, &s.capOn}, {Output, &s.outOn}} {
		if *x.on {
			t := x.t
			if e := s.ioctlCall(int(s.file.Fd()), StreamOff, unsafe.Pointer(&t)); e != nil {
				return s.quarantine(fmt.Errorf("scaler STREAMOFF %d: %w", t, e))
			}
			*x.on = false
		}
		requested := s.outRequested
		if x.t == Capture {
			requested = s.capRequested
		}
		if requested {
			memory := s.captureMode()
			r := Request{Type: x.t, Memory: memory}
			if e := s.ioctlCall(int(s.file.Fd()), RequestBuffers, unsafe.Pointer(&r)); e != nil {
				return s.quarantine(fmt.Errorf("scaler release buffers %d: %w", x.t, e))
			}
			if x.t == Output {
				s.outRequested = false
			} else {
				s.capRequested = false
			}
		}
	}
	s.queuedTarget = nil
	if s.sourceLease != nil {
		err = errors.Join(err, s.sourceLease.Release())
		s.sourceLease = nil
	}
	if s.targetDMABuf != nil {
		err = errors.Join(err, s.targetDMABuf.Close())
		s.targetDMABuf = nil
	}
	for _, b := range [][]byte{s.sourceY, s.sourceUV} {
		if len(b) != 0 {
			err = errors.Join(err, syscall.Munmap(b))
		}
	}
	if err != nil {
		return err
	}
	s.sourceY, s.sourceUV = nil, nil
	s.closed = true
	return s.file.Close()
}

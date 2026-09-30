//go:build linux && (amd64 || arm64)

package media

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type Image struct {
	Width, Height, StrideY, StrideUV int
	Y, UV                            []byte
	PTS                              int64
	Lease                            *FrameLease
}
type mapping struct {
	planes      [][]byte
	dma         []*os.File
	borrowed    *FrameLease
	exported    *FrameLease
	exportLocal bool
	queued      bool
}

var ErrQuarantined = errors.New("MFC ownership not returned; no reopen before reboot")

type Decoder struct {
	closing         atomic.Bool
	captureRefs     atomic.Int32
	releaseWake     chan struct{}
	poolID          uint64
	unsafeOwnership bool
	file            *os.File
	output, capture []mapping
	outOn, capOn    bool
	format          Format
	crop            Crop
	memory          uint32
	closed          bool
	pendingPictures int
}

var quarantineMu sync.Mutex
var quarantined []*Decoder // A failed STREAMOFF must not release or recycle mapped memory.
var decoderPoolID atomic.Uint64

func OpenDecoder(path string, first []byte, pts uint64) (d *Decoder, err error) {
	if IsQuarantined() {
		return nil, ErrQuarantined
	}
	if e := VerifyABI(); e != nil {
		return nil, e
	}
	if len(first) < 4 || len(first) > 1<<20 {
		return nil, fmt.Errorf("invalid first AU")
	}
	config, picture, e := DecoderStartup(first)
	if e != nil {
		return nil, e
	}
	w, h, e := MonitorKeyDimensions(first)
	if e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	d = &Decoder{file: f, memory: MemoryDMABuf, releaseWake: make(chan struct{}, 1), poolID: decoderPoolID.Add(1)}
	owned := d
	defer func() {
		if err != nil {
			err = errors.Join(err, owned.Close())
		}
	}()
	fmtOut := Format{Type: Output}
	fmtOut.Set(w, h, H264, 1)
	fmtOut.SetPlaneSize(0, 1<<20)
	if err = ioctl(d.fd(), SFormat, unsafe.Pointer(&fmtOut)); err != nil {
		return nil, fmt.Errorf("MFC S_FMT OUTPUT: %w", err)
	}
	if fmtOut.PixelFormat() != H264 || fmtOut.Planes() != 1 {
		return nil, fmt.Errorf("unexpected coded format")
	}
	// Prefer linear, two-plane output. Explicitly reject tiled/protected vendor formats.
	capture := Format{Type: Capture}
	capture.Set(w, h, NV12M, 2)
	if e = ioctl(d.fd(), SFormat, unsafe.Pointer(&capture)); e != nil {
		capture = Format{Type: Capture}
		capture.Set(w, h, NV12, 1)
		if err = ioctl(d.fd(), SFormat, unsafe.Pointer(&capture)); err != nil {
			return nil, fmt.Errorf("MFC cannot select linear NV12: %w", err)
		}
	}
	if d.output, err = d.allocateDMA(Output, 3, []uint32{1 << 20}); err != nil {
		return nil, err
	}
	if err = d.Submit(config, pts); err != nil {
		return nil, err
	}
	t := Output
	if err = ioctl(d.fd(), StreamOn, unsafe.Pointer(&t)); err != nil {
		return nil, fmt.Errorf("STREAMON OUTPUT: %w", err)
	}
	d.outOn = true
	// Older MFC implementations can complete the header parse during G_FMT instead
	// of providing a modern SOURCE_CHANGE event. Bound retries; no guessed geometry.
	end := time.Now().Add(2 * time.Second)
	for {
		capture = Format{Type: Capture}
		err = ioctl(d.fd(), GFormat, unsafe.Pointer(&capture))
		if err == nil && capture.Width() > 0 && capture.Height() > 0 && capture.PlaneSize(0) > 0 {
			break
		}
		if time.Now().After(end) {
			return nil, fmt.Errorf("MFC header/format timeout: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if capture.PixelFormat() != NV12M && capture.PixelFormat() != NV12 {
		return nil, fmt.Errorf("unsupported raw format %q", FourCC(capture.PixelFormat()))
	}
	if capture.Width() < w || capture.Width() > w+64 || capture.Height() < h || capture.Height() > h+48 {
		return nil, fmt.Errorf("decoded geometry differs from selected mode: %dx%d", capture.Width(), capture.Height())
	}
	if (capture.PixelFormat() == NV12M && capture.Planes() != 2) || (capture.PixelFormat() == NV12 && capture.Planes() != 1) {
		return nil, fmt.Errorf("unexpected raw plane count")
	}
	d.format = capture
	d.crop = Crop{Type: Capture}
	err = ioctl(d.fd(), GetCrop, unsafe.Pointer(&d.crop))
	if err != nil {
		return nil, fmt.Errorf("MFC crop is required to distinguish visible from padded frame: %w", err)
	}
	if d.crop.Width != w || d.crop.Height != h || d.crop.Left < 0 || d.crop.Top < 0 || d.crop.Left%2 != 0 || d.crop.Top%2 != 0 || uint64(d.crop.Left)+uint64(w) > uint64(capture.Width()) || uint64(d.crop.Top)+uint64(h) > uint64(capture.Height()) {
		return nil, fmt.Errorf("MFC visible crop not supported: %+v", d.crop)
	}
	minimum := int32(0)
	c := Control{ID: 0x00980927}
	if ioctl(d.fd(), GetControl, unsafe.Pointer(&c)) == nil {
		minimum = c.Value
	}
	if minimum < 0 || minimum > 24 {
		return nil, fmt.Errorf("unreasonable DPB requirement %d", minimum)
	}
	// Reference DPB plus current scanout, presenter, two pending frames, one
	// newly dequeued frame and two DECON retire-fence records. Display leases
	// must not starve MFC's required DPB while scanout is pipelined.
	n := uint32(max(8, minimum+7))
	sizes := make([]uint32, int(capture.Planes()))
	for i := range sizes {
		sizes[i] = capture.PlaneSize(i)
	}
	if d.capture, err = d.allocateDMA(Capture, n, sizes); err != nil {
		return nil, err
	}
	if minimum > 0 && len(d.capture) < int(minimum) {
		return nil, fmt.Errorf("insufficient capture buffers")
	}
	for i := range d.capture {
		if err = d.queueCapture(i); err != nil {
			return nil, err
		}
	}
	t = Capture
	if err = ioctl(d.fd(), StreamOn, unsafe.Pointer(&t)); err != nil {
		return nil, fmt.Errorf("STREAMON CAPTURE: %w", err)
	}
	d.capOn = true
	// Samsung completes the config OUTPUT only after CAPTURE is streaming.
	// The config buffer contains no VCL, so starting CAPTURE cannot decode it.
	if err = d.reclaimStartupConfig(time.Now().Add(time.Second)); err != nil {
		return nil, err
	}
	d.pendingPictures = 0 // The startup SPS/PPS submission has no decoded picture.
	if err = d.Submit(picture, pts); err != nil {
		return nil, fmt.Errorf("submit initial IDR after capture setup: %w", err)
	}
	return d, nil
}
func (d *Decoder) fd() int { return int(d.file.Fd()) }
func (d *Decoder) memoryType() uint32 {
	if d.memory == MemoryDMABuf {
		return MemoryDMABuf
	}
	if d.memory == MemoryUserPtr {
		return MemoryUserPtr
	}
	return MemoryMMap
}

// e418 verify_userptr rejects anonymous VMAs on every QBUF. vb2 then remaps
// them, while static MFC DPBs keep their first IOVA. Imported ION buffers keep
// that IOVA through DQBUF; only detach after STREAMOFF releases it.
func (d *Decoder) allocateDMA(t, n uint32, sizes []uint32) (out []mapping, err error) {
	if len(sizes) < 1 || len(sizes) > 2 {
		return nil, fmt.Errorf("invalid MFC DMA plane count")
	}
	q := Request{Count: n, Type: t, Memory: MemoryDMABuf}
	if err = ioctl(d.fd(), RequestBuffers, unsafe.Pointer(&q)); err != nil {
		return nil, fmt.Errorf("REQBUFS DMABUF %d: %w", t, err)
	}
	if q.Count < 1 || q.Count > 32 {
		return nil, fmt.Errorf("invalid MFC DMA buffer count %d", q.Count)
	}
	out = make([]mapping, q.Count)
	if t == Output {
		d.output = out
	} else {
		d.capture = out
	}
	var total uint64
	for i := range out {
		for _, size := range sizes {
			total += uint64(size)
			if size == 0 || size > 16<<20 || total > 96<<20 {
				return out, fmt.Errorf("unsafe MFC DMA geometry")
			}
			fd, err := allocateCodecDMABuf(int(size))
			if err != nil {
				return out, err
			}
			out[i].dma = append(out[i].dma, fd)
			data, err := syscall.Mmap(int(fd.Fd()), 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
			if err != nil {
				return out, fmt.Errorf("map MFC ION plane: %w", err)
			}
			out[i].planes = append(out[i].planes, data)
		}
	}
	return out, nil
}

// Samsung's vb2-ion MMAP path panics in remap_pfn_range on e418. USERPTR is
// explicitly advertised by both MFC queues and avoids mapping driver-owned ION.
func (d *Decoder) allocateUser(t, n uint32, sizes []uint32) (out []mapping, err error) {
	if len(sizes) < 1 || len(sizes) > 8 {
		return nil, fmt.Errorf("invalid USERPTR plane count")
	}
	q := Request{Count: n, Type: t, Memory: MemoryUserPtr}
	if err = ioctl(d.fd(), RequestBuffers, unsafe.Pointer(&q)); err != nil {
		return nil, fmt.Errorf("REQBUFS USERPTR %d: %w", t, err)
	}
	if q.Count < 1 || q.Count > 32 {
		return nil, fmt.Errorf("invalid USERPTR buffer count %d", q.Count)
	}
	out = make([]mapping, q.Count)
	if t == Output {
		d.output = out
	} else {
		d.capture = out
	}
	var total uint64
	for i := range out {
		for _, size := range sizes {
			total += uint64(size)
			if size == 0 || size > 16<<20 || total > 64<<20 {
				return out, fmt.Errorf("unsafe USERPTR geometry")
			}
			data, mapErr := syscall.Mmap(-1, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
			if mapErr != nil {
				return out, fmt.Errorf("anonymous USERPTR allocation: %w", mapErr)
			}
			out[i].planes = append(out[i].planes, data)
		}
	}
	return out, nil
}

func userPlaneAddress(p []byte) uint64 {
	if len(p) == 0 {
		return 0
	}
	return uint64(uintptr(unsafe.Pointer(&p[0])))
}
func (d *Decoder) preparePlanes(m *mapping) []Plane {
	p := make([]Plane, len(m.planes))
	for i, data := range m.planes {
		p[i].Length = uint32(len(data))
		if d.memoryType() == MemoryUserPtr {
			p[i].Memory = userPlaneAddress(data)
		} else if d.memoryType() == MemoryDMABuf {
			p[i].Memory = uint64(m.dma[i].Fd())
		}
	}
	return p
}
func (d *Decoder) confirmUserPlanes(m *mapping, p []Plane) error {
	if m.borrowed != nil {
		if len(p) != len(m.borrowed.Planes) {
			return fmt.Errorf("borrowed MFC plane count changed")
		}
		for i, plane := range m.borrowed.Planes {
			if p[i].Memory != uint64(plane.FD) || p[i].Length != plane.Length {
				return fmt.Errorf("borrowed MFC plane identity changed")
			}
		}
		return nil
	}
	if d.memoryType() == MemoryDMABuf {
		if len(p) != len(m.planes) || len(p) != len(m.dma) {
			return fmt.Errorf("MFC DMABUF plane count changed")
		}
		for i := range p {
			if p[i].Memory != uint64(m.dma[i].Fd()) || p[i].Length != uint32(len(m.planes[i])) {
				return fmt.Errorf("MFC returned a different DMA plane")
			}
		}
		return nil
	}
	if d.memoryType() != MemoryUserPtr {
		return nil
	}
	if len(p) != len(m.planes) {
		return fmt.Errorf("USERPTR plane count changed")
	}
	for i := range p {
		if p[i].Memory != userPlaneAddress(m.planes[i]) {
			return fmt.Errorf("MFC returned a different USERPTR")
		}
	}
	return nil
}
func (d *Decoder) reclaimStartupConfig(deadline time.Time) error {
	for {
		p := make([]Plane, 1)
		b := Buffer{Type: Output, Memory: d.memoryType()}
		err := bufferIoctl(d.fd(), DequeueBuffer, &b, p)
		if err == syscall.EAGAIN && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
			continue
		}
		if err != nil {
			return fmt.Errorf("reclaim MFC startup config: %w", err)
		}
		if int(b.Index) >= len(d.output) || !d.output[b.Index].queued {
			return fmt.Errorf("invalid startup config completion")
		}
		if err = d.confirmUserPlanes(&d.output[b.Index], p); err != nil {
			return err
		}
		d.output[b.Index].queued = false
		if b.Flags&0x40 != 0 {
			return fmt.Errorf("MFC flagged damaged startup configuration")
		}
		return nil
	}
}
func (d *Decoder) allocate(t, n uint32, planes int) (out []mapping, err error) {
	q := Request{Count: n, Type: t, Memory: MemoryMMap}
	if err = ioctl(d.fd(), RequestBuffers, unsafe.Pointer(&q)); err != nil {
		return nil, fmt.Errorf("REQBUFS %d: %w", t, err)
	}
	if q.Count < 1 || q.Count > 32 {
		return nil, fmt.Errorf("invalid buffer count %d", q.Count)
	}
	// Attach ownership before any mapping so initialization errors can be unwound.
	out = make([]mapping, q.Count)
	if t == Output {
		d.output = out
	} else {
		d.capture = out
	}
	var total uint64
	for i := range out {
		p := make([]Plane, planes)
		b := Buffer{Index: uint32(i), Type: t, Memory: MemoryMMap}
		if err = bufferIoctl(d.fd(), QueryBuffer, &b, p); err != nil {
			return out, fmt.Errorf("QUERYBUF: %w", err)
		}
		if int(b.Length) != planes {
			return out, fmt.Errorf("plane count changed")
		}
		for _, v := range p {
			total += uint64(v.Length)
			if v.Length == 0 || v.Length > 16<<20 || total > 64<<20 || v.Memory > 0xffffffff || v.Memory%4096 != 0 {
				return out, fmt.Errorf("unsafe mapping geometry")
			}
			var data []byte
			data, err = syscall.Mmap(d.fd(), int64(v.Memory), int(v.Length), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
			if err != nil {
				return out, fmt.Errorf("MFC MMAP: %w", err)
			}
			out[i].planes = append(out[i].planes, data)
		}
	}
	return out, nil
}
func (d *Decoder) queueCapture(i int) error {
	m := &d.capture[i]
	if m.exported != nil && m.exported.Live() {
		return nil
	}
	p := d.preparePlanes(m)
	b := Buffer{Index: uint32(i), Type: Capture, Memory: d.memoryType()}
	if e := bufferIoctl(d.fd(), QueueBuffer, &b, p); e != nil {
		return e
	}
	m.queued = true
	m.exported = nil
	return nil
}

func (d *Decoder) returnCapture(i int) error {
	m := &d.capture[i]
	if m.exportLocal {
		m.exportLocal = false
		if err := m.exported.Release(); err != nil {
			return err
		}
	}
	return d.queueCapture(i)
}
func (d *Decoder) flushReleased() error {
	for i := range d.capture {
		m := &d.capture[i]
		if !m.queued && !m.exportLocal && m.exported != nil && !m.exported.Live() {
			if err := d.queueCapture(i); err != nil {
				return err
			}
		}
	}
	return nil
}
func (d *Decoder) Submit(data []byte, pts uint64) error {
	if d.closed {
		return os.ErrClosed
	}
	if pts > 1<<63-1 || len(data) < 4 || len(data) > 1<<20 {
		return fmt.Errorf("invalid access unit")
	}
	for i := range d.output {
		m := &d.output[i]
		if m.queued {
			continue
		}
		if len(m.planes) != 1 || len(data) > len(m.planes[0]) {
			return fmt.Errorf("access unit exceeds MFC buffer")
		}
		copy(m.planes[0], data)
		p := d.preparePlanes(m)
		p[0].Used = uint32(len(data))
		b := Buffer{Index: uint32(i), Type: Output, Memory: d.memoryType(), Seconds: int64(pts / 1000000), Microseconds: int64(pts % 1000000)}
		if e := bufferIoctl(d.fd(), QueueBuffer, &b, p); e != nil {
			return e
		}
		m.queued = true
		d.pendingPictures++
		return nil
	}
	return syscall.EAGAIN
}

// Called only by the decoder owner. Retired display leases wake it separately
// so a static desktop does not need constant DQBUF polling to recycle DPBs.
func (d *Decoder) DecodeWork() (bool, <-chan struct{}) {
	if d.pendingPictures != 0 {
		return true, d.releaseWake
	}
	for i := range d.output {
		if d.output[i].queued {
			return true, d.releaseWake
		}
	}
	return false, d.releaseWake
}
func (d *Decoder) Drain(present func(Image) error) (int, error) {
	return d.drain(present, false)
}
func (d *Decoder) DrainLatest(present func(Image) error) (int, error) {
	return d.drain(present, true)
}
func (d *Decoder) drain(present func(Image) error, latest bool) (count int, err error) {
	defer func() { d.pendingPictures = max(0, d.pendingPictures-count) }()
	if d.closed {
		return 0, os.ErrClosed
	}
	if present == nil {
		return 0, fmt.Errorf("nil decoder consumer")
	}
	if err := d.flushReleased(); err != nil {
		return 0, err
	}
	// Bounded even if a broken driver produces duplicate or unexpected completions.
	for j := 0; j < len(d.output); j++ {
		p := make([]Plane, 1)
		b := Buffer{Type: Output, Memory: d.memoryType()}
		e := bufferIoctl(d.fd(), DequeueBuffer, &b, p)
		if e == syscall.EAGAIN {
			break
		}
		if e != nil {
			return 0, fmt.Errorf("DQ OUTPUT: %w", e)
		}
		if int(b.Index) >= len(d.output) || !d.output[b.Index].queued {
			return 0, fmt.Errorf("invalid output completion")
		}
		if e = d.confirmUserPlanes(&d.output[b.Index], p); e != nil {
			return 0, e
		}
		d.output[b.Index].queued = false
		if b.Flags&0x40 != 0 {
			return 0, fmt.Errorf("MFC flagged a damaged compressed access unit")
		}
	}
	return drainDecoded(len(d.capture), latest, func() (decodedBuffer, error) {
		p := make([]Plane, int(d.format.Planes()))
		b := Buffer{Type: Capture, Memory: d.memoryType()}
		e := bufferIoctl(d.fd(), DequeueBuffer, &b, p)
		if e != nil {
			return decodedBuffer{}, e
		}
		if int(b.Index) >= len(d.capture) || !d.capture[b.Index].queued {
			return decodedBuffer{}, fmt.Errorf("invalid capture completion")
		}
		m := &d.capture[b.Index]
		if e = d.confirmUserPlanes(m, p); e != nil {
			return decodedBuffer{}, e
		}
		m.queued = false
		if b.Flags&0x40 != 0 {
			return decodedBuffer{}, fmt.Errorf("MFC flagged a damaged decoded frame")
		}
		if p[0].Used == 0 {
			return decodedBuffer{Index: int(b.Index), Empty: true}, nil
		}
		if b.Seconds < 0 || b.Seconds > (1<<63-1-999999)/1000000 || b.Microseconds < 0 || b.Microseconds >= 1000000 {
			return decodedBuffer{}, fmt.Errorf("invalid MFC timestamp")
		}
		im, e := d.image(*m, p, b.Seconds*1000000+b.Microseconds)
		if e == nil && d.memoryType() == MemoryDMABuf {
			planes := make([]DMAPlane, len(m.dma))
			for j, fd := range m.dma {
				stride := im.StrideY
				offset := int(d.crop.Top)*stride + int(d.crop.Left)
				if j == 1 {
					stride = im.StrideUV
					offset = int(d.crop.Top)/2*stride + int(d.crop.Left)
				}
				planes[j] = DMAPlane{FD: int(fd.Fd()), Length: uint32(len(m.planes[j])), Offset: p[j].Offset + uint32(offset), Stride: uint32(stride)}
			}
			if len(m.dma) == 1 {
				uv := planes[0]
				uv.Offset = p[0].Offset + d.format.Stride(0)*d.format.Height() + uint32(int(d.crop.Top)/2*im.StrideUV+int(d.crop.Left))
				uv.Stride = uint32(im.StrideUV)
				planes = append(planes, uv)
			}
			im.Lease, e = NewFrameLease(d.poolID<<16|uint64(b.Index+1), d.poolID, im.Width, im.Height, im.PTS, BT709Limited, planes, func() error {
				d.captureRefs.Add(-1)
				select {
				case d.releaseWake <- struct{}{}:
				default:
				}
				return nil
			})
			if e == nil {
				im.Lease.StorageWidth = int(d.format.Stride(0))
				im.Lease.StorageHeight = int(d.format.Height())
				d.captureRefs.Add(1)
				m.exported = im.Lease
				m.exportLocal = true
			}
		}
		return decodedBuffer{Index: int(b.Index), Image: im}, e
	}, d.returnCapture, present)
}
func (d *Decoder) image(m mapping, p []Plane, pts int64) (Image, error) {
	var im Image
	stride := int(d.format.Stride(0))
	h := int(d.format.Height())
	uvstride := stride
	if stride < int(d.format.Width()) || stride > 8192 {
		return im, fmt.Errorf("invalid Y stride")
	}
	// Samsung 3.18 checks the coded H264 source format here instead of the
	// decoded NV12M destination format. It therefore reports the combined Y+UV
	// payload in plane 0 and leaves plane 1 at zero. Accept only that exact,
	// allocation-bounded legacy shape and normalize it before slicing.
	if d.format.PixelFormat() == NV12M && len(p) == 2 && len(m.planes) == 2 &&
		p[0].Offset == 0 && p[1].Offset == 0 && p[1].Used == 0 &&
		uint64(p[0].Used) == uint64(len(m.planes[0]))+uint64(len(m.planes[1])) {
		p[0].Used = uint32(len(m.planes[0]))
		p[1].Used = uint32(len(m.planes[1]))
	}
	slices := make([][]byte, len(p))
	for i, x := range p {
		if x.Used > uint32(len(m.planes[i])) || x.Offset > x.Used {
			return im, fmt.Errorf("invalid plane %d bytesused=%d data_offset=%d allocation=%d", i, x.Used, x.Offset, len(m.planes[i]))
		}
		slices[i] = m.planes[i][x.Offset:x.Used]
	}
	var y, uv []byte
	if d.format.PixelFormat() == NV12 {
		n := stride * h
		if n > len(slices[0]) {
			return im, fmt.Errorf("short NV12 Y")
		}
		y = slices[0][:n]
		uv = slices[0][n:]
	} else {
		y = slices[0]
		uv = slices[1]
		uvstride = int(d.format.Stride(1))
		if uvstride == 0 {
			uvstride = stride
		}
	}
	left, top := int(d.crop.Left), int(d.crop.Top)
	oy := top*stride + left
	ou := top/2*uvstride + left
	w, visibleH := int(d.crop.Width), int(d.crop.Height)
	if w < 1 || visibleH < 2 || uvstride < w || uvstride > 8192 || oy+(visibleH-1)*stride+w > len(y) || ou+(visibleH/2-1)*uvstride+w > len(uv) {
		return im, fmt.Errorf("short/out-of-range decoded image")
	}
	return Image{Width: w, Height: visibleH, StrideY: stride, StrideUV: uvstride, Y: y[oy:], UV: uv[ou:], PTS: pts}, nil
}
func (d *Decoder) Close() error {
	if d == nil || d.closed {
		return nil
	}
	if d.unsafeOwnership {
		return ErrQuarantined
	}
	d.closing.Store(true)
	for i := range d.capture {
		m := &d.capture[i]
		if m.exportLocal {
			m.exportLocal = false
			_ = m.exported.Release()
		}
	}
	// STREAMOFF returns ownership. On failure keep mappings alive, do not reuse the device.
	for _, x := range []struct {
		t  uint32
		on *bool
	}{{Capture, &d.capOn}, {Output, &d.outOn}} {
		queued := false
		q := d.output
		if x.t == Capture {
			q = d.capture
		}
		for _, m := range q {
			queued = queued || m.queued
		}
		if *x.on || queued {
			t := x.t
			if e := ioctl(d.fd(), StreamOff, unsafe.Pointer(&t)); e != nil {
				d.unsafeOwnership = true
				quarantineMu.Lock()
				quarantined = append(quarantined, d)
				quarantineMu.Unlock()
				return errors.Join(ErrQuarantined, fmt.Errorf("STREAMOFF failed: %w", e))
			}
			*x.on = false
		}
	}
	if d.captureRefs.Load() != 0 {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		for d.captureRefs.Load() != 0 {
			select {
			case <-d.releaseWake:
			case <-timer.C:
				d.unsafeOwnership = true
				quarantineMu.Lock()
				quarantined = append(quarantined, d)
				quarantineMu.Unlock()
				return errors.Join(ErrQuarantined, fmt.Errorf("decoded DMA leases still active"))
			}
		}
	}
	var err error
	// Close detaches the imported pools only after both STREAMOFF calls above.
	// DMA fds and CPU mappings remain owned until the driver has detached them.
	err = d.file.Close()
	for _, queue := range [][]mapping{d.output, d.capture} {
		for _, m := range queue {
			if m.borrowed != nil {
				err = errors.Join(err, m.borrowed.Release())
			}
			for _, p := range m.planes {
				err = errors.Join(err, syscall.Munmap(p))
			}
			for _, fd := range m.dma {
				err = errors.Join(err, fd.Close())
			}
		}
	}
	d.closed = true
	return err
}

func IsQuarantined() bool {
	quarantineMu.Lock()
	defer quarantineMu.Unlock()
	return len(quarantined) != 0
}

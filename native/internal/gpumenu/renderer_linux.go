//go:build linux && (amd64 || arm64)

// Package gpumenu sends drawing commands to Mali. The output stays in a shared
// ION DMA-BUF; no menu pixels are read, rasterized or copied by Go.
package gpumenu

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"perimode/native/internal/childproc"
	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
)

const (
	Width             = 1440
	Height            = 2560
	FrameBytes        = Width * Height * 4
	BlurBytes         = 1 << 20
	PreviewBytes      = 4 << 20
	PreviewInputBytes = 2560 * 1440 * 3 / 2
	MaxCommands       = 4096
	Rectangle         = 1
	Glyph             = 2
	Line              = 3
)

// Kind, clip bounds, RGB, opacity, then shape-specific geometry/atlas offsets.
type Command [16]int32

type Renderer struct {
	mu              sync.Mutex
	cmd             *exec.Cmd
	dma             *os.File
	blurIn, blurOut *os.File
	previewOut      *os.File
	previewOther    *os.File
	transferFD      *os.File
	dmaCache        [8]uint64
	nextDMA         int
	heldFrames      []*media.FrameLease
	in, out         *os.File
	done            chan error
	seq             uint32
	closed          bool
	failed          error
	LastUS          uint32
	stderr          boundedOutput
}

type boundedOutput struct {
	mu   sync.Mutex
	text []byte
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.text) < 4096 {
		b.text = append(b.text, p[:min(len(p), 4096-len(b.text))]...)
	}
	return len(p), nil
}
func (b *boundedOutput) String() string { b.mu.Lock(); defer b.mu.Unlock(); return string(b.text) }

func newOutput(bytes int) (*os.File, error) {
	if bytes <= 0 || bytes > FrameBytes || bytes%4096 != 0 {
		return nil, fmt.Errorf("invalid menu DMA size")
	}
	ions, err := os.OpenFile("/dev/ion", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer ions.Close()
	allocation := struct {
		Length, Alignment uint64
		Heap, Flags       uint32
		Handle            int32
		Padding           uint32
	}{Length: uint64(bytes), Alignment: 4096, Heap: 1, Handle: -1}
	if err = linuxio.Ioctl(int(ions.Fd()), 0xc0204900, unsafe.Pointer(&allocation)); err != nil {
		return nil, err
	}
	if allocation.Handle < 0 {
		return nil, fmt.Errorf("ION returned invalid menu handle")
	}
	share := struct{ Handle, FD int32 }{allocation.Handle, -1}
	err = linuxio.Ioctl(int(ions.Fd()), 0xc0084904, unsafe.Pointer(&share))
	freeErr := linuxio.Ioctl(int(ions.Fd()), 0xc0044901, unsafe.Pointer(&allocation.Handle))
	if err != nil || freeErr != nil || share.FD < 0 {
		if share.FD >= 0 {
			_ = syscall.Close(int(share.FD))
		}
		return nil, fmt.Errorf("ION menu share/free: %v / %v", err, freeErr)
	}
	return os.NewFile(uintptr(share.FD), "s7-gpu-menu"), nil
}

func Start(ctx context.Context, linker, worker string, env []string, atlas []byte) (*Renderer, error) {
	return start(ctx, linker, worker, env, atlas, false)
}

func StartPreview(ctx context.Context, linker, worker string, env []string) (*Renderer, error) {
	return start(ctx, linker, worker, env, nil, true)
}

func start(ctx context.Context, linker, worker string, env []string, atlas []byte, previewOnly bool) (*Renderer, error) {
	if ctx == nil || !previewOnly && len(atlas) == 0 || len(atlas) > 8<<20 {
		return nil, fmt.Errorf("invalid GPU menu atlas")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	outputBytes := FrameBytes
	previewBytes := 4096
	if previewOnly {
		outputBytes = 4096
		previewBytes = PreviewBytes
	}
	dma, err := newOutput(outputBytes)
	if err != nil {
		return nil, err
	}
	r := &Renderer{dma: dma, done: make(chan error, 1)}
	secondMenuBytes := 4096
	if !previewOnly {
		secondMenuBytes = FrameBytes
	}
	r.blurIn, err = newOutput(secondMenuBytes)
	if err != nil {
		_ = dma.Close()
		return nil, err
	}
	r.blurOut, err = newOutput(4096)
	if err != nil {
		_ = dma.Close()
		_ = r.blurIn.Close()
		return nil, err
	}
	r.previewOut, err = newOutput(previewBytes)
	if err != nil {
		_ = dma.Close()
		_ = r.blurIn.Close()
		_ = r.blurOut.Close()
		return nil, err
	}
	r.previewOther, err = newOutput(previewBytes)
	if err != nil {
		_ = dma.Close()
		_ = r.blurIn.Close()
		_ = r.blurOut.Close()
		_ = r.previewOut.Close()
		return nil, err
	}
	sockets, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		_ = dma.Close()
		_ = r.blurIn.Close()
		_ = r.blurOut.Close()
		_ = r.previewOut.Close()
		_ = r.previewOther.Close()
		return nil, err
	}
	r.transferFD = os.NewFile(uintptr(sockets[0]), "gpu-dma-transfer")
	childFD := os.NewFile(uintptr(sockets[1]), "gpu-dma-worker")
	defer childFD.Close()
	closeDMA := func() {
		_ = dma.Close()
		_ = r.blurIn.Close()
		_ = r.blurOut.Close()
		_ = r.previewOut.Close()
		_ = r.previewOther.Close()
		_ = r.transferFD.Close()
	}
	r.cmd = exec.Command(linker, worker)
	r.cmd.Env = env
	r.cmd.ExtraFiles = []*os.File{dma, r.blurIn, r.blurOut, r.previewOut, r.previewOther, childFD}
	r.cmd.Stderr = &r.stderr
	in, err := r.cmd.StdinPipe()
	if err != nil {
		closeDMA()
		return nil, err
	}
	out, err := r.cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		closeDMA()
		return nil, err
	}
	r.in, r.out = in.(*os.File), out.(*os.File)
	if err = childproc.Start(r.cmd); err != nil {
		_ = r.in.Close()
		_ = r.out.Close()
		closeDMA()
		return nil, err
	}
	go func() { r.done <- childproc.Wait(r.cmd) }()
	deadline := time.Now().Add(10 * time.Second)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	_ = r.in.SetWriteDeadline(deadline)
	_ = r.out.SetReadDeadline(deadline)
	header := [4]uint32{0x31414d47, uint32(len(atlas)), Width, Height}
	if previewOnly {
		header[0] = 0x32414d47
	}
	if err = binary.Write(r.in, binary.LittleEndian, header); err == nil {
		err = writeAll(r.in, atlas)
	}
	if err == nil {
		err = r.ack(0)
	}
	if err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("Mali menu startup: %w; %s", err, r.stderr.String())
	}
	return r, nil
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, e := w.Write(data)
		if e != nil {
			return e
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
func (r *Renderer) ack(seq uint32) error {
	var reply [4]uint32
	if err := binary.Read(r.out, binary.LittleEndian, &reply); err != nil {
		return err
	}
	if reply[0] != 0x31524d47 || reply[1] != seq {
		return fmt.Errorf("GPU menu reply mismatch")
	}
	if reply[2] != 0 {
		return fmt.Errorf("GPU menu OpenCL error %d", int32(reply[2]))
	}
	r.LastUS = reply[3]
	return nil
}

func (r *Renderer) Render(rotation, width, height int, commands []Command) (int, error) {
	return r.RenderSlot(rotation, width, height, commands, 0)
}
func (r *Renderer) MenuFD(slot int) int {
	if slot == 1 {
		return int(r.blurIn.Fd())
	}
	return int(r.dma.Fd())
}
func (r *Renderer) RenderSlot(rotation, width, height int, commands []Command, slot int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return -1, os.ErrClosed
	}
	if r.failed != nil {
		return -1, r.failed
	}
	if slot < 0 || slot > 1 || len(commands) == 0 || len(commands) > MaxCommands || width*height != Width*Height ||
		(rotation != 0 && rotation != 90 && rotation != 180 && rotation != 270) {
		return -1, fmt.Errorf("invalid GPU menu scene")
	}
	r.seq++
	if r.seq == 0 {
		r.seq++
	}
	deadline := time.Now().Add(750 * time.Millisecond)
	_ = r.in.SetWriteDeadline(deadline)
	_ = r.out.SetReadDeadline(deadline)
	header := [8]uint32{0x31444d47, r.seq, uint32(rotation), uint32(len(commands)), uint32(width), uint32(height), uint32(slot)}
	err := binary.Write(r.in, binary.LittleEndian, header)
	if err == nil {
		err = writeAll(r.in, unsafe.Slice((*byte)(unsafe.Pointer(&commands[0])), len(commands)*64))
		runtime.KeepAlive(commands)
	}
	if err == nil {
		err = r.ack(r.seq)
	}
	if err != nil {
		r.failed = fmt.Errorf("Mali menu raster: %w; %s", err, r.stderr.String())
		return -1, r.failed
	}
	return r.MenuFD(slot), nil
}

func (r *Renderer) BlurInputFD() int  { return int(r.blurIn.Fd()) }
func (r *Renderer) BlurOutputFD() int { return int(r.blurOut.Fd()) }

func (r *Renderer) PreviewFD(slot int) int {
	if slot == 1 {
		return int(r.previewOther.Fd())
	}
	return int(r.previewOut.Fd())
}

func (r *Renderer) Blur(width, height int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return os.ErrClosed
	}
	if r.failed != nil {
		return r.failed
	}
	if width <= 0 || height <= 0 || width > 1024 || height > 1024 || width*height*4 > BlurBytes {
		return fmt.Errorf("invalid Mali blur extent")
	}
	r.seq++
	if r.seq == 0 {
		r.seq++
	}
	deadline := time.Now().Add(250 * time.Millisecond)
	_ = r.in.SetWriteDeadline(deadline)
	_ = r.out.SetReadDeadline(deadline)
	header := [8]uint32{0x31424d47, r.seq, 0, 0, uint32(width), uint32(height)}
	err := binary.Write(r.in, binary.LittleEndian, header)
	if err == nil {
		err = r.ack(r.seq)
	}
	if err != nil {
		r.failed = fmt.Errorf("Mali blur: %w; %s", err, r.stderr.String())
		return r.failed
	}
	return nil
}

func (r *Renderer) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	_ = r.in.Close()
	select {
	case err := <-r.done:
		return errors.Join(err, r.closeCompleted())
	case <-time.After(time.Second):
		_ = r.cmd.Process.Kill()
	}
	select {
	case err := <-r.done:
		return errors.Join(err, r.closeCompleted())
	case <-time.After(time.Second):
		// A kernel call may still own the DMA-BUF. Keep its fd reachable.
		retained.Lock()
		retained.items = append(retained.items, r)
		retained.Unlock()
		return fmt.Errorf("Mali menu shutdown incomplete; DMA-BUF retained")
	}
}

func (r *Renderer) closeCompleted() error {
	// exec.Cmd.Wait closes StdoutPipe after the child exits. That is not a
	// failed GPU teardown and must not poison an otherwise released camera.
	pipeErr := r.out.Close()
	if errors.Is(pipeErr, os.ErrClosed) {
		pipeErr = nil
	}
	err := errors.Join(pipeErr, r.dma.Close(), r.blurIn.Close(), r.blurOut.Close(), r.previewOut.Close(), r.previewOther.Close(), r.transferFD.Close())
	for _, frame := range r.heldFrames {
		err = errors.Join(err, frame.Release())
	}
	r.heldFrames = nil
	return err
}

var retained struct {
	sync.Mutex
	items []*Renderer
}

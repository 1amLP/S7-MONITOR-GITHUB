//go:build linux && (amd64 || arm64)

package fimg2d

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"perimode/native/internal/linuxio"
)

var ErrComposerPoisoned = errors.New("G2D ownership uncertain; retain mappings until reboot")
var prefaultChecksum atomic.Uint32

type HardwareAcceptance struct {
	SourceCopyARGB32     bool
	PremultipliedSrcOver bool
}

type SelfTestFailure struct {
	Result SelfTestResult
	Err    error
}

func (e *SelfTestFailure) Error() string { return e.Err.Error() }
func (e *SelfTestFailure) Unwrap() error { return e.Err }

type FrameResult struct {
	DownscaleUS int64
	CompositeUS int64
	Composed    bool
}

type HardwareBlur interface {
	BlurInputFD() int
	BlurOutputFD() int
	Blur(width, height int) error
}

type Composer struct {
	mu           sync.Mutex
	fb           *os.File
	tasks        taskContexts
	mapping      []byte
	ownedMapping bool
	scratchMem   []byte
	overlay      *staticOverlay
	rect         Rect
	scratch      scratchLayout
	closed       bool
	poisoned     bool
	frames       uint64
	proof        SelfTestResult
	ownedTest    *selfTestBuffers
	blur         HardwareBlur
}

func completeTask(cmd *task, expected int) error {
	if cmd.Flags&processError != 0 || cmd.Target.Plane[0].Payload != uint32(expected) {
		return fmt.Errorf("G2D failed flags=0x%x payload=%d expected=%d",
			cmd.Flags, cmd.Target.Plane[0].Payload, expected)
	}
	return nil
}

type processTask func(*task, *[2]image) error
type frameProcessTask func(*task, *[g2dMaxSources]image) error

// runTwoPass prepares commands only. No CPU pixel read or write occurs here.
// Process is blocking in the pinned m2m1shot2 driver; its completion is checked.
func runTwoPass(ctx context.Context, mapped, scratchMem []byte, overlay *staticOverlay, r Rect, process frameProcessTask) (result FrameResult, poison bool, err error) {
	return runFrame(ctx, mapped, scratchMem, overlay, r, process, nil)
}

func runFrame(ctx context.Context, mapped, scratchMem []byte, overlay *staticOverlay, r Rect, process frameProcessTask, blur HardwareBlur) (result FrameResult, poison bool, err error) {
	if ctx == nil || process == nil || overlay == nil {
		return result, false, fmt.Errorf("G2D needs context, overlay and processor")
	}
	if err = ctx.Err(); err != nil {
		return result, false, err
	}
	layout, err := layoutFor(r)
	if err != nil {
		return result, false, err
	}
	var downSources, blendSources *[g2dMaxSources]image
	var down, blend *task
	if blur == nil {
		downSources, down, err = downscaleCommand(mapped, scratchMem, r, layout)
	} else {
		downSources, down, err = downscaleDMACommand(mapped, blur.BlurInputFD(), r, layout)
	}
	if err != nil {
		return result, false, err
	}
	if blur == nil {
		blendSources, blend, err = compositeCommand(mapped, scratchMem, overlay, r, layout)
	} else {
		blendSources, blend, err = compositeDMACommand(mapped, blur.BlurOutputFD(), overlay, r, layout)
	}
	if err != nil {
		return result, false, err
	}
	started := time.Now()
	if err = process(down, downSources); err != nil {
		return result, true, fmt.Errorf("G2D downscale: %w", err)
	}
	result.DownscaleUS = time.Since(started).Microseconds()
	if err = completeTask(down, layout.payload); err != nil {
		return result, true, fmt.Errorf("G2D downscale completion: %w", err)
	}
	if blur != nil {
		started = time.Now()
		if err = blur.Blur(layout.width, layout.height); err != nil {
			return result, true, err
		}
		result.DownscaleUS += time.Since(started).Microseconds()
	}
	if err = ctx.Err(); err != nil {
		return result, false, err
	}
	started = time.Now()
	if err = process(blend, blendSources); err != nil {
		return result, true, fmt.Errorf("G2D composite: %w", err)
	}
	result.CompositeUS = time.Since(started).Microseconds()
	if err = completeTask(blend, frameBytes); err != nil {
		return result, true, fmt.Errorf("G2D composite completion: %w", err)
	}
	result.Composed = true
	return result, false, nil
}

var retainedComposers struct {
	sync.Mutex
	items []*Composer
}

func (c *Composer) retainOnFault() {
	retainedComposers.Lock()
	retainedComposers.items = append(retainedComposers.items, c)
	retainedComposers.Unlock()
}

func (c *Composer) closeOwned() error {
	if err := c.tasks.Close(); err != nil {
		return err
	}
	var err error
	if c.ownedTest != nil {
		err = errors.Join(err, c.ownedTest.close())
	}
	if c.overlay != nil {
		err = errors.Join(err, c.overlay.close())
	}
	if len(c.scratchMem) != 0 {
		err = errors.Join(err, syscall.Munmap(c.scratchMem))
	}
	if c.ownedMapping && len(c.mapping) != 0 {
		err = errors.Join(err, syscall.Munmap(c.mapping))
	}
	if c.fb != nil {
		err = errors.Join(err, c.fb.Close())
	}
	return err
}

// OpenComposer owns a separate fb0 mapping and an immutable copy of the menu
// overlay. It proves source copy and premultiplied SRCOVER offscreen before
// allowing a visible frame. Acceptance uses the raw half, so the caller must
// fill its entire frame again before the first ComposeFrame. The caller owns
// the final fbdev commit.
func OpenComposer(ctx context.Context, r Rect, physicalPremultipliedARGB []uint32) (c *Composer, err error) {
	return openComposer(ctx, r, physicalPremultipliedARGB, -1, nil, nil)
}

func OpenDMAComposer(ctx context.Context, r Rect, fd int, blur HardwareBlur) (*Composer, error) {
	if fd < 0 || blur == nil || blur.BlurInputFD() < 0 || blur.BlurOutputFD() < 0 || blur.BlurInputFD() == blur.BlurOutputFD() {
		return nil, fmt.Errorf("invalid Mali DMA buffers")
	}
	return openComposer(ctx, r, nil, fd, blur, nil)
}

// The framebuffer owner keeps this original mapping alive. After WIN_CONFIG
// fb0 names the new scanout DMA-BUF, so opening/mapping fb0 again is not allowed.
func OpenBorrowedDMAComposer(ctx context.Context, r Rect, fd int, blur HardwareBlur, mapped []byte) (*Composer, error) {
	if len(mapped) != 2*frameBytes || fd < 0 || blur == nil || blur.BlurInputFD() < 0 || blur.BlurOutputFD() < 0 || blur.BlurInputFD() == blur.BlurOutputFD() {
		return nil, fmt.Errorf("invalid owned G2D workspace")
	}
	return openComposer(ctx, r, nil, fd, blur, mapped)
}

func openComposer(ctx context.Context, r Rect, physicalPremultipliedARGB []uint32, dmaFD int, blur HardwareBlur, borrowed []byte) (c *Composer, err error) {
	if ctx == nil {
		return nil, fmt.Errorf("G2D self-test requires a context")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 10*time.Second {
		return nil, fmt.Errorf("G2D self-test requires a deadline within ten seconds")
	}
	layout, err := layoutFor(r)
	if err != nil {
		return nil, err
	}
	if err = verifyABI(); err != nil {
		return nil, err
	}
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil || strings.TrimSpace(string(kernel)) != pinnedKernel {
		return nil, fmt.Errorf("G2D requires exact e418 kernel: %v", err)
	}
	var overlay *staticOverlay
	if dmaFD >= 0 {
		overlay, err = newDMAOverlay(dmaFD)
	} else {
		overlay, err = newStaticOverlay(r, physicalPremultipliedARGB)
	}
	if err != nil {
		return nil, err
	}
	owned := &Composer{overlay: overlay, rect: r, scratch: layout, blur: blur}
	c = owned
	defer func() {
		if err != nil {
			if owned.poisoned {
				owned.retainOnFault()
			} else {
				_ = owned.closeOwned()
			}
			c = nil
		}
	}()
	if borrowed != nil {
		c.mapping = borrowed
	} else {
		c.fb, err = os.OpenFile("/dev/fb0", os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		var v variable
		var f fixed
		if err = linuxio.Ioctl(int(c.fb.Fd()), 0x4600, unsafe.Pointer(&v)); err != nil {
			return nil, err
		}
		if err = linuxio.Ioctl(int(c.fb.Fd()), 0x4602, unsafe.Pointer(&f)); err != nil {
			return nil, err
		}
		if err = checkFBLayout(v, f); err != nil {
			return nil, err
		}
		c.mapping, err = syscall.Mmap(int(c.fb.Fd()), 0, int(f.MemoryLength), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			return nil, err
		}
		c.ownedMapping = true
	}
	// Prefault the visible PFNMAP once. ComposeFrame never reads or draws pixels on CPU.
	var touched byte
	for off := 0; off < frameBytes; off += 4096 {
		touched ^= c.mapping[off]
	}
	prefaultChecksum.Store(uint32(touched))
	if blur == nil {
		c.scratchMem, err = syscall.Mmap(-1, 0, c.scratch.mapped,
			syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
		if err != nil {
			return nil, err
		}
		clear(c.scratchMem)
	}
	c.ownedTest, err = newSelfTestBuffers()
	if err != nil {
		return nil, err
	}
	var uncertain bool
	c.proof, uncertain, err = runSelfTest(ctx, c.mapping, c.ownedTest, func(cmd *task, sources *[2]image) error {
		e := c.tasks.process(cmd, sources[:cmd.NumSources])
		runtime.KeepAlive(sources)
		runtime.KeepAlive(cmd)
		runtime.KeepAlive(c.mapping)
		runtime.KeepAlive(c.ownedTest)
		return e
	})
	if uncertain {
		c.poisoned = true
		return nil, &SelfTestFailure{Result: c.proof,
			Err: fmt.Errorf("G2D self-test ownership uncertain: %w: %v", ErrComposerPoisoned, err)}
	}
	if err != nil {
		return nil, &SelfTestFailure{Result: c.proof, Err: err}
	}
	if err = c.ownedTest.close(); err != nil {
		return nil, err
	}
	c.ownedTest = nil
	return c, nil
}

func (c *Composer) SelfTestStatus() SelfTestResult {
	if c == nil {
		return SelfTestResult{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.proof
}

// ProbeAcceptance uses the same fail-closed path as OpenComposer, without
// drawing on the visible half. It does overwrite a small part of the raw half,
// so the next scaler frame must refill that entire half. OpenComposer repeats
// the proof;
// callers cannot pass these booleans to bypass it.
func ProbeAcceptance(ctx context.Context) (HardwareAcceptance, SelfTestResult, error) {
	const side = 64
	c, err := OpenComposer(ctx, Rect{W: side, H: side}, make([]uint32, side*side))
	if err != nil {
		var failure *SelfTestFailure
		if errors.As(err, &failure) {
			result := failure.Result
			return HardwareAcceptance{}, result, err
		}
		return HardwareAcceptance{}, SelfTestResult{}, err
	}
	result := c.SelfTestStatus()
	if err := c.Close(); err != nil {
		return HardwareAcceptance{}, result, err
	}
	return HardwareAcceptance{SourceCopyARGB32: true, PremultipliedSrcOver: true}, result, nil
}

func (c *Composer) ComposeFrame(ctx context.Context) (FrameResult, error) {
	if c == nil || ctx == nil {
		return FrameResult{}, os.ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return FrameResult{}, os.ErrClosed
	}
	if c.poisoned {
		return FrameResult{}, ErrComposerPoisoned
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
		return FrameResult{}, fmt.Errorf("G2D frame requires a deadline within one second")
	}
	result, uncertain, err := runFrame(ctx, c.mapping, c.scratchMem, c.overlay, c.rect, func(cmd *task, sources *[g2dMaxSources]image) error {
		e := c.tasks.process(cmd, sources[:cmd.NumSources])
		runtime.KeepAlive(sources)
		runtime.KeepAlive(cmd)
		runtime.KeepAlive(c.mapping)
		runtime.KeepAlive(c.scratchMem)
		runtime.KeepAlive(c.overlay)
		return e
	}, c.blur)
	if uncertain {
		c.poisoned = true
	}
	if result.Composed {
		c.frames++
	}
	return result, err
}

func (c *Composer) UpdateOverlay(physicalPremultipliedARGB []uint32) error {
	if c == nil {
		return os.ErrClosed
	}
	next, err := newStaticOverlay(c.rect, physicalPremultipliedARGB)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.poisoned {
		_ = next.close()
		if c.poisoned {
			return ErrComposerPoisoned
		}
		return os.ErrClosed
	}
	old := c.overlay
	c.overlay = next
	return old.close()
}

func (c *Composer) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.poisoned {
		c.retainOnFault()
		return ErrComposerPoisoned
	}
	return c.closeOwned()
}

//go:build linux && (amd64 || arm64)

package decon

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"perimode/native/internal/fimg2d"
	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
)

type PresenterStats struct {
	NV12Frames     uint64 `json:"direct_nv12_frames"`
	LayerUpdates   uint64 `json:"layer_updates"`
	HardwareLayers bool   `json:"hardware_layers"`
	Frames         uint64 `json:"frames"`
	LastCopyUS     int64  `json:"last_copy_us"`
	LastFenceUS    int64  `json:"last_fence_us"`
	LastPaceUS     int64  `json:"last_pace_us"`
	LastSubmitUS   int64  `json:"last_submit_us"`
	RetiredVideo   uint64 `json:"retired_video_frames"`
	PendingRetires int    `json:"pending_retire_fences"`
	Active         bool   `json:"active"`
	Error          string `json:"error,omitempty"`
}

// Buffer.mu is the single caller/owner. Exactly two persistent scanout slots.
type Presenter struct {
	device                  *os.File
	copy                    *fimg2d.FrameCopier
	frames                  [2]*os.File
	pixels                  [2][]byte
	current                 int
	retireFD                int
	retired                 []retiredVideo
	waitFenceTest           func(int, int32) error
	submitConfigTest        func(winConfigData) (int, error)
	poisoned                bool
	closed                  bool
	stats                   PresenterStats
	lastSubmit              time.Time
	nextSubmit              time.Time
	video                   media.Image
	cameraView              *CameraView
	menuLayer, previewLayer RGBLayer
	layerRotation           int
	uncertainVideo          []*media.FrameLease
}

var retainedPresenters struct {
	sync.Mutex
	items []*Presenter
}

func OpenPresenter(device *os.File, v linuxio.FBVariable, f linuxio.FBFixed) (_ *Presenter, err error) {
	if device == nil {
		return nil, fmt.Errorf("DECON framebuffer owner missing")
	}
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil || strings.TrimSpace(string(kernel)) != pinnedKernel {
		return nil, fmt.Errorf("DECON needs pinned e418 kernel")
	}
	if err = checkFBLayout(v, f); err != nil {
		return nil, err
	}
	if err = verifyABI(); err != nil {
		return nil, err
	}
	p := &Presenter{device: device, current: -1, retireFD: -1}
	defer func() {
		if err != nil {
			_ = p.Close()
		}
	}()
	p.copy, err = fimg2d.OpenFrameCopier()
	if err != nil {
		return nil, err
	}
	for i := range p.frames {
		p.frames[i], err = linuxio.NewIONDMABuf(panelBytes)
		if err != nil {
			return nil, err
		}
		p.pixels[i], err = syscall.Mmap(int(p.frames[i].Fd()), 0, panelBytes, syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *Presenter) fail(err error) error {
	p.poisoned = true
	p.stats.Error = err.Error()
	return err
}

func (p *Presenter) Present(completed []byte) error {
	if p == nil || p.closed {
		return os.ErrClosed
	}
	if p.poisoned {
		return fmt.Errorf("DECON ownership uncertain: %s", p.stats.Error)
	}
	next := 0
	if p.current == 0 {
		next = 1
	}
	started := time.Now()
	if err := p.copy.Copy(completed, int(p.frames[next].Fd())); err != nil {
		return p.fail(err)
	}
	p.stats.LastCopyUS = time.Since(started).Microseconds()
	return p.presentNext(next)
}

func (p *Presenter) BackFD() (int, error) {
	if p == nil || p.closed {
		return -1, os.ErrClosed
	}
	if p.poisoned {
		return -1, fmt.Errorf("DECON ownership uncertain")
	}
	next := 0
	if p.current == 0 {
		next = 1
	}
	return int(p.frames[next].Fd()), nil
}

func (p *Presenter) PresentReady(fd int) error {
	back, err := p.BackFD()
	if err != nil {
		return err
	}
	if fd != back {
		return fmt.Errorf("DECON completed frame is not the back slot")
	}
	next := 0
	if p.current == 0 {
		next = 1
	}
	p.stats.LastCopyUS = 0
	return p.presentNext(next)
}

func (p *Presenter) presentNext(next int) error {
	config, err := fullFrame(int(p.frames[next].Fd()))
	if err != nil {
		return err
	}
	if err = p.presentConfig(config); err != nil {
		return err
	}
	p.current = next
	return nil
}

func (p *Presenter) presentConfig(config winConfigData) error {
	paceStart := time.Now()
	if delay := presentationDelay(p.UsesLayers(), p.nextSubmit, paceStart); delay > 0 {
		time.Sleep(delay)
	}
	started := time.Now()
	p.stats.LastPaceUS = started.Sub(paceStart).Microseconds()
	p.lastSubmit = started
	p.nextSubmit = nextPresentationDeadline(p.nextSubmit, started)
	fence, err := p.submitConfig(config)
	if err != nil {
		return p.fail(err)
	}
	previous := p.retireFD
	waitLimit := 250 * time.Millisecond
	if previous < 0 {
		// DSI timeline_max starts one ahead of the timeline. The fence for
		// frame N signals only after N+1 retires it. Bootstrap with a second
		// submission of the SAME completed buffer, with no intervening write.
		previous = fence
		// The first worker also drains the bootloader's pending shadow update
		// (the pinned kernel allows 300 ms). Steady-state waits stay bounded.
		waitLimit = 750 * time.Millisecond
		fence, err = p.submitConfig(config)
		if err != nil {
			_ = syscall.Close(previous)
			return p.fail(err)
		}
	}
	defer syscall.Close(previous)
	p.retireFD = fence
	if err = p.waitRetired(previous, waitLimit); err != nil {
		return p.fail(err)
	}
	if err = p.reclaimRetired(true); err != nil {
		return p.fail(err)
	}
	// The previous front slot is now released. This new front remains
	// immutable until its own fence signals after the next submission.
	p.stats.Active = true
	p.stats.Frames++
	p.stats.LastFenceUS = time.Since(started).Microseconds()
	return nil
}

func (p *Presenter) ClearBack() error {
	fd, err := p.BackFD()
	if err != nil {
		return err
	}
	if err = p.copy.ClearDMA(fd); err != nil {
		return p.fail(err)
	}
	return nil
}

func (p *Presenter) CopyCurrentTo(fd int) error {
	if p == nil || p.closed || p.poisoned {
		return fmt.Errorf("scanout unavailable")
	}
	if p.current < 0 {
		return p.copy.ClearDMA(fd)
	}
	return p.copy.CopyDMAFrame(int(p.frames[p.current].Fd()), fd)
}
func (p *Presenter) CopyToBack(sourceFD int) (int, error) {
	fd, err := p.BackFD()
	if err != nil {
		return -1, err
	}
	if err = p.copy.CopyDMAFrame(sourceFD, fd); err != nil {
		return -1, p.fail(err)
	}
	return fd, nil
}

func (p *Presenter) RestoreCurrent(target []byte) error {
	if p == nil || p.closed {
		return os.ErrClosed
	}
	if p.poisoned {
		return fmt.Errorf("DECON ownership uncertain")
	}
	if p.current < 0 {
		return p.ClearWorkspace(target)
	}
	if err := p.copy.CopyDMAWorkspace(int(p.frames[p.current].Fd()), target); err != nil {
		return p.fail(err)
	}
	return nil
}

func (p *Presenter) FillRectangle(target []byte, back bool, r fimg2d.Rect, color uint32) error {
	fd := -1
	if p == nil || p.closed {
		return os.ErrClosed
	}
	if p.poisoned {
		return fmt.Errorf("DECON ownership uncertain")
	}
	if back {
		var err error
		fd, err = p.BackFD()
		if err != nil {
			return err
		}
	}
	if err := p.copy.FillRectangle(target, fd, r, color); err != nil {
		return p.fail(err)
	}
	return nil
}

func (p *Presenter) CloneCurrentBack() (int, error) {
	fd, err := p.BackFD()
	if err != nil {
		return -1, err
	}
	if p.current < 0 {
		err = p.copy.ClearDMA(fd)
	} else {
		err = p.copy.CopyDMAFrame(int(p.frames[p.current].Fd()), fd)
	}
	if err != nil {
		return -1, p.fail(err)
	}
	return fd, nil
}

func (p *Presenter) Preview(target []byte, back bool, sourceFD, width, height, bytes int, r fimg2d.Rect) error {
	if p == nil || p.closed {
		return os.ErrClosed
	}
	if p.poisoned {
		return fmt.Errorf("DECON ownership uncertain")
	}
	fd := -1
	if back {
		var err error
		fd, err = p.BackFD()
		if err != nil {
			return err
		}
	}
	if err := p.copy.Preview(target, fd, sourceFD, width, height, bytes, r); err != nil {
		return p.fail(err)
	}
	return nil
}

func (p *Presenter) submit(fd int) (int, error) {
	config, err := fullFrame(fd)
	if err != nil {
		return -1, err
	}
	return p.submitConfig(config)
}
func (p *Presenter) submitConfig(config winConfigData) (int, error) {
	if p.submitConfigTest != nil {
		return p.submitConfigTest(config)
	}
	started := time.Now()
	err := linuxio.Ioctl(int(p.device.Fd()), winConfigIOCTL, unsafe.Pointer(&config))
	p.stats.LastSubmitUS = time.Since(started).Microseconds()
	if err != nil {
		if config.Fence >= 0 {
			_ = syscall.Close(int(config.Fence))
		}
		return -1, fmt.Errorf("DECON WIN_CONFIG: %w", err)
	}
	if config.Fence < 0 {
		return -1, fmt.Errorf("DECON returned no retire fence")
	}
	syscall.CloseOnExec(int(config.Fence))
	return int(config.Fence), nil
}

func (p *Presenter) waitRetired(fence int, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	var err error
	for {
		remaining := time.Until(deadline).Milliseconds()
		if remaining <= 0 {
			return fmt.Errorf("DECON retire fence timeout")
		}
		timeout := int32(remaining)
		err = linuxio.Ioctl(fence, syncWait, unsafe.Pointer(&timeout))
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("DECON retire fence: %w", err)
	}
	return nil
}

func (p *Presenter) CurrentPixels() []byte {
	if p == nil || p.current < 0 || p.poisoned {
		return nil
	}
	return p.pixels[p.current]
}
func (p *Presenter) Stats() PresenterStats { return p.stats }

func (p *Presenter) RestoreWorkspace(source, target []byte) error {
	if p == nil || p.closed {
		return os.ErrClosed
	}
	if p.poisoned {
		return fmt.Errorf("DECON ownership uncertain")
	}
	if err := p.copy.CopyWorkspace(source, target); err != nil {
		return p.fail(err)
	}
	return nil
}

func (p *Presenter) ClearWorkspace(target []byte) error {
	if p == nil || p.closed {
		return os.ErrClosed
	}
	if p.poisoned {
		return fmt.Errorf("DECON ownership uncertain")
	}
	if err := p.copy.ClearWorkspace(target); err != nil {
		return p.fail(err)
	}
	return nil
}

func (p *Presenter) Close() error {
	if p == nil || p.closed {
		return nil
	}
	if p.UsesLayers() && !p.poisoned {
		if err := p.PresentLayers(media.Image{}, p.layerRotation, RGBLayer{}, RGBLayer{}); err != nil {
			return err
		}
	}
	p.closed = true
	if len(p.retired) != 0 {
		p.fail(fmt.Errorf("DECON retired leases were not confirmed at close"))
	}
	if p.retireFD >= 0 {
		_ = syscall.Close(p.retireFD)
		p.retireFD = -1
	}
	if p.poisoned {
		retainedPresenters.Lock()
		retainedPresenters.items = append(retainedPresenters.items, p)
		retainedPresenters.Unlock()
		return fmt.Errorf("DECON/G2D buffers retained until reboot: %s", p.stats.Error)
	}
	var err error
	if p.copy != nil {
		err = p.copy.Close()
	}
	for i, f := range p.frames {
		if p.pixels[i] != nil {
			err = errors.Join(err, syscall.Munmap(p.pixels[i]))
		}
		if f != nil {
			err = errors.Join(err, f.Close())
		}
	}
	// The kernel retains a reference to its current front buffer after fd close.
	return err
}

// Diagnostic-only RGB workspace, never submitted while hardware layers are on.
func (p *Presenter) DiagnosticBuffer() (int, []byte, error) {
	if p == nil || p.closed || p.poisoned || !p.UsesLayers() {
		return -1, nil, fmt.Errorf("layer capture unavailable")
	}
	return int(p.frames[0].Fd()), p.pixels[0], nil
}
func (p *Presenter) CaptureClear(fd int) error { return p.copy.ClearDMA(fd) }
func (p *Presenter) CaptureCopy(source, destination int) error {
	return p.copy.CopyDMAFrame(source, destination)
}
func (p *Presenter) CapturePreview(destination int, layer RGBLayer) error {
	if !layer.Enabled {
		return nil
	}
	if layer.Blank {
		return p.copy.FillRectangle(nil, destination, layer.Rect, 0xff000000)
	}
	return p.copy.Preview(nil, destination, layer.FD, layer.Width, layer.Height, 4<<20, layer.Rect)
}

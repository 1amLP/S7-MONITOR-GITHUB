//go:build linux && (amd64 || arm64)

package fimg2d

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"
)

// FrameCopier publishes the completed offscreen composition to an inactive
// scanout DMA-BUF. It never modifies a buffer currently owned by the panel.
type FrameCopier struct {
	tasks      *taskContexts
	held       []byte
	failed     bool
	workspaces [2]uintptr
}

func OpenFrameCopier() (*FrameCopier, error) {
	if err := verifyABI(); err != nil {
		return nil, err
	}
	return &FrameCopier{tasks: &taskContexts{}}, nil
}

func (c *FrameCopier) Copy(source []byte, targetFD int) error {
	if targetFD < 0 {
		return fmt.Errorf("invalid scanout target")
	}
	full := Rect{W: panelWidth, H: panelHeight}
	return c.run(source, dmaImage(targetFD, panelWidth, panelHeight, frameBytes, full, full, 0, 0, 0))
}

func (c *FrameCopier) CopyWorkspace(source, target []byte) error {
	if len(target) != frameBytes {
		return fmt.Errorf("invalid workspace target")
	}
	full := Rect{W: panelWidth, H: panelHeight}
	err := c.run(source, userImage(target, panelWidth, panelHeight, full, full, 0, 0, 0))
	runtime.KeepAlive(target)
	return err
}

func (c *FrameCopier) run(source []byte, dst image) error {
	if c == nil || c.tasks == nil {
		return os.ErrClosed
	}
	if c.failed {
		return ErrComposerPoisoned
	}
	if len(source) != frameBytes {
		return fmt.Errorf("invalid scanout copy extent")
	}
	c.prefault(source)
	full := Rect{W: panelWidth, H: panelHeight}
	src := userImage(source, panelWidth, panelHeight, full, full, 0, blendSource, 0)
	cmd := task{Sources: uintptr(unsafe.Pointer(&src)), Target: dst, NumSources: 1}
	c.held = source
	err := c.tasks.process(&cmd, unsafe.Slice(&src, 1))
	runtime.KeepAlive(&src)
	runtime.KeepAlive(source)
	if err == nil {
		err = completeTask(&cmd, frameBytes)
	}
	if err != nil {
		c.failed = true
		return fmt.Errorf("G2D scanout copy: %w", err)
	}
	c.held = nil
	return nil
}

func (c *FrameCopier) Close() error {
	if c == nil || c.tasks == nil {
		return nil
	}
	if c.failed {
		return ErrComposerPoisoned
	}
	err := c.tasks.Close()
	if err == nil {
		c.tasks = nil
	}
	return err
}

func (c *FrameCopier) ClearWorkspace(target []byte) error {
	if c == nil || c.tasks == nil {
		return os.ErrClosed
	}
	if c.failed {
		return ErrComposerPoisoned
	}
	if len(target) != frameBytes {
		return fmt.Errorf("invalid GPU clear extent")
	}
	c.prefault(target)
	full := Rect{W: panelWidth, H: panelHeight}
	c.held = target
	err := c.clearImage(userImage(target, panelWidth, panelHeight, full, full, 0, 0, 0))
	runtime.KeepAlive(target)
	if err == nil {
		c.held = nil
	}
	return err
}

func (c *FrameCopier) ClearDMA(fd int) error {
	if fd < 0 {
		return fmt.Errorf("invalid GPU clear fd")
	}
	full := Rect{W: panelWidth, H: panelHeight}
	return c.clearImage(dmaImage(fd, panelWidth, panelHeight, frameBytes, full, full, 0, 0, 0))
}

func (c *FrameCopier) clearImage(dst image) error {
	if c == nil || c.tasks == nil {
		return os.ErrClosed
	}
	if c.failed {
		return ErrComposerPoisoned
	}
	src := image{Flags: colorFill, Fence: -1, Memory: bufferEmpty,
		Format: format{Width: panelWidth, Height: panelHeight, PixelFormat: formatARGB32, Crop: rect{Width: panelWidth, Height: panelHeight}, Window: rect{Width: panelWidth, Height: panelHeight}},
		Extra:  extra{FillColor: 0xff000000, Composite: blendSource}}
	cmd := task{Sources: uintptr(unsafe.Pointer(&src)), Target: dst, NumSources: 1}
	err := c.tasks.process(&cmd, unsafe.Slice(&src, 1))
	runtime.KeepAlive(&src)
	if err == nil {
		err = completeTask(&cmd, frameBytes)
	}
	if err != nil {
		c.failed = true
		return err
	}
	c.held = nil
	return nil
}

func (c *FrameCopier) prefault(data []byte) {
	address := uintptrOf(data)
	for _, seen := range c.workspaces {
		if seen == address {
			return
		}
	}
	var value byte
	for offset := 0; offset < len(data); offset += 4096 {
		value ^= data[offset]
	}
	prefaultChecksum.Store(uint32(value))
	for i, seen := range c.workspaces {
		if seen == 0 {
			c.workspaces[i] = address
			return
		}
	}
}

func (c *FrameCopier) CopyDMAWorkspace(fd int, target []byte) error {
	if c == nil || c.tasks == nil {
		return os.ErrClosed
	}
	if c.failed {
		return ErrComposerPoisoned
	}
	if fd < 0 || len(target) != frameBytes {
		return fmt.Errorf("invalid GPU restore")
	}
	c.prefault(target)
	full := Rect{W: panelWidth, H: panelHeight}
	src := dmaImage(fd, panelWidth, panelHeight, frameBytes, full, full, 0, blendSource, 0)
	dst := userImage(target, panelWidth, panelHeight, full, full, 0, 0, 0)
	c.held = target
	err := c.imageTask(src, dst)
	runtime.KeepAlive(target)
	if err == nil {
		c.held = nil
	}
	return err
}

func (c *FrameCopier) FillRectangle(target []byte, fd int, r Rect, color uint32) error {
	if c == nil || c.tasks == nil {
		return os.ErrClosed
	}
	if c.failed {
		return ErrComposerPoisoned
	}
	if r.X < 0 || r.Y < 0 || r.W <= 0 || r.H <= 0 || r.X+r.W > panelWidth || r.Y+r.H > panelHeight {
		return fmt.Errorf("GPU rectangle outside panel")
	}
	var dst image
	if fd >= 0 {
		dst = dmaImage(fd, panelWidth, panelHeight, frameBytes, r, r, 0, 0, 0)
	} else {
		if len(target) != frameBytes {
			return fmt.Errorf("invalid GPU rectangle target")
		}
		c.prefault(target)
		dst = userImage(target, panelWidth, panelHeight, r, r, 0, 0, 0)
		c.held = target
	}
	src := image{Flags: colorFill, Fence: -1, Memory: bufferEmpty,
		Format: format{Width: uint32(r.W), Height: uint32(r.H), PixelFormat: formatARGB32, Crop: rect{Width: uint32(r.W), Height: uint32(r.H)}, Window: rect{Left: int32(r.X), Top: int32(r.Y), Width: uint32(r.W), Height: uint32(r.H)}},
		Extra:  extra{FillColor: color, Composite: blendSource}}
	err := c.imageTask(src, dst)
	runtime.KeepAlive(target)
	if err == nil {
		c.held = nil
	}
	return err
}

func (c *FrameCopier) imageTask(src, dst image) error {
	cmd := task{Sources: uintptr(unsafe.Pointer(&src)), Target: dst, NumSources: 1}
	err := c.tasks.process(&cmd, unsafe.Slice(&src, 1))
	runtime.KeepAlive(&src)
	if err == nil {
		err = completeTask(&cmd, frameBytes)
	}
	if err != nil {
		c.failed = true
	}
	return err
}

func (c *FrameCopier) CopyDMAFrame(sourceFD, targetFD int) error {
	if c == nil || c.tasks == nil {
		return os.ErrClosed
	}
	if c.failed {
		return ErrComposerPoisoned
	}
	if sourceFD < 0 || targetFD < 0 || sourceFD == targetFD {
		return fmt.Errorf("invalid scanout copy fds")
	}
	full := Rect{W: panelWidth, H: panelHeight}
	return c.imageTask(dmaImage(sourceFD, panelWidth, panelHeight, frameBytes, full, full, 0, blendSource, 0), dmaImage(targetFD, panelWidth, panelHeight, frameBytes, full, full, 0, 0, 0))
}

// Fill a page-aligned NV12 plane with one repeated byte. This is a hardware
// initialization of padding/letterbox bars, never a CPU pixel loop.
func (c *FrameCopier) FillPlane(fd, bytes int, value byte) error {
	if c == nil || c.tasks == nil {
		return os.ErrClosed
	}
	if c.failed {
		return ErrComposerPoisoned
	}
	if fd < 0 || bytes <= 0 || bytes%4096 != 0 || bytes > 16<<20 {
		return fmt.Errorf("invalid DMA plane fill")
	}
	r := Rect{W: 1024, H: bytes / 4096}
	src := image{Flags: colorFill, Fence: -1, Memory: bufferEmpty, Format: format{Width: 1024, Height: uint32(r.H), PixelFormat: formatARGB32, Crop: rect{Width: 1024, Height: uint32(r.H)}, Window: rect{Width: 1024, Height: uint32(r.H)}}, Extra: extra{FillColor: uint32(value) * 0x01010101, Composite: blendSource}}
	cmd := task{Sources: uintptr(unsafe.Pointer(&src)), Target: dmaImage(fd, 1024, r.H, bytes, r, r, 0, 0, 0), NumSources: 1}
	err := c.tasks.process(&cmd, unsafe.Slice(&src, 1))
	if err == nil {
		err = completeTask(&cmd, bytes)
	}
	if err != nil {
		c.failed = true
	}
	return err
}

func (c *FrameCopier) Preview(target []byte, targetFD, sourceFD, width, height, bytes int, r Rect) error {
	if c == nil || c.tasks == nil {
		return os.ErrClosed
	}
	if c.failed {
		return ErrComposerPoisoned
	}
	if sourceFD < 0 || sourceFD == targetFD || r.X < 0 || r.Y < 0 || r.W <= 0 || r.H <= 0 || r.X+r.W > panelWidth || r.Y+r.H > panelHeight || width < 2 || height < 2 || width > 1024 || height > 1024 || bytes < width*height*4 || bytes > 4<<20 {
		return fmt.Errorf("invalid GPU Preview geometry")
	}
	var dst image
	if targetFD >= 0 {
		dst = dmaImage(targetFD, panelWidth, panelHeight, frameBytes, r, r, 0, 0, 0)
	} else {
		if len(target) != frameBytes {
			return fmt.Errorf("invalid GPU Preview workspace")
		}
		c.prefault(target)
		dst = userImage(target, panelWidth, panelHeight, r, r, 0, 0, 0)
		c.held = target
	}
	// e418 g2d1shot_hw5x handles flips only; ROT90/180/270 are ignored.
	// Mali supplies an already panel-oriented texture, including its black bars.
	src := dmaImage(sourceFD, width, height, bytes, Rect{W: width, H: height}, r, 0, blendSource, bilinearFilter)
	err := c.imageTask(src, dst)
	runtime.KeepAlive(target)
	if err == nil {
		c.held = nil
	}
	return err
}

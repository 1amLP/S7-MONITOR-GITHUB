//go:build linux && (amd64 || arm64)

package fimg2d

import (
	"context"
	"fmt"
	"time"
	"unsafe"
)

// Scene composes video plus a cached premultiplied menu directly into back
// scanout. Both inputs are DMA-BUF; no blur pass or full-frame copy follows it.
type Scene struct {
	tasks          taskContexts
	video, overlay int
	target         func() (int, error)
	failed         bool
}

func OpenScene(video, overlay int, target func() (int, error)) (*Scene, error) {
	if video < 0 || overlay < 0 || video == overlay || target == nil {
		return nil, fmt.Errorf("invalid DMA scene")
	}
	if err := verifyABI(); err != nil {
		return nil, err
	}
	return &Scene{video: video, overlay: overlay, target: target}, nil
}
func (s *Scene) ComposeFrame(ctx context.Context) (FrameResult, error) {
	var result FrameResult
	if s.failed {
		return result, ErrComposerPoisoned
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	fd, err := s.target()
	if err != nil {
		return result, err
	}
	if fd == s.video || fd == s.overlay {
		return result, fmt.Errorf("scene output aliases an input")
	}
	full := Rect{W: panelWidth, H: panelHeight}
	sources := [2]image{dmaImage(s.video, panelWidth, panelHeight, frameBytes, full, full, 0, blendSource, 0), dmaImage(s.overlay, panelWidth, panelHeight, frameBytes, full, full, premultAlpha, blendSourceOver, 0)}
	cmd := task{Sources: uintptr(unsafe.Pointer(&sources[0])), Target: dmaImage(fd, panelWidth, panelHeight, frameBytes, full, full, 0, 0, 0), NumSources: 2}
	start := time.Now()
	err = s.tasks.process(&cmd, sources[:])
	if err == nil {
		err = completeTask(&cmd, frameBytes)
	}
	if err != nil {
		s.failed = true
		return result, err
	}
	return FrameResult{Composed: true, CompositeUS: time.Since(start).Microseconds()}, nil
}
func (s *Scene) UpdateOverlay([]uint32) error {
	return fmt.Errorf("DMA scene accepts GPU overlay only")
}
func (s *Scene) Close() error {
	if s.failed {
		return ErrComposerPoisoned
	}
	return s.tasks.Close()
}

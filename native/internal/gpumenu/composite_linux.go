//go:build linux && (amd64 || arm64)

package gpumenu

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"

	"perimode/native/internal/fimg2d"
	"perimode/native/internal/media"
)

// The framebuffer owner keeps these persistent fds alive through GPU completion.
// The renderer imports them once; only a bounded command travels through the pipe.
func (r *Renderer) composite(video, destination int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return os.ErrClosed
	}
	if r.failed != nil {
		return r.failed
	}
	if video < 0 || destination < 0 || video == destination || video == int(r.dma.Fd()) || destination == int(r.dma.Fd()) {
		return fmt.Errorf("invalid GPU composition buffers")
	}
	deadline := time.Now().Add(250 * time.Millisecond)
	_ = r.in.SetWriteDeadline(deadline)
	_ = r.out.SetReadDeadline(deadline)
	input, err := r.importBuffers(1<<60|uint64(video+1), []media.DMAPlane{{FD: video, Length: FrameBytes}}, false)
	var output int
	if err == nil {
		output, err = r.importBuffers(1<<61|uint64(destination+1), []media.DMAPlane{{FD: destination, Length: FrameBytes}}, true)
	}
	if err == nil {
		head := [8]uint32{0x31434d47, r.nextSequence(), uint32(input), uint32(output), Width, Height}
		err = binary.Write(r.in, binary.LittleEndian, head)
		if err == nil {
			err = r.ack(head[1])
		}
	}
	if err != nil {
		r.failed = fmt.Errorf("Mali DMA composition: %w; %s", err, r.stderr.String())
	}
	return r.failed
}

type Scene struct {
	renderer *Renderer
	video    int
	target   func() (int, error)
	failed   bool
}

func NewScene(renderer *Renderer, video int, target func() (int, error)) (*Scene, error) {
	if renderer == nil || video < 0 || target == nil {
		return nil, fmt.Errorf("missing GPU scene owner")
	}
	return &Scene{renderer: renderer, video: video, target: target}, nil
}
func (s *Scene) ComposeFrame(ctx context.Context) (fimg2d.FrameResult, error) {
	if err := ctx.Err(); err != nil {
		return fimg2d.FrameResult{}, err
	}
	if s.failed {
		return fimg2d.FrameResult{}, fimg2d.ErrComposerPoisoned
	}
	fd, err := s.target()
	if err == nil {
		err = s.renderer.composite(s.video, fd)
	}
	if err != nil {
		s.failed = true
		return fimg2d.FrameResult{}, err
	}
	return fimg2d.FrameResult{Composed: true, CompositeUS: int64(s.renderer.LastUS)}, nil
}
func (s *Scene) UpdateOverlay([]uint32) error { return fmt.Errorf("GPU commands only") }
func (s *Scene) Close() error {
	if s.failed {
		// Do not free scanout or source fds while a lost GPU reply could mean
		// outstanding DMA. The caller retains them on this explicit error.
		return errors.Join(fimg2d.ErrComposerPoisoned, s.renderer.Close())
	}
	return nil
}

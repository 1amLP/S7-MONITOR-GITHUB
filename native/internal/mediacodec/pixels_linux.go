//go:build linux && (amd64 || arm64)

package mediacodec

import (
	"fmt"
	"perimode/native/internal/media"
	"os"
	"time"
)

// submitPixels fills the single sealed raw-frame slot directly. The worker
// consumes it before acknowledging this request; EAGAIN ends the transaction
// without handing the slot to the codec. A caller may then safely refill it.
func (s *session) submitPixels(im media.Image, timeout time.Duration) error {
	s.tx.Lock()
	defer s.tx.Unlock()
	if s.closed {
		return os.ErrClosed
	}
	if s.ctx != nil && s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	if s.poisoned != nil {
		return s.poisoned
	}
	w, h := im.Width, im.Height
	if s.role != roleEncode || w <= 0 || h <= 0 || w > 2560 || h > 1440 || (w|h)&1 != 0 || im.StrideY < w || im.StrideUV < w || im.StrideY > 8192 || im.StrideUV > 8192 ||
		len(im.Y) < (h-1)*im.StrideY+w || len(im.UV) < (h/2-1)*im.StrideUV+w || im.PTS < 0 || w*h*3/2 > len(s.pixels) {
		return fmt.Errorf("invalid shared NV12 input")
	}
	copyPixelPlanes(s.pixels, im)
	_, _, e := s.callLocked(header{op: opSubmit, pts: uint64(im.PTS), size: uint32(w * h * 3 / 2), storage: storagePixels}, nil, timeout)
	return e
}

// Destination is a private slot, never a DMA pointer; copy visible rows only.
func copyPixelPlanes(out []byte, im media.Image) {
	w, h := im.Width, im.Height
	if im.StrideY == w {
		copy(out[:w*h], im.Y[:w*h])
	} else {
		for y := 0; y < h; y++ {
			copy(out[y*w:(y+1)*w], im.Y[y*im.StrideY:y*im.StrideY+w])
		}
	}
	uv := out[w*h : w*h*3/2]
	if im.StrideUV == w {
		copy(uv, im.UV[:w*h/2])
	} else {
		for y := 0; y < h/2; y++ {
			copy(uv[y*w:(y+1)*w], im.UV[y*im.StrideUV:y*im.StrideUV+w])
		}
	}
}

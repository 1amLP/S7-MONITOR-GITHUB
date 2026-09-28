package fb

import (
	"perimode/native/internal/orientation"
	"image"
)

type inputGeometry struct {
	width, height int
	rotation      orientation.Degrees
	preview       image.Rectangle
}

// Called under Buffer.mu, or before a new Buffer is exposed to other workers.
// The pointer changes only when geometry does, never for each video frame.
func (b *Buffer) publishInputGeometry() {
	g := inputGeometry{width: b.Width, height: b.Height, rotation: b.Rotation}
	if !b.closed && b.preview != nil {
		g.preview = b.preview.rect
	}
	if old := b.inputView.Load(); old == nil || *old != g {
		b.inputView.Store(&inputGeometry{width: g.width, height: g.height, rotation: g.rotation, preview: g.preview})
	}
}

func (b *Buffer) inputGeometry() inputGeometry {
	if g := b.inputView.Load(); g != nil {
		return *g
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishInputGeometry()
	return *b.inputView.Load()
}

func (b *Buffer) InputSize() (int, int) { g := b.inputGeometry(); return g.width, g.height }

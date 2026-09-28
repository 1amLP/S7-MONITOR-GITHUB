package fb

import (
	"fmt"
	"os"
	"time"
)

// Capture uses committed layer buffers, not a re-render of the UI model. A
// hardware-layer snapshot is composed on demand; it is not panel readback.
// Pixel conversion/PNG encoding remains outside the display frame loop.
type Capture struct {
	Width, Height, Stride, Bits int
	Rotation                    int
	Red, Green, Blue            BitField
	Pixels                      []byte `json:"-"`
}

func (b *Buffer) Capture() (Capture, error) {
	if !b.capturePending.CompareAndSwap(false, true) {
		return Capture{}, fmt.Errorf("capture already pending")
	}
	defer b.capturePending.Store(false)
	deadline := time.Now().Add(100 * time.Millisecond)
	for !b.mu.TryLock() {
		if time.Now().After(deadline) {
			return Capture{}, fmt.Errorf("framebuffer busy; retry capture")
		}
		time.Sleep(time.Millisecond)
	}
	defer b.mu.Unlock()
	if b.closed {
		return Capture{}, os.ErrClosed
	}
	if err := b.Validate(); err != nil {
		return Capture{}, err
	}
	if b.V.X > 2560 || b.V.Y > 2560 || len(b.Data) < int(b.F.MemoryLength) {
		return Capture{}, fmt.Errorf("capture geometry exceeds S7 bounds")
	}
	stride := int(b.V.X * (b.V.Bits / 8))
	c := Capture{Width: int(b.V.X), Height: int(b.V.Y), Stride: stride, Bits: int(b.V.Bits),
		Rotation: int(b.Rotation), Red: b.V.Red, Green: b.V.Green, Blue: b.V.Blue,
		Pixels: make([]byte, stride*int(b.V.Y))}
	source := b.Data
	if b.scanout != nil && b.scanout.UsesLayers() {
		var err error
		source, err = b.captureLayersLocked()
		if err != nil {
			return Capture{}, err
		}
	} else if b.scanout != nil {
		source = b.scanout.CurrentPixels()
		if len(source) != len(c.Pixels) {
			return Capture{}, fmt.Errorf("DECON has no completed scanout")
		}
	}
	for y := 0; y < c.Height; y++ {
		off := (y+int(b.V.YOffset))*int(b.F.LineLength) + int(b.V.XOffset)*int(b.V.Bits/8)
		copy(c.Pixels[y*stride:(y+1)*stride], source[off:off+stride])
	}
	return c, nil
}

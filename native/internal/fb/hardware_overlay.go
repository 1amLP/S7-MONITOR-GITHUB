//go:build linux && (amd64 || arm64)

package fb

import (
	"fmt"
	"image"
)

// hardwareMenuOverlay prepares one premultiplied ARGB layer when menu content
// changes. The video-frame path only submits this layer to the 2D engine.
func (b *Buffer) hardwareMenuOverlay(sc *glassScene, l GlassLayout) (image.Rectangle, []uint32, error) {
	panel := l.Panel
	if sc == nil || sc.ink == nil || sc.ink.rect != panel ||
		len(sc.ink.pixels) != panel.Dx()*panel.Dy() || panel.Empty() ||
		!panel.In(image.Rect(0, 0, b.Width, b.Height)) {
		return image.Rectangle{}, nil, fmt.Errorf("menu foreground is not ready for hardware composition")
	}
	ink := &glassInk{rect: panel, pixels: append([]uint32(nil), sc.ink.pixels...)}
	if !l.Statistics {
		previous := b.inkTarget
		b.inkTarget = ink
		b.paintGlassStatus(l)
		b.inkTarget = previous
	}
	physical := image.Rectangle{Min: image.Pt(int(b.V.X), int(b.V.Y))}
	for _, p := range [4]image.Point{panel.Min, {panel.Max.X - 1, panel.Min.Y}, {panel.Min.X, panel.Max.Y - 1}, panel.Max.Sub(image.Pt(1, 1))} {
		x, y := b.Rotation.ToPanel(p.X, p.Y, int(b.V.X), int(b.V.Y))
		physical.Min.X = min(physical.Min.X, x)
		physical.Min.Y = min(physical.Min.Y, y)
		physical.Max.X = max(physical.Max.X, x+1)
		physical.Max.Y = max(physical.Max.Y, y+1)
	}
	if physical.Empty() || physical.Dx()*physical.Dy() != len(ink.pixels) {
		return image.Rectangle{}, nil, fmt.Errorf("invalid rotated menu geometry")
	}
	out := make([]uint32, len(ink.pixels))
	for y := panel.Min.Y; y < panel.Max.Y; y++ {
		for x := panel.Min.X; x < panel.Max.X; x++ {
			p := ink.pixels[(y-panel.Min.Y)*panel.Dx()+x-panel.Min.X]
			if roundedInside(panel, x, y, max(1, l.Font*5/4)) {
				// The opaque video remains visible through a dark, blurred panel.
				p = premultOver(p, 0xa0000000)
			}
			px, py := b.Rotation.ToPanel(x, y, int(b.V.X), int(b.V.Y))
			out[(py-physical.Min.Y)*physical.Dx()+px-physical.Min.X] = p
		}
	}
	return physical, out, nil
}

func premultOver(foreground, background uint32) uint32 {
	inv := 255 - int(foreground>>24)
	channel := func(shift uint) uint32 {
		f := int((foreground >> shift) & 255)
		b := int((background >> shift) & 255)
		return uint32(min(255, f+(b*inv+127)/255))
	}
	return channel(24)<<24 | channel(16)<<16 | channel(8)<<8 | channel(0)
}

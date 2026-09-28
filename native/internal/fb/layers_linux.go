//go:build linux && (amd64 || arm64)

package fb

import (
	"perimode/native/internal/decon"
	"perimode/native/internal/fimg2d"
	"perimode/native/internal/media"
	"image"
)

func (b *Buffer) desiredLayersLocked() (decon.RGBLayer, decon.RGBLayer, error) {
	var menu, preview decon.RGBLayer
	if (b.glass != nil || b.cameraHUD != nil || b.bootOrbit) && b.layerMenuFD > 0 {
		r := b.layerMenuRect
		if r.Empty() {
			r = image.Rect(0, 0, 1440, 2560)
		}
		crop := fimg2d.Rect{X: r.Min.X, Y: r.Min.Y, W: r.Dx(), H: r.Dy()}
		menu = decon.RGBLayer{Enabled: true, FD: b.layerMenuFD, Width: 1440, Height: 2560, Rect: crop, Source: crop}
	}
	if p := b.preview; p != nil {
		r := PreviewLayout(b.Width, b.Height, p.options, b.glass != nil, b.previewObstacles())
		p.rect = r
		if p.options.Fullscreen {
			return menu, preview, nil
		}
		if !r.Empty() {
			physical, err := fimg2d.PhysicalRect(fimg2d.Rect{X: r.Min.X, Y: r.Min.Y, W: r.Dx(), H: r.Dy()}, b.Rotation)
			if err != nil {
				return menu, preview, err
			}
			preview = decon.RGBLayer{Enabled: true, Blank: p.blank, FD: p.texture.FD, Width: p.texture.Width, Height: p.texture.Height, Rect: physical}
		}
	}
	return menu, preview, nil
}

// The full DMA allocation stays immutable. Only the occupied part is fetched
// by G1; no crop texture or per-frame copy is needed for a small control bar.
func (b *Buffer) overlayBounds(logical image.Rectangle) (image.Rectangle, error) {
	r, err := fimg2d.PhysicalRect(fimg2d.Rect{X: logical.Min.X, Y: logical.Min.Y, W: logical.Dx(), H: logical.Dy()}, b.Rotation)
	if err != nil {
		return image.Rectangle{}, err
	}
	return image.Rect(r.X&^7, r.Y&^1, min(1440, (r.X+r.W+7)&^7), min(2560, (r.Y+r.H+1)&^1)), nil
}

func (b *Buffer) commitLayersLocked(next *media.Image) error {
	video, _, _, _ := b.scanout.LayerState()
	if _, camera := b.scanout.CameraView(); camera {
		video = media.Image{}
	}
	if next != nil {
		video = *next
	}
	menu, preview, err := b.desiredLayersLocked()
	if err == nil {
		if p := b.preview; p != nil && p.options.Fullscreen {
			frame := p.direct
			if p.blank {
				frame = media.Image{}
			}
			err = b.scanout.PresentCamera(frame, p.view, menu)
		} else {
			err = b.scanout.PresentLayers(video, int(b.Rotation), menu, preview)
		}
	}
	if err != nil {
		b.hardwareGlassUnsafe = true
		b.hardwareGlassError = err.Error()
		return err
	}
	b.publishInputGeometry()
	return nil
}

func (b *Buffer) ReleaseVideo() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.scanout == nil {
		return nil
	}
	if _, camera := b.scanout.CameraView(); camera {
		return nil
	}
	return b.scanout.DropVideo()
}

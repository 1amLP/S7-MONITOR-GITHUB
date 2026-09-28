//go:build linux && (amd64 || arm64)

package fb

import (
	"fmt"
	"perimode/native/internal/media"
	"image"

	"perimode/native/internal/gpumenu"
)

type gpuDrawList struct {
	commands []gpumenu.Command
	err      error
}

func (b *Buffer) SetPreviewGPUFactory(factory func() (*gpumenu.Renderer, error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.previewGPUFactory = factory
}

func (b *Buffer) ClosePreviewGPU() error {
	b.previewMu.Lock()
	defer b.previewMu.Unlock()
	b.mu.Lock()
	if b.hardwareGlassUnsafe {
		b.mu.Unlock()
		return fmt.Errorf("retain Preview GPU while compositor ownership is uncertain")
	}
	r := b.previewGPU
	b.previewGPU = nil
	b.previewRevision++
	var old *media.FrameLease
	if b.preview != nil {
		old = b.preview.direct.Lease
		b.preview.direct = media.Image{}
		b.preview.blank = true
		b.preview.texture = gpumenu.PreviewTexture{}
	}
	if b.scanout != nil && b.scanout.UsesLayers() {
		if err := b.commitLayersLocked(nil); err != nil {
			b.previewGPU = r
			b.mu.Unlock()
			return err
		}
	}
	if old != nil {
		if err := old.Release(); err != nil {
			b.mu.Unlock()
			return err
		}
	}
	b.mu.Unlock()
	if r != nil {
		return r.Close()
	}
	return nil
}

func (d *gpuDrawList) add(kind int, clip image.Rectangle, rgb uint32, alpha int, geometry ...int) {
	if d.err != nil || clip.Empty() || alpha <= 0 {
		return
	}
	if len(d.commands) >= gpumenu.MaxCommands || len(geometry) > 9 {
		d.err = fmt.Errorf("GPU menu command budget exceeded")
		return
	}
	c := gpumenu.Command{int32(kind), int32(clip.Min.X), int32(clip.Min.Y), int32(clip.Max.X), int32(clip.Max.Y), int32(rgb), int32(alpha)}
	for i, v := range geometry {
		c[7+i] = int32(v)
	}
	d.commands = append(d.commands, c)
}

func MenuGlyphAtlas() []byte { loadMasks(); return maskData }

func (b *Buffer) AttachMenuGPU(renderer *gpumenu.Renderer) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return fmt.Errorf("framebuffer closed")
	}
	if renderer == nil && (b.hardwareGlassUnsafe || b.scanout != nil && b.scanout.Stats().Error != "") {
		return fmt.Errorf("GPU buffers retained while compositor ownership is uncertain")
	}
	if renderer == nil && b.scanout != nil && b.scanout.UsesLayers() {
		b.layerMenuFD = 0
		b.cameraHUD = nil
		b.glass = nil
		if err := b.commitLayersLocked(nil); err != nil {
			return err
		}
	}
	b.menuGPU = renderer
	return nil
}

func (b *Buffer) gpuMenuCommands(lines []string, l GlassLayout) ([]gpumenu.Command, error) {
	if cap(b.gpuCommandStorage) < 512 {
		b.gpuCommandStorage = make([]gpumenu.Command, 0, 512)
	}
	d := &gpuDrawList{commands: b.gpuCommandStorage[:0]}
	b.gpuTarget = d
	defer func() { b.gpuTarget = nil }()
	b.glassCard(l.Panel, 0, 0, 224, l.Panel)
	b.rasterGlassForeground(lines, l)
	if !l.Statistics && !l.PowerOnly {
		b.paintGlassStatus(l)
	}
	b.gpuNotice(b.menuStatus.Notice)
	b.gpuCommandStorage = d.commands
	return d.commands, d.err
}

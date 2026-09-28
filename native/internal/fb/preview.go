package fb

import (
	"fmt"
	"perimode/native/internal/decon"
	"perimode/native/internal/fimg2d"
	"perimode/native/internal/gpumenu"
	"image"
	"os"
	"slices"
	"time"

	"perimode/native/internal/media"
)

// PreviewOptions are placement preferences only. Capture is never enabled by
// loading this structure from storage. A local consumer must explicitly opt in.
type PreviewOptions struct {
	Corner       int  `json:"corner"`
	WidthPercent int  `json:"width_percent"`
	Fullscreen   bool `json:"fullscreen,omitempty"`
}

func DefaultPreviewOptions() PreviewOptions { return PreviewOptions{Corner: 3, WidthPercent: 33} }
func (o PreviewOptions) Validate() error {
	if o.Corner < 0 || o.Corner > 3 || (o.WidthPercent != 25 && o.WidthPercent != 33 && o.WidthPercent != 40) {
		return fmt.Errorf("invalid preview position/size")
	}
	return nil
}

type previewLayer struct {
	direct        media.Image
	view          decon.CameraView
	texture       gpumenu.PreviewTexture
	blank         bool
	nv12          []byte
	pixels        []uint32
	width, height int
	options       PreviewOptions
	backup        []savedPixel
	rect          image.Rectangle
	docked        bool
}

// PreviewLayout keeps the menu and preview in disjoint areas. Outside a menu,
// move from a conflicting indicator corner to the first available corner.
func PreviewLayout(w, h int, o PreviewOptions, menu bool, obstacles []image.Rectangle) image.Rectangle {
	if w < 160 || h < 160 || w > 4096 || h > 4096 || o.Validate() != nil {
		return image.Rectangle{}
	}
	if o.Fullscreen {
		return image.Rect(0, 0, w, h)
	}
	margin := max(8, min(w, h)/40)
	width := max(64, w*o.WidthPercent/100)
	width = min(width, w-2*margin, (h-2*margin)*16/9)
	width = max(32, width/32*32)
	height := width * 9 / 16
	for _, c := range []int{o.Corner, o.Corner ^ 1, o.Corner ^ 2, o.Corner ^ 3} {
		x, y := margin, margin
		if c&1 != 0 {
			x = w - margin - width
		}
		if c&2 != 0 {
			y = h - margin - height
		}
		rect := image.Rect(x, y, x+width, y+height)
		collision := false
		for _, v := range obstacles {
			if rect.Inset(-margin).Overlaps(v) {
				collision = true
				break
			}
		}
		if !collision {
			return rect
		}
	}
	return image.Rectangle{} // never cover menu/indicators to fake a viable layout
}
func LayoutGlassPreview(w, h int, lines []string, scroll int, on bool, o PreviewOptions) GlassLayout {
	// PiP does not rescale or reflow the monitor's OSD.
	return LayoutGlass(w, h, lines, scroll)
}
func (b *Buffer) MenuLayout(lines []string, scroll int) GlassLayout {
	b.mu.Lock()
	defer b.mu.Unlock()
	// A preview can appear/disappear asynchronously. Hit testing must use the
	// geometry most recently painted, not the next layout's larger/smaller body.
	if b.glass != nil && scroll == b.glass.layout.Scroll && slices.Equal(lines, b.glass.lines) {
		return b.glass.layout
	}
	if b.glass != nil {
		l := LayoutGlassPreview(b.Width, b.Height, lines, scroll, b.glass.previewDocked, DefaultPreviewOptions())
		applyGlassDetails(&l, b.menuStatus)
		return l
	}
	if b.preview == nil {
		l := LayoutGlass(b.Width, b.Height, lines, scroll)
		applyGlassDetails(&l, b.menuStatus)
		return l
	}
	l := LayoutGlassPreview(b.Width, b.Height, lines, scroll, true, b.preview.options)
	applyGlassDetails(&l, b.menuStatus)
	return l
}
func (b *Buffer) restorePreview() {
	if b.preview == nil {
		return
	}
	for _, p := range b.preview.backup {
		b.point(int(p.X), int(p.Y), p.Value)
	}
	b.preview.backup = b.preview.backup[:0]
	b.preview.rect = image.Rectangle{}
}
func (b *Buffer) previewObstacles() []image.Rectangle {
	if !b.indicatorsOn {
		return nil
	}
	var out []image.Rectangle
	for _, p := range IndicatorLayoutScaled(b.Width, b.Height, b.indicators, b.indicatorCorner, b.indicatorPercent()) {
		out = append(out, p.Rect)
	}
	return out
}
func (b *Buffer) drawPreview() {
	p := b.preview
	if p == nil {
		return
	}
	if b.s7BootVisual && b.scanout != nil {
		return
	} // Independent DECON layer.
	if b.s7BootVisual && b.scanout == nil {
		b.cpuRasterRejected = true
		return
	}
	r := PreviewLayout(b.Width, b.Height, p.options, b.glass != nil, b.previewObstacles())
	p.rect = r
	if r.Empty() {
		return
	}
	// Backup is only the small local overlay, not another full-screen snapshot.
	// A full monitor refresh discards this backup before writing new video pixels.
	p.backup = p.backup[:0]
	black := b.pixelRGB(0, 0, 0)
	border := max(1, min(b.Width, b.Height)/400)
	outer := r.Inset(-border).Intersect(image.Rect(0, 0, b.Width, b.Height))
	if p.blank && b.scanout != nil {
		for i, rect := range []image.Rectangle{outer, r} {
			physical, err := fimg2d.PhysicalRect(fimg2d.Rect{X: rect.Min.X, Y: rect.Min.Y, W: rect.Dx(), H: rect.Dy()}, b.Rotation)
			color := uint32(0xff8da9ad)
			if i == 1 {
				color = 0xff000000
			}
			if err == nil {
				err = b.scanout.FillRectangle(b.Data[:int(b.V.Y)*int(b.F.LineLength)], b.directFrameReady, physical, color)
			}
			if err != nil {
				b.hardwareGlassError = err.Error()
				return
			}
		}
		return
	}
	if b.scanout != nil {
		if b.menuGPU == nil {
			b.hardwareGlassError = "GPU preview renderer unavailable"
			return
		}
		physical, err := fimg2d.PhysicalRect(fimg2d.Rect{X: r.Min.X, Y: r.Min.Y, W: r.Dx(), H: r.Dy()}, b.Rotation)
		if err == nil {
			err = b.scanout.Preview(b.Data[:int(b.V.Y)*int(b.F.LineLength)], b.directFrameReady, p.texture.FD, p.texture.Width, p.texture.Height, p.texture.Bytes, physical)
		}
		if err != nil {
			b.hardwareGlassError = err.Error()
		}
		return
	}
	for y := outer.Min.Y; y < outer.Max.Y; y++ {
		for x := outer.Min.X; x < outer.Max.X; x++ {
			p.backup = append(p.backup, savedPixel{X: int16(x), Y: int16(y), Value: b.at(x, y)})
			v := black
			if !p.blank && image.Pt(x, y).In(r) {
				sx := (x - r.Min.X) * p.width / r.Dx()
				sy := (y - r.Min.Y) * p.height / r.Dy()
				v = p.pixels[sy*p.width+sx]
			}
			b.point(x, y, v)
		}
	}
}

// UpdatePreview copies the supplied NV12 thumbnail synchronously. Camera DMA
// buffers can be returned immediately after this call. No encoding is performed.
func (b *Buffer) UpdatePreview(im media.Image, o PreviewOptions) error {
	if b.s7BootVisual {
		return b.UpdateCameraPreview(im, o, 0, false, 100)
	}
	if e := o.Validate(); e != nil {
		return e
	}
	if im.Width != 320 || im.Height != 180 || im.StrideY < 320 || im.StrideUV < 320 || im.StrideY > 4096 || im.StrideUV > 4096 || im.PTS < 0 || len(im.Y) < 179*im.StrideY+320 || len(im.UV) < 89*im.StrideUV+320 {
		return fmt.Errorf("invalid bounded NV12 preview")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return os.ErrClosed
	}
	if e := b.Validate(); e != nil {
		return e
	}
	if len(b.Data) < int(b.F.MemoryLength) {
		return fmt.Errorf("preview framebuffer mapping missing")
	}
	if b.glass != nil && b.glass.sourceBacked {
		b.glass.stageReady = false
	}
	b.restoreFeedback()
	b.restoreIndicators()
	b.restorePreview()
	if b.preview == nil {
		b.preview = &previewLayer{pixels: make([]uint32, 320*180), width: 320, height: 180}
	}
	p := b.preview
	if len(p.pixels) != 320*180 {
		p.pixels = make([]uint32, 320*180)
		p.width = 320
		p.height = 180
	}
	p.blank = false
	p.options = o
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			off := (y/2)*im.StrideUV + (x &^ 1)
			r, g, bl := RGB(im.Y[y*im.StrideY+x], im.UV[off], im.UV[off+1])
			p.pixels[y*320+x] = b.pixelRGB(r, g, bl)
		}
	}
	// The menu reserves space for PiP before capture starts; the image is NOT
	// sampled into the blur snapshot. While the menu is open it stays sharp.
	b.drawPreview()
	b.drawIndicators()
	b.drawFeedback()
	return b.commit()
}

// Preparing a back texture never holds the monitor/framebuffer lock.
func (b *Buffer) UpdateCameraPreview(im media.Image, o PreviewOptions, rotation int, mirror bool, zoom int) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if !im.Lease.Live() {
		return fmt.Errorf("native Preview requires DMA; CPU upload disabled")
	}
	if o.Fullscreen {
		return b.updateFullscreenPreview(im, o, rotation, mirror, zoom)
	}
	b.previewMu.Lock()
	defer b.previewMu.Unlock()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return os.ErrClosed
	}
	if !b.s7BootVisual || b.scanout == nil || b.previewGPUFactory == nil {
		b.mu.Unlock()
		return fmt.Errorf("GPU Preview unavailable")
	}
	if b.previewGPU == nil {
		factory := b.previewGPUFactory
		b.mu.Unlock()
		r, err := factory()
		if err != nil {
			return err
		}
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			_ = r.Close()
			return os.ErrClosed
		}
		b.previewGPU = r
	}
	if b.preview == nil {
		b.preview = &previewLayer{blank: true, options: o}
	}
	p := b.preview
	revision := b.previewRevision
	panelRotation := b.Rotation
	renderer := b.previewGPU
	rect := PreviewLayout(b.Width, b.Height, o, b.glass != nil, b.previewObstacles())
	width, height := rect.Dx()&^1, rect.Dy()&^1
	_, _, _, activeTexture := b.scanout.LayerState()
	slot := 0
	if activeTexture.Enabled && !activeTexture.Blank && activeTexture.FD == renderer.PreviewFD(0) {
		slot = 1
	}
	// A pending, unpresented texture may be overwritten. Remove it from the
	// published state before unlocking, so scanout cannot acquire it mid-write.
	if p.texture.FD == renderer.PreviewFD(slot) {
		p.blank = !activeTexture.Enabled || activeTexture.Blank
		if !p.blank {
			activeSlot := 0
			if activeTexture.FD == renderer.PreviewFD(1) {
				activeSlot = 1
			}
			p.texture = gpumenu.PreviewTexture{FD: activeTexture.FD, Width: activeTexture.Width, Height: activeTexture.Height, Bytes: gpumenu.PreviewBytes, Slot: activeSlot}
		}
	}
	b.mu.Unlock()
	if width < 2 || height < 2 {
		return nil
	}
	texture, err := renderer.PreviewDMA(im.Lease, rotation, int(panelRotation), mirror, zoom, slot, width, height)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.preview != p || b.previewRevision != revision || b.Rotation != panelRotation {
		return nil
	}
	p.blank = false
	p.options = o
	p.texture = texture
	if time.Since(b.lastMonitorFrame) < 100*time.Millisecond {
		return nil
	}
	return b.commitLayersLocked(nil)
}
func (b *Buffer) ClearPreview() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.previewRevision++
	if b.closed {
		return os.ErrClosed
	}
	if b.glass != nil && b.glass.sourceBacked {
		b.glass.stageReady = false
	}
	b.restoreFeedback()
	b.restoreIndicators()
	b.restorePreview()
	old := b.preview
	b.preview = nil
	b.drawIndicators()
	b.drawFeedback()
	err := b.commit()
	if err == nil && old != nil {
		err = old.direct.Lease.Release()
	}
	return err
}

func (b *Buffer) PreviewPlaceholder(o PreviewOptions) error {
	if err := o.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return os.ErrClosed
	}
	if b.preview != nil && b.preview.blank && b.preview.options == o {
		return nil
	}
	b.previewRevision++
	if b.scanout != nil && !b.scanout.UsesLayers() {
		if err := b.scanout.RestoreCurrent(b.Data[:int(b.V.Y)*int(b.F.LineLength)]); err != nil {
			return err
		}
	} else {
		b.restorePreview()
	}
	old := b.preview
	b.preview = &previewLayer{blank: true, width: 320, height: 180, options: o, docked: b.glass != nil}
	if b.glass != nil {
		b.glass.stageReady = false
	}
	b.drawPreview()
	err := b.commit()
	if err == nil && old != nil {
		err = old.direct.Lease.Release()
	}
	return err
}

// Rear Preview owns a completed sensor DMA lease, not a panel-sized RGB texture.
// VPP performs crop, rotate, CSC and scaling during scanout. Menu stays on G1.
func (b *Buffer) updateFullscreenPreview(im media.Image, o PreviewOptions, rotation int, mirror bool, zoom int) error {
	b.previewMu.Lock()
	defer b.previewMu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return os.ErrClosed
	}
	if !b.s7BootVisual || b.scanout == nil {
		return fmt.Errorf("camera scanout unavailable")
	}
	if b.preview == nil || b.preview.options != o {
		return nil
	}
	if err := im.Lease.Retain(); err != nil {
		return err
	}
	p := b.preview
	old := p.direct
	p.direct, p.blank = im, false
	p.view = decon.CameraView{Rotation: rotation, PanelRotation: int(b.Rotation), Mirror: mirror, Zoom: zoom}
	err := b.commitLayersLocked(nil)
	// PresentCamera retains both possible scanouts on an uncertain fence.
	// Keep this owner's old pin too when that uncertainty must be quarantined.
	if err == nil {
		err = old.Lease.Release()
	}
	return err
}
func (b *Buffer) PreviewHit(x, y uint16) bool {
	g := b.inputGeometry()
	if g.preview.Empty() || x > 32767 || y > 32767 {
		return false
	}
	p := image.Pt(int(x)*(g.width-1)/32767, int(y)*(g.height-1)/32767)
	return p.In(g.preview)
}

func (b *Buffer) PreviewPoint(x, y uint16) (int, int, int, int, bool) {
	g := b.inputGeometry()
	if g.preview.Empty() || x > 32767 || y > 32767 {
		return 0, 0, 0, 0, false
	}
	p := image.Pt(int(x)*(g.width-1)/32767, int(y)*(g.height-1)/32767)
	if !p.In(g.preview) {
		return 0, 0, 0, 0, false
	}
	return p.X - g.preview.Min.X, p.Y - g.preview.Min.Y, g.preview.Dx(), g.preview.Dy(), true
}

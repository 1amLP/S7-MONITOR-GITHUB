package fb

import (
	"encoding/binary"
	"image"
	"time"
	"unsafe"

	"perimode/native/internal/media"
)

// Limit only open-menu background work, never the ordinary 30/60-FPS monitor.
// At least one composition-duration of idle time prevents an unbounded render
// backlog on a slow CPU. No additional goroutine or DMA reference is retained.
const liveGlassMinimumGap = time.Second / 15
const liveNV12Bytes = 1280 * 720 * 3 / 2

// Observations, not physical scanout/FPS claims. All counters are under Buffer.mu.
type LiveGlassStats struct {
	Backend          string `json:"backend"`
	LastError        string `json:"hardware_error,omitempty"`
	Offered          uint64 `json:"frames_offered_while_menu_open"`
	Committed        uint64 `json:"live_background_commits"`
	Coalesced        uint64 `json:"frames_replaced_by_pacing"`
	PendingFlushes   uint64 `json:"pending_background_flushes"`
	CloseRefreshes   uint64 `json:"pending_presented_on_close"`
	ForegroundBuilds uint64 `json:"foreground_cache_builds"`
	LastComposeUS    int64  `json:"last_compose_us"`
	BlurUS           int64  `json:"hardware_blur_us"`
	BlendUS          int64  `json:"hardware_blend_us"`
	MaxComposeUS     int64  `json:"max_compose_us"`
	ResidentBytes    int    `json:"owned_menu_bytes"`
	Pending          bool   `json:"latest_frame_pending"`
	Active           bool   `json:"menu_open"`
}

func (b *Buffer) LiveGlassStatus() LiveGlassStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.liveStats
	v.LastError = b.hardwareGlassError
	if b.glass != nil && b.scanout != nil && b.scanout.UsesLayers() {
		v.Backend = "Mali raster / independent DECON layer"
	} else if b.menuComposer != nil {
		v.Backend = "Mali raster / FIMG2D compose"
	} else if b.glass == nil {
		v.Backend = "OFF"
	} else {
		v.Backend = "GPU unavailable"
	}
	if sc := b.glass; sc != nil {
		v.Active, v.Pending = true, sc.hasPending
		v.ResidentBytes = len(sc.raw) + len(sc.cachedRaw) + cap(sc.pending) + cap(sc.backdrop) +
			4*(len(sc.sourceRGB)+len(sc.rgb)+len(sc.blurTemp)+len(sc.blurOutput)+len(sc.panelPacked)) + len(sc.coverage) + len(sc.edge) +
			int(unsafe.Sizeof(glassSample{}))*(cap(sc.xSamples)+cap(sc.ySamples)) + cap(b.menuStage)
		if sc.ink != nil {
			v.ResidentBytes += 4 * len(sc.ink.pixels)
		}
	}
	return v
}

func storeGlassImage(dst *[]byte, im media.Image) {
	if len(*dst) != liveNV12Bytes {
		*dst = make([]byte, liveNV12Bytes)
	}
	for y := 0; y < 720; y++ {
		copy((*dst)[y*1280:(y+1)*1280], im.Y[y*im.StrideY:y*im.StrideY+1280])
	}
	for y := 0; y < 360; y++ {
		copy((*dst)[1280*720+y*1280:1280*720+(y+1)*1280], im.UV[y*im.StrideUV:y*im.StrideUV+1280])
	}
}

func (sc *glassScene) storePending(im media.Image) {
	storeGlassImage(&sc.pending, im)
	sc.pendingPTS, sc.hasPending = im.PTS, true
}
func (sc *glassScene) pendingImage() media.Image {
	return media.Image{Width: 1280, Height: 720, StrideY: 1280, StrideUV: 1280,
		Y: sc.pending[:1280*720], UV: sc.pending[1280*720:], PTS: sc.pendingPTS}
}
func (sc *glassScene) storeBackdrop(im media.Image) {
	storeGlassImage(&sc.backdrop, im)
	sc.backdropPTS, sc.hasBackdrop = im.PTS, true
}
func (sc *glassScene) backdropImage() media.Image {
	return media.Image{Width: 1280, Height: 720, StrideY: 1280, StrideUV: 1280,
		Y: sc.backdrop[:1280*720], UV: sc.backdrop[1280*720:], PTS: sc.backdropPTS}
}

func (b *Buffer) sampleImageBackdrop(sc *glassScene, im media.Image) {
	for y := 0; y < sc.height; y++ {
		sy := y * 720 / sc.height
		for x := 0; x < sc.width; x++ {
			sx := x * 1280 / sc.width
			uvx := sx &^ 1
			r, g, bl := RGB(im.Y[sy*im.StrideY+sx], im.UV[(sy/2)*im.StrideUV+uvx], im.UV[(sy/2)*im.StrideUV+uvx+1])
			sc.sourceRGB[y*sc.width+x] = uint32(r)<<16 | uint32(g)<<8 | uint32(bl)
		}
	}
	copy(sc.rgb, sc.sourceRGB)
	for i := 0; i < 3; i++ {
		blurBoxInto(sc.rgb, sc.blurTemp, sc.blurOutput, sc.width, sc.height, 5)
		sc.rgb, sc.blurOutput = sc.blurOutput, sc.rgb
	}
	for i, p := range sc.rgb {
		sc.rgb[i] = champagneTint(p)
	}
	sc.cachedPanel = image.Rectangle{}
}
func (b *Buffer) sampleGlassBackdrop(sc *glassScene) {
	for y := 0; y < sc.height; y++ {
		for x := 0; x < sc.width; x++ {
			r, g, bl := unpackRGB(b.at(x*b.Width/sc.width, y*b.Height/sc.height), b.V)
			sc.rgb[y*sc.width+x] = uint32(r)<<16 | uint32(g)<<8 | uint32(bl)
		}
	}
	for i := 0; i < 3; i++ {
		blurBoxInto(sc.rgb, sc.blurTemp, sc.blurOutput, sc.width, sc.height, 5)
		sc.rgb, sc.blurOutput = sc.blurOutput, sc.rgb
	}
	for i, p := range sc.rgb {
		sc.rgb[i] = champagneTint(p)
	}
}
func (b *Buffer) updateGlassSource(sc *glassScene) {
	// Called BEFORE PiP, text, notifications or indicators are composited.
	// No successive blur of already blurred/glass pixels.
	copy(sc.raw, b.Data)
	b.sampleGlassBackdrop(sc)
	sc.cachedPanel = image.Rectangle{}
}

// Premultiplied RGBA for static sharp UI. Fonts, switches and cards are rasterized
// only when labels/layout/scroll change, not for each video frame.
type glassInk struct {
	rect   image.Rectangle
	pixels []uint32
}

func newGlassInk(r image.Rectangle) *glassInk {
	return &glassInk{rect: r, pixels: make([]uint32, r.Dx()*r.Dy())}
}
func (g *glassInk) blend(x, y int, rgb uint32, a int) {
	if !image.Pt(x, y).In(g.rect) {
		return
	}
	i := (y-g.rect.Min.Y)*g.rect.Dx() + x - g.rect.Min.X
	old := g.pixels[i]
	inv := 255 - a
	aa := a + (int(old>>24)*inv+127)/255
	rr := (int(rgb>>16&255)*a + int(old>>16&255)*inv + 127) / 255
	gg := (int(rgb>>8&255)*a + int(old>>8&255)*inv + 127) / 255
	bb := (int(rgb&255)*a + int(old&255)*inv + 127) / 255
	g.pixels[i] = uint32(aa)<<24 | uint32(rr)<<16 | uint32(gg)<<8 | uint32(bb)
}
func (b *Buffer) paintGlassInk(g *glassInk) {
	if g == nil {
		return
	}
	for y := g.rect.Min.Y; y < g.rect.Max.Y; y++ {
		for x := g.rect.Min.X; x < g.rect.Max.X; x++ {
			p := g.pixels[(y-g.rect.Min.Y)*g.rect.Dx()+x-g.rect.Min.X]
			a := int(p >> 24)
			if a == 0 {
				continue
			}
			r, gg, bl := unpackRGB(b.at(x, y), b.V)
			b.point(x, y, b.pixelRGB(byte(min(255, int(p>>16&255)+(int(r)*(255-a)+127)/255)),
				byte(min(255, int(p>>8&255)+(int(gg)*(255-a)+127)/255)),
				byte(min(255, int(p&255)+(int(bl)*(255-a)+127)/255))))
		}
	}
}
func (b *Buffer) paintGlassBackdrop(sc *glassScene, l GlassLayout) {
	b.paintGlassBackdropRegion(sc, l, l.Panel, true)
}

func (b *Buffer) paintGlassBackdropRegion(sc *glassScene, l GlassLayout, dirty image.Rectangle, recompose bool) {
	panel := l.Panel.Intersect(image.Rect(0, 0, b.Width, b.Height))
	region := dirty.Intersect(panel)
	if region.Empty() {
		return
	}
	if sc.sourceBacked {
		if sc.cachedPanel != panel || len(sc.panelPacked) != panel.Dx()*panel.Dy() {
			region, recompose = panel, true
		}
		if !recompose {
			b.writePackedGlassPanelRegion(panel, sc.panelPacked, region)
			return
		}
	} else if sc.cachedPanel == panel && len(sc.cachedRaw) == len(b.Data) {
		copy(b.Data, sc.cachedRaw)
		return
	}
	if sc.coveragePanel != panel {
		sc.coveragePanel = panel
		sc.coverage = make([]byte, panel.Dx()*panel.Dy())
		sc.edge = make([]byte, len(sc.coverage))
		radius := float64(l.Font * 5 / 4)
		for y := panel.Min.Y; y < panel.Max.Y; y++ {
			for x := panel.Min.X; x < panel.Max.X; x++ {
				i := (y-panel.Min.Y)*panel.Dx() + x - panel.Min.X
				cov := roundCoverage(panel, x, y, radius)
				sc.coverage[i] = byte(cov)
				sc.edge[i] = byte(max(0, cov-roundCoverage(panel.Inset(max(1, l.Scale)), x, y, max(1, radius-float64(l.Scale)))) * 105 / 255)
			}
		}
	}
	// Compute resampling coordinates outside the pixel loop. Edges are cached;
	// neither divisions nor square roots belong in the per-video-pixel path.
	if len(sc.xSamples) != panel.Dx() || len(sc.ySamples) != panel.Dy() {
		sc.xSamples = make([]glassSample, panel.Dx())
		sc.ySamples = make([]glassSample, panel.Dy())
	}
	for x := range sc.xSamples {
		n := (x + panel.Min.X) * (sc.width - 1) * 256 / max(1, b.Width-1)
		sc.xSamples[x] = glassSample{a: n / 256, b: min(n/256+1, sc.width-1), fraction: uint32(n % 256)}
	}
	for y := range sc.ySamples {
		n := (y + panel.Min.Y) * (sc.height - 1) * 256 / max(1, b.Height-1)
		sc.ySamples[y] = glassSample{a: (n / 256) * sc.width, b: min(n/256+1, sc.height-1) * sc.width, fraction: uint32(n % 256)}
	}
	b.prepareCPU()
	if sc.sourceBacked {
		if len(sc.panelPacked) != panel.Dx()*panel.Dy() {
			sc.panelPacked = make([]uint32, panel.Dx()*panel.Dy())
		}
	}
	bytespp := int(b.V.Bits / 8)
	step := bytespp
	switch b.Rotation {
	case 90:
		step = int(b.F.LineLength)
	case 180:
		step = -bytespp
	case 270:
		step = -int(b.F.LineLength)
	}
	for y := region.Min.Y; y < region.Max.Y; y++ {
		yy := y - panel.Min.Y
		sy := sc.ySamples[yy]
		px, py := b.Rotation.ToPanel(region.Min.X, y, int(b.V.X), int(b.V.Y))
		off := (py+int(b.V.YOffset))*int(b.F.LineLength) + (px+int(b.V.XOffset))*bytespp
		for x := region.Min.X; x < region.Max.X; x++ {
			xx := x - panel.Min.X
			sx := sc.xSamples[xx]
			i := yy*panel.Dx() + xx
			ink := sc.ink.pixels[i]
			var rgb uint32
			if ink>>24 == 255 {
				rgb = ink & 0xffffff
			} else {
				top := lerpGlass(sc.rgb[sy.a+sx.a], sc.rgb[sy.a+sx.b], sx.fraction)
				bottom := lerpGlass(sc.rgb[sy.b+sx.a], sc.rgb[sy.b+sx.b], sx.fraction)
				rgb = lerpGlass(top, bottom, sy.fraction)
				if cov := int(sc.coverage[i]); cov < 255 {
					var rawRGB uint32
					if sc.sourceBacked {
						rawRGB = sampleGlassPixels(sc.sourceRGB, sc.width, sc.height, x, y, b.Width, b.Height)
					} else {
						var raw uint32
						if bytespp == 4 {
							raw = binary.LittleEndian.Uint32(b.Data[off:])
						} else {
							raw = uint32(binary.LittleEndian.Uint16(b.Data[off:]))
						}
						r, g, bl := unpackRGB(raw, b.V)
						rawRGB = uint32(r)<<16 | uint32(g)<<8 | uint32(bl)
					}
					rgb = lerpGlass(rawRGB, rgb, uint32(cov+cov/128))
				}
				if edge := int(sc.edge[i]); edge > 0 {
					rgb = lerpGlass(rgb, 0xfff4df, uint32(edge+edge/128))
				}
				if a := int(ink >> 24); a > 0 {
					inv := 255 - a
					r := min(255, int(ink>>16&255)+(int(rgb>>16&255)*inv+127)/255)
					g := min(255, int(ink>>8&255)+(int(rgb>>8&255)*inv+127)/255)
					bl := min(255, int(ink&255)+(int(rgb&255)*inv+127)/255)
					rgb = uint32(r)<<16 | uint32(g)<<8 | uint32(bl)
				}
			}
			packed := b.redPacked[rgb>>16&255] | b.greenPacked[rgb>>8&255] | b.bluePacked[rgb&255]
			if sc.sourceBacked {
				sc.panelPacked[i] = packed
			}
			if bytespp == 4 {
				binary.LittleEndian.PutUint32(b.Data[off:], packed)
			} else {
				binary.LittleEndian.PutUint16(b.Data[off:], uint16(packed))
			}
			off += step
		}
	}

	if sc.sourceBacked && region == panel {
		sc.cachedPanel = panel
	} else if !sc.sourceBacked && len(b.Data) <= 32<<20 {
		sc.cachedRaw = append(sc.cachedRaw[:0], b.Data...)
		sc.cachedPanel = l.Panel
	}
}

func (b *Buffer) writePackedGlassPanel(panel image.Rectangle, pixels []uint32) {
	b.writePackedGlassPanelRegion(panel, pixels, panel)
}

func (b *Buffer) writePackedGlassPanelRegion(panel image.Rectangle, pixels []uint32, dirty image.Rectangle) {
	if len(pixels) != panel.Dx()*panel.Dy() {
		return
	}
	region := dirty.Intersect(panel)
	if region.Empty() {
		return
	}
	bytespp := int(b.V.Bits / 8)
	step := bytespp
	switch b.Rotation {
	case 90:
		step = int(b.F.LineLength)
	case 180:
		step = -bytespp
	case 270:
		step = -int(b.F.LineLength)
	}
	for y := region.Min.Y; y < region.Max.Y; y++ {
		yy := y - panel.Min.Y
		px, py := b.Rotation.ToPanel(region.Min.X, y, int(b.V.X), int(b.V.Y))
		off := (py+int(b.V.YOffset))*int(b.F.LineLength) + (px+int(b.V.XOffset))*bytespp
		for x := region.Min.X; x < region.Max.X; x++ {
			xx := x - panel.Min.X
			packed := pixels[yy*panel.Dx()+xx]
			if bytespp == 4 {
				binary.LittleEndian.PutUint32(b.Data[off:], packed)
			} else {
				binary.LittleEndian.PutUint16(b.Data[off:], uint16(packed))
			}
			off += step
		}
	}
}

// HasGlass distinguishes a painted menu from the short rotation/startup gap.
func (b *Buffer) HasGlass() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.glass != nil
}

// Stream boundaries invalidate only the unsent pending image. Already displayed
// pixels may remain as a still background, never as proof of a new connection.
func (b *Buffer) SetVideoGeneration(gen uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.videoGeneration != gen {
		b.videoGeneration = gen
		if sc := b.glass; sc != nil {
			sc.hasPending = false
			sc.nextLive = time.Time{}
		}
	}
}

type glassSample struct {
	a, b     int
	fraction uint32
}

// Independent 16-bit lanes for red/blue, green in its own lane. Weights sum
// to 256, so neither channel spills into its neighbour (including rounding).
func lerpGlass(a, b, f uint32) uint32 {
	inv := 256 - f
	rb := (((a&0xff00ff)*inv + (b&0xff00ff)*f + 0x800080) >> 8) & 0xff00ff
	g := (((a&0x00ff00)*inv + (b&0x00ff00)*f + 0x008000) >> 8) & 0x00ff00
	return rb | g
}

package fb

import (
	"fmt"
	"image"
	"math"
	"os"
	"unicode/utf8"
)

// LinkFeedback is a bounded, transient notification; it is not a menu or a
// second video source. Progress is supplied by the native UI's monotonic clock.
type LinkFeedback struct {
	Visible              bool
	Phase, Title, Detail string
	Progress             float64
}
type feedbackRegion struct {
	rect   image.Rectangle
	pixels []uint32
}
type feedbackLayer struct {
	view                                LinkFeedback
	card, wave                          feedbackRegion
	cacheRect                           image.Rectangle
	cacheTitle, cacheDetail, cachePhase string
	cacheRotation                       int
	rgb, packed                         []uint32
	alpha                               []uint8
}
type FeedbackLayout struct {
	Card, Wave       image.Rectangle
	Font, Small, Pad int
	Title, Detail    []string
}

func LayoutLinkFeedback(w, h int, title, detail string) FeedbackLayout {
	if w < 320 || h < 320 || w > 4096 || h > 4096 {
		return FeedbackLayout{}
	}
	f := fontSize(max(16, min(36, min(w, h)/36)))
	small := fontSize(f * 4 / 5)
	pad := max(12, f)
	margin := max(12, min(w, h)/40)
	width := min(w-2*margin, min(900, max(360, w*3/5)))
	textW := width - 2*pad - 3*f
	ts := wrapText(title, textW, f)
	ds := wrapText(detail, textW, small)
	if len(ts) > 2 {
		ts = append(ts[:1], ts[1]+"...")
	}
	if len(ds) > 2 {
		ds = append(ds[:1], ds[1]+"...")
	}
	height := 2*pad + len(ts)*f*5/4 + len(ds)*small*5/4 + f/4
	band := min(200, max(48, h/8))
	bottom := h - band - margin
	card := image.Rect((w-width)/2, bottom-height, (w+width)/2, bottom)
	return FeedbackLayout{Card: card, Wave: image.Rect(0, h-band, w, h),
		Font: f, Small: small, Pad: pad, Title: ts, Detail: ds}
}
func feedbackValid(v LinkFeedback) bool {
	if !v.Visible {
		return true
	}
	if math.IsNaN(v.Progress) || math.IsInf(v.Progress, 0) || v.Progress < 0 || v.Progress > 1 ||
		len(v.Title) > 160 || len(v.Detail) > 200 || !utf8.ValidString(v.Title) || !utf8.ValidString(v.Detail) {
		return false
	}
	switch v.Phase {
	case "connecting", "disconnecting", "unavailable", "disconnected", "waiting_usb",
		"checking_driver", "no_reply", "ready", "lost", "error", "driver_error", "thermal", "suspended":
		return true
	}
	return false
}
func (r *feedbackRegion) capture(b *Buffer, rect image.Rectangle) {
	r.rect = rect.Intersect(image.Rect(0, 0, b.Width, b.Height))
	n := r.rect.Dx() * r.rect.Dy()
	if cap(r.pixels) < n {
		r.pixels = make([]uint32, n)
	} else {
		r.pixels = r.pixels[:n]
	}
	i := 0
	for y := r.rect.Min.Y; y < r.rect.Max.Y; y++ {
		for x := r.rect.Min.X; x < r.rect.Max.X; x++ {
			r.pixels[i] = b.at(x, y)
			i++
		}
	}
}
func (r *feedbackRegion) restore(b *Buffer) {
	i := 0
	if len(r.pixels) != r.rect.Dx()*r.rect.Dy() {
		r.pixels = r.pixels[:0]
		return
	}
	for y := r.rect.Min.Y; y < r.rect.Max.Y; y++ {
		for x := r.rect.Min.X; x < r.rect.Max.X; x++ {
			b.point(x, y, r.pixels[i])
			i++
		}
	}
	r.pixels = r.pixels[:0]
	r.rect = image.Rectangle{}
}
func (b *Buffer) restoreFeedback() {
	if b.feedback == nil {
		return
	}
	b.feedback.card.restore(b)
	b.feedback.wave.restore(b)
}
func (b *Buffer) discardFeedback() {
	if b.feedback == nil {
		return
	}
	b.feedback.card.pixels = b.feedback.card.pixels[:0]
	b.feedback.wave.pixels = b.feedback.wave.pixels[:0]
	b.feedback.card.rect = image.Rectangle{}
	b.feedback.wave.rect = image.Rectangle{}
}

// Every framebuffer writer restores or discards this top layer BEFORE editing
// its underlay and composites it last. No trails, even when video has stopped.
func (b *Buffer) LinkFeedback(v LinkFeedback) error {
	if b.s7BootVisual {
		return fmt.Errorf("native link status belongs to the Mali menu header")
	}
	if !feedbackValid(v) {
		return fmt.Errorf("invalid link feedback")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return os.ErrClosed
	}
	if err := b.Validate(); err != nil {
		return err
	}
	if b.glass != nil && b.glass.sourceBacked {
		b.glass.stageReady = false
	}
	w, h := b.Rotation.Size(int(b.V.X), int(b.V.Y))
	if !b.Rotation.Valid() || w != b.Width || h != b.Height || len(b.Data) < int(b.F.MemoryLength) {
		return fmt.Errorf("invalid feedback framebuffer mapping")
	}
	b.restoreFeedback()
	if !v.Visible {
		b.feedback = nil
		return b.commit()
	}
	if b.feedback == nil {
		b.feedback = &feedbackLayer{}
	}
	b.feedback.view = v
	b.drawFeedback()
	return b.commit()
}

func easeUnit(v float64) float64 { v = math.Max(0, math.Min(1, v)); return v * v * (3 - 2*v) }
func feedbackColor(phase string) uint32 {
	switch phase {
	case "error", "driver_error", "lost", "thermal", "no_reply":
		return 0xffbd8c
	case "disconnected", "disconnecting", "suspended":
		return 0xd5ccc0
	default:
		return 0xffdfaa
	}
}
func (b *Buffer) drawFeedback() {
	if b.s7BootVisual {
		return
	}
	f := b.feedback
	if f == nil || !f.view.Visible {
		return
	}
	p := f.view.Progress
	opacity := easeUnit(p/.12) * easeUnit((1-p)/.25)
	if opacity <= 0 {
		return
	}
	l := LayoutLinkFeedback(b.Width, b.Height, f.view.Title, f.view.Detail)
	if l.Card.Empty() {
		return
	}
	// Camera pixels stay sharp. The message also moves above a bottom PiP.
	exclusion := image.Rectangle{}
	if b.preview != nil {
		exclusion = b.preview.rect.Inset(-2).Intersect(image.Rect(0, 0, b.Width, b.Height))
		if !exclusion.Empty() && l.Card.Overlaps(exclusion) {
			dy := exclusion.Min.Y - l.Pad - l.Card.Max.Y
			if l.Card.Min.Y+dy >= l.Pad {
				l.Card = l.Card.Add(image.Pt(0, dy))
			}
		}
	}
	f.wave.capture(b, l.Wave)
	f.card.capture(b, l.Card)
	tint := feedbackColor(f.view.Phase)
	direction := 1.0
	if f.view.Phase == "disconnected" || f.view.Phase == "disconnecting" {
		direction = -1
	}
	travel := 1 - math.Pow(1-p, 3)
	if direction < 0 {
		travel = 1 - travel
	}
	band := float64(l.Wave.Dy())
	glow := max(4, l.Font/3)
	coreWidth := math.Max(1, float64(l.Font)/14)
	for x := l.Wave.Min.X; x < l.Wave.Max.X; x++ {
		nx := float64(x) / float64(b.Width)
		envelope := math.Pow(math.Sin(math.Pi*nx), .7)
		var centers, strengths [3]float64
		low, high := float64(b.Height), float64(l.Wave.Min.Y)
		for layer := 0; layer < 3; layer++ {
			centers[layer] = float64(b.Height) - 12 - travel*band*.62 + float64(layer)*float64(l.Font)/2 +
				envelope*float64(l.Font)*math.Sin(2*math.Pi*nx+direction*p*5+float64(layer)*.7)
			strengths[layer] = opacity * envelope * (1 - float64(layer)*.23)
			low = math.Min(low, centers[layer])
			high = math.Max(high, centers[layer])
		}
		for y := max(l.Wave.Min.Y, int(low)-glow*3); y < min(l.Wave.Max.Y, int(high)+glow*3+1); y++ {
			if image.Pt(x, y).In(exclusion) {
				continue
			}
			alpha := 0.0
			for layer := 0; layer < 3; layer++ {
				d := math.Abs(float64(y) + .5 - centers[layer])
				core := math.Max(0, 1-d/coreWidth)
				halo := math.Max(0, 1-d/float64(glow*3))
				alpha += strengths[layer] * (210*core + 32*halo*halo)
			}
			b.blendPoint(x, y, tint, min(245, int(alpha)))
		}
	}
	// Snapshot the local glass only once per notification/geometry, like the
	// existing native menu. Every frame still backs up its CURRENT video pixels.
	if f.cacheRect == l.Card && f.cacheTitle == f.view.Title && f.cacheDetail == f.view.Detail &&
		f.cachePhase == f.view.Phase && f.cacheRotation == int(b.Rotation) {
		b.paintFeedbackCache(l, opacity)
		return
	}
	// Blur just the message card's actual underlay, not a fake solid rectangle.
	sw := min(220, l.Card.Dx())
	sh := max(1, l.Card.Dy()*sw/l.Card.Dx())
	sc := &glassScene{width: sw, height: sh, rgb: make([]uint32, sw*sh)}
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			sx, sy := x*l.Card.Dx()/sw, y*l.Card.Dy()/sh
			r, g, bl := unpackRGB(f.card.pixels[sy*l.Card.Dx()+sx], b.V)
			sc.rgb[y*sw+x] = uint32(r)<<16 | uint32(g)<<8 | uint32(bl)
		}
	}
	for i := 0; i < 3; i++ {
		sc.rgb = blurBox(sc.rgb, sw, sh, 3)
	}
	radius := float64(l.Font)
	inner := l.Card.Inset(max(1, l.Font/18))
	for y := l.Card.Min.Y; y < l.Card.Max.Y; y++ {
		for x := l.Card.Min.X; x < l.Card.Max.X; x++ {
			if image.Pt(x, y).In(exclusion) {
				continue
			}
			cov := roundCoverage(l.Card, x, y, radius)
			if cov == 0 {
				continue
			}
			rgb := champagneTint(sampleBackdrop(sc, x-l.Card.Min.X, y-l.Card.Min.Y, l.Card.Dx(), l.Card.Dy()))
			b.blendPoint(x, y, rgb, cov)
			edge := max(0, cov-roundCoverage(inner, x, y, radius-1))
			b.blendPoint(x, y, 0xfff2db, edge*100/255)
		}
	}
	cx := float64(l.Card.Min.X + l.Pad + l.Font)
	cy := float64((l.Card.Min.Y + l.Card.Max.Y) / 2)
	rad := float64(l.Font)
	// Halo + clean ring, with a quiet travelling highlight.
	for y := int(cy - rad - 3); y <= int(cy+rad+3); y++ {
		for x := int(cx - rad - 3); x <= int(cx+rad+3); x++ {
			d := math.Abs(math.Hypot(float64(x)+.5-cx, float64(y)+.5-cy) - rad)
			a := math.Max(0, 1-d/1.4)*155 + math.Max(0, 1-d/3)*22
			b.blendPoint(x, y, tint, int(a))
		}
	}
	// Monitor pictogram, no external assets or bitmap text.
	unit := float64(l.Font) / 20
	stroke := func(x1, y1, x2, y2 float64) {
		x1, y1, x2, y2 = cx+x1*unit, cy+y1*unit, cx+x2*unit, cy+y2*unit
		for y := int(math.Min(y1, y2) - 2); y <= int(math.Max(y1, y2)+2); y++ {
			for x := int(math.Min(x1, x2) - 2); x <= int(math.Max(x1, x2)+2); x++ {
				dx, dy := x2-x1, y2-y1
				q := math.Max(0, math.Min(1, ((float64(x)+.5-x1)*dx+(float64(y)+.5-y1)*dy)/(dx*dx+dy*dy)))
				d := math.Hypot(float64(x)+.5-x1-q*dx, float64(y)+.5-y1-q*dy)
				b.blendPoint(x, y, 0xfff6e6, int(math.Max(0, 1.4*unit-d)*230))
			}
		}
	}
	stroke(-10, -7, 10, -7)
	stroke(10, -7, 10, 6)
	stroke(10, 6, -10, 6)
	stroke(-10, 6, -10, -7)
	stroke(0, 6, 0, 11)
	stroke(-5, 11, 5, 11)
	tx := l.Card.Min.X + l.Pad + 3*l.Font
	ty := l.Card.Min.Y + l.Pad
	for _, text := range l.Title {
		b.smoothText(tx, ty, text, l.Font, 0xfffbf3, l.Card)
		ty += l.Font * 5 / 4
	}
	ty += l.Font / 4
	for _, text := range l.Detail {
		b.smoothText(tx, ty, text, l.Small, 0xeee0c8, l.Card)
		ty += l.Small * 5 / 4
	}
	b.cacheFeedbackCard(l)
	b.paintFeedbackCache(l, opacity)
}

// Cache the opaque tinted snapshot + separate antialiased coverage. Old video
// pixels at rounded corners are NOT cached as opaque parts of the notification.
func (b *Buffer) cacheFeedbackCard(l FeedbackLayout) {
	f := b.feedback
	n := l.Card.Dx() * l.Card.Dy()
	f.rgb = make([]uint32, n)
	f.packed = make([]uint32, n)
	f.alpha = make([]uint8, n)
	inner := l.Card.Inset(max(1, l.Font/18))
	radius := float64(l.Font)
	i := 0
	for y := l.Card.Min.Y; y < l.Card.Max.Y; y++ {
		for x := l.Card.Min.X; x < l.Card.Max.X; x++ {
			cov := roundCoverage(l.Card, x, y, radius)
			edge := max(0, cov-roundCoverage(inner, x, y, radius-1))
			border := edge * 100 / 255
			a := 255 - (255-cov)*(255-border)/255
			f.alpha[i] = uint8(a)
			if a > 0 {
				rr, gg, bb := unpackRGB(b.at(x, y), b.V)
				br, bg, bbg := unpackRGB(f.card.pixels[i], b.V)
				recover := func(v, base byte) byte { return clamp((int(v)*255 - int(base)*(255-a) + a/2) / a) }
				r, g, bl := recover(rr, br), recover(gg, bg), recover(bb, bbg)
				f.rgb[i] = uint32(r)<<16 | uint32(g)<<8 | uint32(bl)
				f.packed[i] = b.pixelRGB(r, g, bl)
			}
			i++
		}
	}
	f.cacheRect = l.Card
	f.cacheTitle = f.view.Title
	f.cacheDetail = f.view.Detail
	f.cachePhase = f.view.Phase
	f.cacheRotation = int(b.Rotation)
}

func (b *Buffer) paintFeedbackCache(l FeedbackLayout, opacity float64) {
	f := b.feedback
	fade := int(opacity * 255)
	i := 0
	for y := l.Card.Min.Y; y < l.Card.Max.Y; y++ {
		for x := l.Card.Min.X; x < l.Card.Max.X; x++ {
			a := int(f.alpha[i]) * fade / 255
			pixel := f.card.pixels[i]
			if a == 255 {
				pixel = f.packed[i]
			} else if a > 0 {
				r, g, bl := unpackRGB(pixel, b.V)
				rgb := f.rgb[i]
				pixel = b.pixelRGB(
					byte((int(r)*(255-a)+int(rgb>>16&255)*a+127)/255),
					byte((int(g)*(255-a)+int(rgb>>8&255)*a+127)/255),
					byte((int(bl)*(255-a)+int(rgb&255)*a+127)/255))
			}
			b.point(x, y, pixel)
			i++
		}
	}
}

// Structural underlay changes (menu open/close/live menu refresh) invalidate the
// small message-card snapshot. Animation ticks alone keep using its cached ink.
func (b *Buffer) invalidateFeedbackBackdrop() {
	if b.feedback != nil {
		b.feedback.cacheRect = image.Rectangle{}
	}
}

package fb

import (
	"fmt"
	"image"
)

type CameraHUD struct {
	WideSlider                  bool
	Notice                      string
	Titles, Values              [16]string
	Steppers                    [16]bool
	Count, Selected             int
	Options                     [8]string
	OptionCount, OptionSelected int
	Slider                      bool
	Value, Min, Max             int
	Focus                       bool
	FocusX, FocusY              uint16
}

type CameraHUDGeometry struct {
	Fields               [16]image.Rectangle
	Reset                [16]image.Rectangle
	Minus, Plus          [16]image.Rectangle
	Values               [8]image.Rectangle
	Panel, Title, Slider image.Rectangle
}

func LayoutCameraHUD(w, h int, v CameraHUD) CameraHUDGeometry {
	var g CameraHUDGeometry
	if v.WideSlider && w >= 160 && h >= 160 && v.Count > 0 && v.Count <= len(g.Fields) {
		margin, gap := min(16, w/40), 8
		columns := min(v.Count, max(1, (w-2*margin+gap)/260))
		rows := (v.Count + columns - 1) / columns
		sliderHeight := min(80, h/5)
		height := min(124, (h-sliderHeight-2*margin-gap*(rows+1))/rows)
		cell := (w - 2*margin + gap) / columns
		g.Slider = image.Rect(margin, h-margin-sliderHeight, w-margin, h-margin)
		top := g.Slider.Min.Y - gap - rows*(height+gap)
		for i := 0; i < v.Count && i < len(g.Fields); i++ {
			x, y := margin+(i%columns)*cell, top+(i/columns)*(height+gap)
			g.Fields[i] = image.Rect(x, y, x+cell-gap, y+height)
			if i == 1 || i == 2 || v.Steppers[i] {
				r := g.Fields[i]
				split := r.Max.X - r.Dx()/3
				g.Reset[i] = image.Rect(split+4, r.Min.Y, r.Max.X, r.Max.Y)
				g.Fields[i].Max.X = split - 4
			}
			if v.Steppers[i] {
				r := g.Fields[i]
				side := min(56, r.Dx()/4)
				g.Minus[i] = image.Rect(r.Min.X, r.Min.Y, r.Min.X+side, r.Max.Y)
				g.Plus[i] = image.Rect(r.Max.X-side, r.Min.Y, r.Max.X, r.Max.Y)
				g.Fields[i] = image.Rect(r.Min.X+side+4, r.Min.Y, r.Max.X-side-4, r.Max.Y)
			}
		}
		return g
	}
	if w < 160 || h < 160 || v.Count < 1 || v.Count > len(v.Titles) {
		return g
	}
	margin, gap := max(12, min(w, h)/40), 12
	columns := min(v.Count, min(8, max(2, (w-2*margin+gap)/180)))
	rows := (v.Count + columns - 1) / columns
	height := min(132, max(64, h/(rows+4)))
	cell := (w - 2*margin - gap*(columns-1)) / columns
	controlsTop := h - margin - rows*height - (rows-1)*gap
	for i := 0; i < v.Count; i++ {
		x := margin + (i%columns)*(cell+gap)
		y := controlsTop + (i/columns)*(height+gap)
		g.Fields[i] = image.Rect(x, y, x+cell, y+height)
	}
	if v.Selected < 0 || v.Selected >= v.Count {
		return g
	}
	g.Panel = image.Rect(max(margin, w-margin-max(300, w/4)), margin+64, w-margin, controlsTop-gap)
	g.Title = image.Rect(g.Panel.Min.X, g.Panel.Min.Y, g.Panel.Max.X, g.Panel.Min.Y+72)
	body := image.Rect(g.Panel.Min.X, g.Title.Max.Y+8, g.Panel.Max.X, g.Panel.Max.Y)
	if v.Slider {
		g.Slider = body.Inset(12)
		return g
	}
	if v.OptionCount < 1 || v.OptionCount > 8 {
		return g
	}
	row := body.Dy() / v.OptionCount
	for i := 0; i < v.OptionCount; i++ {
		g.Values[i] = image.Rect(body.Min.X, body.Min.Y+i*row, body.Max.X, body.Min.Y+(i+1)*row)
	}
	return g
}

// Camera controls use the same double-buffered Mali overlay as the menu.
// The live camera stays on VPP; unchanged controls submit no GPU work.
func (b *Buffer) RenderCameraHUD(v *CameraHUD) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.hardwareGlassUnsafe {
		return fmt.Errorf("camera overlay unavailable")
	}
	if v != nil && v.Count == 0 && v.Selected == -1 && v.OptionCount == 0 && !v.Slider && !v.WideSlider && !v.Focus && v.Notice == "" {
		v = nil
	}
	if b.bootOrbit {
		if v == nil || v.Count == 0 {
			return nil
		}
		b.bootOrbit = false
	}
	if v == nil {
		if b.cameraHUD == nil {
			return nil
		}
		b.cameraHUD = nil
		b.layerMenuFD = 0
		return b.commitLayersLocked(nil)
	}
	if b.menuGPU == nil || b.scanout == nil {
		return fmt.Errorf("camera overlay requires GPU")
	}
	if v.Count < 0 || v.Count > len(v.Titles) || v.Selected < -1 || v.Selected >= v.Count || v.OptionCount < 0 || v.OptionCount > len(v.Options) || (v.Slider || v.WideSlider) && (v.Max <= v.Min || v.Value < v.Min || v.Value > v.Max) {
		return fmt.Errorf("invalid camera overlay state")
	}
	if b.cameraHUD != nil && *b.cameraHUD == *v && b.glass == nil && b.layerMenuFD > 0 {
		return nil
	}
	g := LayoutCameraHUD(b.Width, b.Height, *v)
	d := &gpuDrawList{commands: b.gpuCommandStorage[:0]}
	b.gpuTarget = d
	defer func() { b.gpuTarget = nil }()
	bounds := image.Rect(0, 0, b.Width, b.Height)
	occupied := g.Panel.Union(b.noticeBounds(v.Notice))
	text := func(r image.Rectangle, s string, color uint32) {
		font := fontSize(min(32, max(12, r.Dy()-8)))
		for font > 12 && textWidth(s, font) > r.Dx()-24 {
			font = fontSize(font - 4)
		}
		b.smoothText(r.Min.X+12, r.Min.Y+(r.Dy()-font)/2, s, font, color, r)
	}
	if v.WideSlider {
		occupied = occupied.Union(g.Slider)
		level := g.Slider.Min.X + g.Slider.Dx()*(v.Value-v.Min)/max(1, v.Max-v.Min)
		b.glassCard(g.Slider, 0, 0, 210, bounds)
		for i := 0; i < 40; i++ {
			r := image.Rect(g.Slider.Min.X+i*g.Slider.Dx()/40+2, g.Slider.Min.Y+8, g.Slider.Min.X+(i+1)*g.Slider.Dx()/40-2, g.Slider.Max.Y-8)
			b.glassCard(r, 0, 0xffdf32, 40, bounds)
			r.Max.X = min(r.Max.X, level)
			if !r.Empty() {
				b.glassCard(r, 0, 0xffdf32, 245, bounds)
			}
		}
	}
	for i := 0; i < v.Count; i++ {
		for j, r := range []image.Rectangle{g.Minus[i], g.Plus[i]} {
			if r.Empty() {
				continue
			}
			occupied = occupied.Union(r)
			b.glassCard(r, 4, 0, 210, bounds)
			x, y := (r.Min.X+r.Max.X)/2, (r.Min.Y+r.Max.Y)/2
			d := min(16, r.Dx()/4)
			b.glassCard(image.Rect(x-d, y-2, x+d, y+2), 0, 0xffdf32, 255, r)
			if j == 1 {
				b.glassCard(image.Rect(x-2, y-d, x+2, y+d), 0, 0xffdf32, 255, r)
			}
		}
		if r := g.Reset[i]; !r.Empty() {
			occupied = occupied.Union(r)
			b.glassCard(r, 4, 0, 210, bounds)
			x, y := (r.Min.X+r.Max.X)/2, (r.Min.Y+r.Max.Y)/2
			d := min(24, min(r.Dx(), r.Dy())/4)
			b.glassOutline(image.Rect(x-d, y-d, x+d, y+d), 2, 0xffdf32, 255, r)
			b.glassCard(image.Rect(x-2, y-d-8, x+2, y+d+8), 0, 0xffdf32, 255, r)
			b.glassCard(image.Rect(x-d-8, y-2, x+d+8, y+2), 0, 0xffdf32, 255, r)
		}
		r := g.Fields[i]
		occupied = occupied.Union(r)
		b.glassCard(r, 4, 0, 210, bounds)
		color := uint32(0xffffff)
		if i == v.Selected {
			color = 0xffffff
			b.glassOutline(r, 3, color, 255, bounds)
		}
		middle := r.Min.Y + r.Dy()/2
		text(image.Rect(r.Min.X, r.Min.Y, r.Max.X, middle), v.Titles[i], color)
		text(image.Rect(r.Min.X, middle, r.Max.X, r.Max.Y), v.Values[i], 0xd8d8d8)
	}
	if !g.Panel.Empty() {
		b.glassCard(g.Panel, 4, 0, 235, bounds)
		title := v.Titles[v.Selected] + " " + v.Values[v.Selected]
		text(g.Title, title, 0xffffff)
		if v.Slider {
			level := g.Slider.Max.Y - g.Slider.Dy()*(v.Value-v.Min)/max(1, v.Max-v.Min)
			for i := 0; i < 20; i++ {
				r := image.Rect(g.Slider.Min.X, g.Slider.Min.Y+i*g.Slider.Dy()/20+2, g.Slider.Max.X, g.Slider.Min.Y+(i+1)*g.Slider.Dy()/20-2)
				b.glassCard(r, 0, 0xffffff, 45, g.Slider)
				r.Min.Y = max(r.Min.Y, level)
				if !r.Empty() {
					b.glassCard(r, 0, 0xffffff, 245, g.Slider)
				}
			}
		} else {
			for i := 0; i < v.OptionCount; i++ {
				r := g.Values[i]
				color := uint32(0xd8d8d8)
				if i == v.OptionSelected {
					color = 0xffffff
					b.glassOutline(r.Inset(4), 2, color, 255, g.Panel)
				}
				text(r, v.Options[i], color)
			}
		}
	}
	if v.Focus {
		x, y := int(v.FocusX)*(b.Width-1)/32767, int(v.FocusY)*(b.Height-1)/32767
		b.glassOutline(image.Rect(x-40, y-40, x+40, y+40).Intersect(bounds), 3, 0xffffff, 255, bounds)
		occupied = occupied.Union(image.Rect(x-40, y-40, x+40, y+40).Intersect(bounds))
	}
	b.gpuNotice(v.Notice)
	if d.err != nil {
		return d.err
	}
	b.gpuCommandStorage = d.commands
	_, _, active, _ := b.scanout.LayerState()
	slot := 0
	if active.Enabled && active.FD == b.menuGPU.MenuFD(0) {
		slot = 1
	}
	if err := b.scanout.WaitMenuReusable(b.menuGPU.MenuFD(slot)); err != nil {
		return err
	}
	fd, err := b.menuGPU.RenderSlot(int(b.Rotation), b.Width, b.Height, d.commands, slot)
	if err != nil {
		return err
	}
	copy := *v
	b.cameraHUD = &copy
	b.glass = nil
	b.layerMenuFD = fd
	if b.layerMenuRect, err = b.overlayBounds(occupied); err != nil {
		return err
	}
	return b.commitLayersLocked(nil)
}

func (b *Buffer) noticeBounds(text string) image.Rectangle {
	if text == "" {
		return image.Rectangle{}
	}
	return image.Rect(b.Width-39-textWidth(text, 48), 33, b.Width-33, 116).Intersect(image.Rect(0, 0, b.Width, b.Height))
}

func (b *Buffer) gpuNotice(text string) {
	if text == "" {
		return
	}
	font := 48
	clip := image.Rect(0, 0, b.Width, b.Height)
	x, y := b.Width-36-textWidth(text, font), 36
	for _, d := range []image.Point{{-3, -3}, {0, -3}, {3, -3}, {-3, 0}, {3, 0}, {-3, 3}, {0, 3}, {3, 3}} {
		b.smoothText(x+d.X, y+d.Y, text, font, 0, clip)
	}
	b.smoothText(x, y, text, font, 0xffdf32, clip)
}

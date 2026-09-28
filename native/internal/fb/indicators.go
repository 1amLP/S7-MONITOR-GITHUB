package fb

import (
	"fmt"
	"image"
	"os"
)

type Activity uint8

const (
	Off Activity = iota
	Idle
	Active
	Unknown
)

type Indicator struct {
	Kind  string
	State Activity
	Text  string
}
type IndicatorPlacement struct {
	Item            Indicator
	Rect            image.Rectangle
	Size, TextScale int
}
type savedPixel struct {
	X, Y  int16
	Value uint32
}

var kinds = map[string]bool{"monitor": true, "camera": true, "microphone": true, "speaker": true, "touch": true, "torch": true, "battery": true, "temperature": true, "cpu": true}

func IndicatorLayout(w, h int, items []Indicator, corner int) []IndicatorPlacement {
	return IndicatorLayoutScaled(w, h, items, corner, 100)
}
func ValidIndicatorScale(percent int) bool { return percent >= 50 && percent <= 200 && percent%5 == 0 }
func IndicatorLayoutScaled(w, h int, items []Indicator, corner, percent int) []IndicatorPlacement {
	if w < 160 || h < 160 || w > 4096 || h > 4096 || corner < 0 || corner > 3 || len(items) > 9 || !ValidIndicatorScale(percent) {
		return nil
	}
	active := []Indicator{}
	for _, v := range items {
		if v.State != Off {
			active = append(active, v)
		}
	}
	if len(active) == 0 {
		return nil
	}
	size := max(8, max(18, min(w, h)*11/400)*percent/100)
	margin := max(4, min(w, h)/100)
	gap, ts, width, total := 0, 0, 0, 0
	for {
		gap = max(2, size/3)
		ts = max(1, size/10)
		width = size
		for _, v := range active {
			width = max(width, size+gap+len(v.Text)*6*ts)
		}
		total = len(active)*size + (len(active)-1)*gap
		if (total <= h-2*margin && width <= w-2*margin) || size <= 8 {
			break
		}
		size--
	}
	if total > h-2*margin || width > w-2*margin {
		return nil
	}
	x, y := margin, margin
	if corner&1 != 0 {
		x = w - margin - width
	}
	if corner&2 != 0 {
		y = h - margin - total
	}
	out := []IndicatorPlacement{}
	for _, v := range active {
		out = append(out, IndicatorPlacement{v, image.Rect(x, y, x+width, y+size), size, ts})
		y += size + gap
	}
	return out
}
func (b *Buffer) indicatorPercent() int {
	if b.indicatorScale == 0 {
		return 100
	}
	return b.indicatorScale
}
func (b *Buffer) restoreIndicators() {
	for i := len(b.indicatorBackup) - 1; i >= 0; i-- {
		v := b.indicatorBackup[i]
		b.point(int(v.X), int(v.Y), v.Value)
	}
	b.indicatorBackup = b.indicatorBackup[:0]
}
func (b *Buffer) overlayPoint(x, y int, p uint32) {
	if x < 0 || y < 0 || x >= b.Width || y >= b.Height {
		return
	}
	b.indicatorBackup = append(b.indicatorBackup, savedPixel{int16(x), int16(y), b.at(x, y)})
	b.point(x, y, p)
}
func (b *Buffer) overlayDisk(x, y, r int, p uint32) {
	for yy := -r; yy <= r; yy++ {
		for xx := -r; xx <= r; xx++ {
			if xx*xx+yy*yy <= r*r {
				b.overlayPoint(x+xx, y+yy, p)
			}
		}
	}
}
func (b *Buffer) overlayLine(x1, y1, x2, y2, r int, p uint32) {
	dx, dy := x2-x1, y2-y1
	n := max(abs(dx), abs(dy))
	if n == 0 {
		b.overlayDisk(x1, y1, r, p)
		return
	}
	for i := 0; i <= n; i++ {
		b.overlayDisk(x1+dx*i/n, y1+dy*i/n, r, p)
	}
}
func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

type segment struct{ X1, Y1, X2, Y2 int }

func icon(kind string) []segment {
	box := func(x1, y1, x2, y2 int) []segment {
		return []segment{{x1, y1, x2, y1}, {x2, y1, x2, y2}, {x2, y2, x1, y2}, {x1, y2, x1, y1}}
	}
	switch kind {
	case "monitor":
		return append(box(2, 2, 14, 11), segment{8, 11, 8, 14}, segment{5, 14, 11, 14})
	case "camera":
		return append(box(1, 4, 14, 13), box(5, 6, 10, 11)...)
	case "sniper":
		return append(box(4, 4, 12, 12), segment{8, 1, 8, 6}, segment{8, 10, 8, 15}, segment{1, 8, 6, 8}, segment{10, 8, 15, 8})
	case "webcam":
		return append(append(box(2, 2, 14, 11), box(6, 4, 10, 8)...), segment{8, 11, 8, 14}, segment{4, 14, 12, 14})
	case "microphone":
		return append(box(6, 2, 10, 10), segment{3, 7, 3, 10}, segment{3, 10, 8, 13}, segment{8, 13, 13, 10}, segment{13, 10, 13, 7}, segment{8, 13, 8, 15})
	case "speaker":
		return []segment{{2, 6, 5, 6}, {5, 6, 9, 2}, {9, 2, 9, 14}, {9, 14, 5, 10}, {5, 10, 2, 10}, {2, 10, 2, 6}, {12, 4, 14, 8}, {14, 8, 12, 12}}
	case "touch":
		return append(box(1, 2, 14, 14), segment{6, 10, 8, 6}, segment{8, 6, 10, 10}, segment{10, 10, 9, 12})
	case "torch":
		return []segment{{2, 2, 14, 2}, {14, 2, 10, 7}, {10, 7, 10, 14}, {10, 14, 6, 14}, {6, 14, 6, 7}, {6, 7, 2, 2}, {5, 5, 11, 5}}
	case "battery":
		return append(box(1, 4, 13, 12), segment{15, 6, 15, 10}, segment{4, 7, 10, 7}, segment{4, 9, 10, 9})
	case "temperature":
		return append(box(6, 1, 10, 10), segment{6, 10, 4, 12}, segment{4, 12, 6, 15}, segment{6, 15, 10, 15}, segment{10, 15, 12, 12}, segment{12, 12, 10, 10}, segment{8, 6, 8, 12})
	case "cpu", "device":
		return append(box(4, 4, 12, 12), segment{1, 5, 4, 5}, segment{1, 10, 4, 10}, segment{12, 5, 15, 5}, segment{12, 10, 15, 10}, segment{5, 1, 5, 4}, segment{10, 1, 10, 4}, segment{5, 12, 5, 15}, segment{10, 12, 10, 15})
	case "power":
		return []segment{{8, 1, 8, 8}, {4, 3, 2, 5}, {2, 5, 2, 11}, {2, 11, 5, 14}, {5, 14, 11, 14}, {11, 14, 14, 11}, {14, 11, 14, 5}, {14, 5, 12, 3}}
	}
	return nil
}
func (b *Buffer) drawIndicators() {
	if b.s7BootVisual {
		return
	} // Native indicators are drawn in the Mali menu header.
	if !b.indicatorsOn || b.glass != nil {
		return
	}
	black := b.pixelRGB(0, 0, 0)
	for _, v := range IndicatorLayoutScaled(b.Width, b.Height, b.indicators, b.indicatorCorner, b.indicatorPercent()) {
		color := b.pixelRGB(186, 191, 199)
		if v.Item.State == Active {
			color = b.pixelRGB(255, 218, 97)
		}
		x, y, size := v.Rect.Min.X, v.Rect.Min.Y, v.Size
		for _, outline := range []bool{true, false} {
			r := max(1, size/32)
			p := color
			if outline {
				r += max(1, size/30)
				p = black
			}
			for _, l := range icon(v.Item.Kind) {
				b.overlayLine(x+l.X1*size/16, y+l.Y1*size/16, x+l.X2*size/16, y+l.Y2*size/16, r, p)
			}
		}
		if v.Item.Text != "" {
			px, py := x+size+max(4, size/3), y+(size-7*v.TextScale)/2
			// Black outline, no filled badge rectangle or diffuse shadow.
			for _, outline := range []bool{true, false} {
				cx := px
				for _, c := range v.Item.Text {
					g, ok := glyph[c]
					if !ok {
						g = glyph['?']
					}
					for row, bits := range g {
						for col := 0; col < 5; col++ {
							if bits&(1<<(4-col)) != 0 {
								pad := 0
								p := color
								if outline {
									pad = 1
									p = black
								}
								for dy := -pad; dy < v.TextScale+pad; dy++ {
									for dx := -pad; dx < v.TextScale+pad; dx++ {
										b.overlayPoint(cx+col*v.TextScale+dx, py+row*v.TextScale+dy, p)
									}
								}
							}
						}
					}
					cx += 6 * v.TextScale
				}
			}
		}
	}
}

// Indicators changes only the overlay. Old strokes are restored before moving,
// disabling or recolouring, even when the monitor has stopped sending frames.
func (b *Buffer) Indicators(items []Indicator, corner int, on bool) error {
	return b.IndicatorsScaled(items, corner, on, 100)
}
func (b *Buffer) IndicatorsScaled(items []Indicator, corner int, on bool, percent int) error {
	if !ValidIndicatorScale(percent) {
		return fmt.Errorf("indicator size requires 50..200 percent in steps of five")
	}
	if len(items) > 9 || corner < 0 || corner > 3 {
		return fmt.Errorf("invalid indicator collection")
	}
	seen := map[string]bool{}
	for _, v := range items {
		if !kinds[v.Kind] || seen[v.Kind] || v.State > Unknown || len(v.Text) > 12 {
			return fmt.Errorf("invalid indicator")
		}
		seen[v.Kind] = true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return os.ErrClosed
	}
	if e := b.Validate(); e != nil {
		return e
	}
	w, h := b.Rotation.Size(int(b.V.X), int(b.V.Y))
	if !b.Rotation.Valid() || w != b.Width || h != b.Height || len(b.Data) < int(b.F.MemoryLength) {
		return fmt.Errorf("invalid indicator framebuffer mapping")
	}
	b.restoreFeedback()
	b.restoreIndicators()
	if b.glass == nil {
		b.restorePreview()
	}
	b.indicators = append(b.indicators[:0], items...)
	b.indicatorCorner = corner
	b.indicatorScale = percent
	b.indicatorsOn = on
	if b.glass == nil {
		b.drawPreview()
	}
	b.drawIndicators()
	b.drawFeedback()
	if b.glass == nil {
		return b.commit()
	}
	return nil
}

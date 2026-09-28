package fb

import "image"

// Three local actions only. The hardware Home/Back keys dismiss this overlay.
func LayoutPower(w, h int, lines []string) GlassLayout {
	if len(lines) != 4 {
		return GlassLayout{}
	}
	l := LayoutStatistics(w, h, lines, 0)
	if l.Scale == 0 {
		return l
	}
	l.Statistics, l.PowerOnly = false, true
	l.Maximum, l.Scroll = 0, 0
	l.Back, l.Up, l.Down = image.Rectangle{}, image.Rectangle{}, image.Rectangle{}
	l.MiddleTitleRect.Max.X = l.Top.Max.X
	l.Body = l.Middle
	l.Rows = nil
	gap := l.Pad
	height := (l.Body.Dy() - 2*gap) / 3
	for i := 1; i <= 3; i++ {
		y := l.Body.Min.Y + (i-1)*(height+gap)
		r := image.Rect(l.Body.Min.X, y, l.Body.Max.X, y+height)
		text := wrapText(displayLabel(lines[i]), max(1, r.Dx()-2*l.Pad), l.Font)
		tr := r.Inset(l.Pad)
		tr.Min.Y = y + max(0, (height-len(text)*l.LineHeight)/2)
		l.Rows = append(l.Rows, GlassRow{Index: i, Rect: r, TextRect: tr, Lines: text})
	}
	return l
}

func (b *Buffer) rasterPower(l GlassLayout) {
	const gold, ink = uint32(0xffc857), uint32(0xd9f2f2)
	b.glassOutline(l.Panel, max(1, l.Scale), gold, 225, l.Panel)
	for i, title := range l.Title {
		b.smoothText(l.Top.Min.X, l.Top.Min.Y+i*l.TitleFont*5/4, title, l.TitleFont, gold, l.Top)
	}
	for _, row := range l.Rows {
		b.glassOutline(row.Rect, max(1, l.Scale), gold, 200, l.Body)
		for i, label := range row.Lines {
			b.smoothText(row.TextRect.Min.X, row.TextRect.Min.Y+i*l.LineHeight, label, l.Font, ink, row.TextRect)
		}
	}
}

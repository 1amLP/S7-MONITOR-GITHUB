package fb

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"strings"
	"sync"
	"unicode"

	"perimode/native/internal/gpumenu"
)

// The atlas stores raster coverage, not a font interpreter or an Android service.
// Keeping masks at discrete physical pixel sizes avoids magnified bitmap text.
type maskEntry struct{ Size, Rune, Offset, W, H, Left, Top, Advance int }

var maskOnce sync.Once
var maskData []byte
var maskIndex map[[2]int]maskEntry

func loadMasks() {
	maskOnce.Do(func() {
		data, err := base64.StdEncoding.DecodeString(uiMaskZlib)
		if err != nil {
			panic(err)
		}
		z, err := zlib.NewReader(bytes.NewReader(data))
		if err != nil {
			panic(err)
		}
		maskData, err = io.ReadAll(io.LimitReader(z, uiMaskRawBytes+1))
		_ = z.Close()
		if err != nil || len(maskData) != uiMaskRawBytes {
			panic("invalid built-in UI coverage masks")
		}
		maskIndex = make(map[[2]int]maskEntry, len(uiGlyphTable))
		for _, g := range uiGlyphTable {
			maskIndex[[2]int{g.Size, g.Rune}] = g
		}
	})
}
func fontSize(want int) int {
	best := 12
	for _, s := range []int{12, 14, 16, 18, 20, 24, 28, 32, 36, 40, 48, 56, 64} {
		if abs(s-want) < abs(best-want) {
			best = s
		}
	}
	return best
}
func maskGlyph(c rune, size int) maskEntry {
	loadMasks()
	if size >= 32 && (c >= '0' && c <= '9' || c == '.' || c == '-') {
		if g, ok := maskIndex[[2]int{size, int(c) + 0xf000}]; ok {
			return g
		}
	}
	if g, ok := maskIndex[[2]int{size, int(c)}]; ok {
		return g
	}
	return maskIndex[[2]int{size, int('?')}]
}
func textWidth(s string, size int) int {
	n := 0
	for _, c := range s {
		n += maskGlyph(c, size).Advance
	}
	return (n + 63) / 64
}
func (b *Buffer) blendPoint(x, y int, rgb uint32, a int) {
	if b.gpuTarget != nil {
		if b.gpuTarget.err == nil {
			b.gpuTarget.err = fmt.Errorf("software pixel drawing reached GPU menu")
		}
		return
	}
	if a <= 0 || x < 0 || y < 0 || x >= b.Width || y >= b.Height {
		return
	}
	a = min(255, a)
	if b.inkTarget != nil {
		b.inkTarget.blend(x, y, rgb, a)
		return
	}
	r, g, bl := unpackRGB(b.at(x, y), b.V)
	b.point(x, y, b.pixelRGB(byte((int(r)*(255-a)+int(rgb>>16&255)*a+127)/255), byte((int(g)*(255-a)+int(rgb>>8&255)*a+127)/255), byte((int(bl)*(255-a)+int(rgb&255)*a+127)/255)))
}
func (b *Buffer) smoothText(x, y int, s string, size int, rgb uint32, clip image.Rectangle) {
	pen := x * 64
	baseline := y + size
	for _, c := range s {
		g := maskGlyph(c, size)
		ox := (pen+32)/64 + g.Left
		oy := baseline + g.Top
		rect := image.Rect(ox, oy, ox+g.W, oy+g.H).Intersect(clip).Intersect(image.Rect(0, 0, b.Width, b.Height))
		if b.gpuTarget != nil {
			b.gpuTarget.add(gpumenu.Glyph, rect, rgb, 255, ox, oy, g.W, g.H, g.Offset)
			pen += g.Advance
			continue
		}
		for py := rect.Min.Y; py < rect.Max.Y; py++ {
			for px := rect.Min.X; px < rect.Max.X; px++ {
				a := int(maskData[g.Offset+(py-oy)*g.W+px-ox])
				b.blendPoint(px, py, rgb, a)
			}
		}
		pen += g.Advance
	}
}

// Only display labels are normalized. Original command labels/IDs remain intact.
func displayLabel(s string) string {
	s = strings.TrimSpace(s)
	if s == "AUTO UPDATE IN PROGRESS" {
		return "\u0418\u0434\u0451\u0442 \u0430\u0432\u0442\u043e\u043e\u0431\u043d\u043e\u0432\u043b\u0435\u043d\u0438\u0435"
	}
	s = strings.ReplaceAll(s, " PCT", "%")
	s = strings.ReplaceAll(s, " DEG", "\u00b0")
	words := strings.Fields(s)
	keep := map[string]bool{"S7": true, "USB": true, "HID": true, "H264": true, "MFC": true, "ISP": true, "FPS": true, "CPU": true, "PCM": true, "ALSA": true, "ISO": true, "AE": true, "AF": true, "AWB": true, "NV12": true, "IDR": true, "GOP": true, "DPI": true, "TL": true, "TR": true, "BL": true, "BR": true, "EFS": true, "HBM": true, "PTP": true}
	for i, w := range words {
		token := strings.Trim(w, ":,()?")
		if keep[token] || strings.Contains(w, "/") || strings.ContainsAny(w, "0123456789") {
			continue
		}
		w = strings.ToLower(w)
		if i == 0 || words[i-1] == "/" {
			rr := []rune(w)
			if len(rr) > 0 {
				rr[0] = unicode.ToUpper(rr[0])
				w = string(rr)
			}
		}
		words[i] = w
	}
	return strings.Join(words, " ")
}
func wrapText(s string, width, size int) []string {
	out := []string{}
	line := []rune{}
	advance := 0
	for _, c := range s {
		a := maskGlyph(c, size).Advance
		if len(line) > 0 && (advance+a+63)/64 > width {
			cut := len(line)
			for i := len(line) - 1; i > len(line)/2; i-- {
				if line[i] == ' ' {
					cut = i
					break
				}
			}
			out = append(out, string(line[:cut]))
			line = []rune(strings.TrimLeft(string(line[cut:]), " "))
			advance = 0
			for _, r := range line {
				advance += maskGlyph(r, size).Advance
			}
		}
		if len(line) == 0 && c == ' ' {
			continue
		}
		line = append(line, c)
		advance += a
	}
	if len(line) > 0 || len(out) == 0 {
		out = append(out, string(line))
	}
	return out
}

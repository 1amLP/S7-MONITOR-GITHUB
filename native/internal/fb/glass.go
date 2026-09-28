package fb

import (
	"encoding/binary"
	"fmt"
	"image"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"perimode/native/internal/fimg2d"
	"perimode/native/internal/gpumenu"
	"perimode/native/internal/media"
)

// GlassLayout is shared by rendering and input. Rows retain their source index
// across wrapping and scrolling, so a long diagnostic cannot move a button's ID.
type GlassRow struct {
	Index                        int
	Rect                         image.Rectangle
	Lines, ValueLines            []string
	TextRect, ValueRect, Control image.Rectangle
	IconRect                     image.Rectangle
	Kind                         string
	Enabled                      bool
	Value                        int
	Minimum, Maximum, Step       int
}
type GlassLayout struct {
	Install                             image.Rectangle
	InstallLabel                        string
	Panel, Body, Up, Down, Rotate, Back image.Rectangle
	Top, Left, Middle, Right            image.Rectangle
	DetailTitleRect, DetailHintRect     image.Rectangle
	MiddleTitleRect                     image.Rectangle
	Scale, Scroll, Maximum              int
	Font, TitleFont, Pad, LineHeight    int
	Title, DetailTitle, DetailHint      []string
	Rows                                []GlassRow
	Categories                          []GlassRow
	Details                             []GlassRow
	Statistics                          bool
	PowerOnly                           bool
}

const (
	GlassUp             = -2
	GlassDown           = -3
	GlassRotate         = -4
	GlassStatisticsBack = -5
	GlassInstallDisk    = -6
	GlassCategoryBase   = 1000
	GlassDetailBase     = 2000
	GlassDetailLimit    = 16
)

var glassCategories = [...]string{"Monitor", "Touch", "Camera", "Sniper", "Audio", "Device"}
var glassCategoryIcons = [...]string{"monitor", "touch", "camera", "sniper", "speaker", "device"}

type MenuStatus struct {
	InstallStatus, InstallAction                       string
	Notice                                             string
	Selected, Focused                                  int
	DetailSelected, DetailCount                        int
	DetailHasSelection                                 bool
	DetailAction                                       bool
	AutoRotate                                         bool
	Statistics                                         bool
	PowerOnly                                          bool
	DetailSlider                                       bool
	DetailSliderValue                                  int
	DetailSliderMin, DetailSliderMax, DetailSliderStep int
	Mode, Rate, Codec, Battery, Temperature, Load, USB string
	DetailTitle, DetailHint                            string
	DetailValueText                                    string
	DetailOptions                                      [GlassDetailLimit]string
	Functions                                          [6]Indicator
}

func (b *Buffer) SetMenuStatus(v MenuStatus) error {
	if v.Selected < -1 || v.Selected >= len(glassCategories) || v.Focused < 0 || v.Focused > 64 || v.DetailCount < 0 || v.DetailCount > len(v.DetailOptions) {
		return fmt.Errorf("invalid menu category")
	}
	if v.DetailHasSelection && (v.DetailSelected < 0 || v.DetailSelected >= v.DetailCount) {
		return fmt.Errorf("invalid menu detail selection")
	}
	if v.DetailSlider {
		if v.DetailSliderMin == 0 && v.DetailSliderMax == 0 && v.DetailSliderStep == 0 {
			v.DetailSliderMin, v.DetailSliderMax, v.DetailSliderStep = 50, 200, 5
		}
		if v.DetailCount != 0 || v.DetailSliderMin < -1_000_000 || v.DetailSliderMax > 1_000_000 || v.DetailSliderMin >= v.DetailSliderMax || v.DetailSliderStep < 1 || v.DetailSliderStep > v.DetailSliderMax-v.DetailSliderMin || v.DetailSliderValue < v.DetailSliderMin || v.DetailSliderValue > v.DetailSliderMax {
			return fmt.Errorf("invalid menu detail slider")
		}
	}
	for _, s := range append([]string{v.InstallStatus, v.InstallAction, v.Notice, v.Mode, v.Rate, v.Codec, v.Battery, v.Temperature, v.Load, v.USB, v.DetailTitle, v.DetailHint, v.DetailValueText}, v.DetailOptions[:]...) {
		if len(s) > 48 || !utf8.ValidString(s) {
			return fmt.Errorf("invalid menu status")
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.menuStatus != v {
		previous := b.menuStatus
		if !b.menuStatusDirty {
			b.menuPreviousStatus = previous
		}
		staticChanged := previous.PowerOnly != v.PowerOnly || previous.InstallAction != v.InstallAction || b.menuStatus.Selected != v.Selected || b.menuStatus.Focused != v.Focused || previous.Statistics != v.Statistics ||
			b.menuStatus.DetailSelected != v.DetailSelected || b.menuStatus.DetailCount != v.DetailCount ||
			b.menuStatus.DetailHasSelection != v.DetailHasSelection || b.menuStatus.DetailAction != v.DetailAction || b.menuStatus.DetailTitle != v.DetailTitle ||
			b.menuStatus.DetailHint != v.DetailHint || b.menuStatus.DetailOptions != v.DetailOptions ||
			b.menuStatus.DetailSlider != v.DetailSlider || b.menuStatus.DetailSliderValue != v.DetailSliderValue || previous.DetailValueText != v.DetailValueText ||
			b.menuStatus.DetailSliderMin != v.DetailSliderMin || b.menuStatus.DetailSliderMax != v.DetailSliderMax || b.menuStatus.DetailSliderStep != v.DetailSliderStep
		headerChanged := previous.InstallStatus != v.InstallStatus || previous.InstallAction != v.InstallAction || previous.Notice != v.Notice || previous.AutoRotate != v.AutoRotate || previous.Mode != v.Mode || previous.Rate != v.Rate || previous.Codec != v.Codec ||
			previous.Battery != v.Battery || previous.Temperature != v.Temperature || previous.Load != v.Load || previous.USB != v.USB || previous.Functions != v.Functions
		b.menuStatus = v
		b.menuStatusDirty = true
		b.menuForegroundDirty = b.menuForegroundDirty || staticChanged
		b.menuCategoryDirty = b.menuCategoryDirty || previous.Selected != v.Selected
		b.menuHeaderDirty = b.menuHeaderDirty || headerChanged
	}
	return nil
}

// Kept for callers which want character-count wrapping, without forcing CAPS.
func wrapLabel(s string, n int) []string {
	if n < 1 {
		n = 1
	}
	r := []rune(s)
	out := []string{}
	for len(r) > n {
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	if len(r) > 0 || len(out) == 0 {
		out = append(out, string(r))
	}
	return out
}
func rowControl(raw string) (kind string, enabled bool, value int) {
	if strings.HasPrefix(raw, "INDICATOR SIZE:") {
		fields := strings.Fields(strings.TrimPrefix(raw, "INDICATOR SIZE:"))
		if len(fields) > 0 {
			value, _ = strconv.Atoi(fields[0])
			return "slider", false, max(50, min(200, value))
		}
	}
	// A real existing row action owns each switch. Read-only boolean diagnostics
	// are not disguised as toggles.
	prefixes := []string{"ENABLED:", "SPEAKER ENABLED:", "MICROPHONE ENABLED:", "PAD ACCELERATION:", "AUTOMATIC:", "MIRROR:", "INDICATORS:", "SHOW INDICATORS:"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(raw, prefix) && (strings.Contains(raw, "true") || strings.Contains(raw, "false") || strings.Contains(raw, "TRUE") || strings.Contains(raw, "FALSE")) {
			return "toggle", strings.Contains(strings.ToLower(raw), "true"), 0
		}
	}
	return "", false, 0
}
func splitGlassValue(raw string) (label, value string) {
	label = displayLabel(raw)
	if i := strings.Index(label, ":"); i >= 0 {
		value = strings.TrimSpace(label[i+1:])
		label = strings.TrimSpace(label[:i])
	}
	return label, value
}
func sameGlassStructure(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		left, _ := splitGlassValue(a[i])
		right, _ := splitGlassValue(b[i])
		if displayLabel(left) != displayLabel(right) {
			return false
		}
		leftKind, _, _ := rowControl(a[i])
		rightKind, _, _ := rowControl(b[i])
		if leftKind != rightKind {
			return false
		}
	}
	return true
}
func LayoutGlass(w, h int, lines []string, scroll int) GlassLayout {
	l := GlassLayout{}
	if w < 160 || h < 160 || w > 4096 || h > 4096 || len(lines) == 0 || len(lines) > 64 {
		return l
	}
	short := min(w, h)
	l.Scale = max(1, short/360)
	l.Font = fontSize(max(12, short/32))
	l.TitleFont = fontSize(l.Font * 4 / 3)
	l.LineHeight = l.Font * 5 / 4
	l.Pad = max(10, short/32)
	marginX, marginY := max(8, w/10), max(8, h/10)
	panelW := w - 2*marginX
	l.Panel = image.Rect(marginX, marginY, w-marginX, h-marginY)
	headerH := max(l.TitleFont*3, short/7)
	l.Top = image.Rect(l.Panel.Min.X+l.Pad, l.Panel.Min.Y+l.Pad/2, l.Panel.Max.X-l.Pad, l.Panel.Min.Y+headerH)
	rotateSize := max(24, l.Font*2)
	l.Rotate = image.Rect(l.Top.Min.X, l.Top.Min.Y, l.Top.Min.X+rotateSize, l.Top.Min.Y+rotateSize)
	contentTop, contentBottom := l.Panel.Min.Y+headerH, l.Panel.Max.Y-l.Pad
	categoryFont := fontSize(max(12, l.Font*2/3))
	categoryGap := max(4, 2*l.Scale)
	captionH := categoryFont * 5 / 4
	categoryAvailable := (contentBottom - contentTop - categoryGap*(len(glassCategories)-1)) / len(glassCategories)
	iconSize := min(max(20, l.Font*3/2), categoryAvailable-captionH-categoryGap)
	if iconSize < 8 {
		return GlassLayout{}
	}
	leftW := iconSize + 2*categoryGap
	for _, label := range glassCategories {
		leftW = max(leftW, textWidth(label, categoryFont)+2*categoryGap)
	}
	rightW := panelW * 25 / 100
	columnGap := max(6, l.Font/2)
	l.Left = image.Rect(l.Panel.Min.X+l.Pad, contentTop, l.Panel.Min.X+l.Pad+leftW, contentBottom)
	l.Right = image.Rect(l.Panel.Max.X-l.Pad-rightW, contentTop, l.Panel.Max.X-l.Pad, contentBottom)
	l.Middle = image.Rect(l.Left.Max.X+columnGap, contentTop, l.Right.Min.X-columnGap, contentBottom)
	// Body is the independently scrollable middle column. The right column is a
	// stable detail pane for the focused middle row, like a physical monitor OSD.
	indicatorH := max(l.Font, l.Pad)
	l.Title = wrapText(displayLabel(lines[0]), max(1, l.Middle.Dx()-2*l.Pad), l.Font)
	titleH := len(l.Title)*l.LineHeight + l.Pad/2
	l.MiddleTitleRect = image.Rect(l.Middle.Min.X+l.Pad, l.Middle.Min.Y, l.Middle.Max.X-l.Pad, l.Middle.Min.Y+titleH)
	l.Up = image.Rect(l.Middle.Min.X, l.MiddleTitleRect.Max.Y, l.Middle.Max.X, l.MiddleTitleRect.Max.Y+indicatorH)
	l.Down = image.Rect(l.Middle.Min.X, l.Middle.Max.Y-indicatorH, l.Middle.Max.X, l.Middle.Max.Y)
	l.Body = image.Rect(l.Middle.Min.X, l.Up.Max.Y, l.Middle.Max.X, l.Down.Min.Y)
	if l.Body.Dy() < l.Font*2 {
		return GlassLayout{}
	}
	categoryH := iconSize + categoryGap + captionH
	cy := l.Left.Min.Y
	for i, label := range glassCategories {
		r := image.Rect(l.Left.Min.X, cy, l.Left.Max.X, cy+categoryH)
		ix := (r.Min.X + r.Max.X - iconSize) / 2
		l.Categories = append(l.Categories, GlassRow{Index: GlassCategoryBase + i, Rect: r,
			Kind: glassCategoryIcons[i], IconRect: image.Rect(ix, cy, ix+iconSize, cy+iconSize),
			Lines: []string{label}, TextRect: image.Rect(r.Min.X, cy+iconSize+categoryGap, r.Max.X, r.Max.Y)})
		cy += categoryH + categoryGap
	}
	content := 0
	rowGap := max(5, l.Font/3)
	for i := 1; i < len(lines); i++ {
		kind, on, value := rowControl(lines[i])
		label, _ := splitGlassValue(lines[i])
		if strings.TrimSpace(label) == "" {
			l.Rows = append(l.Rows, GlassRow{Index: i})
			continue
		}
		if kind == "slider" {
			label = "Indicator size"
		}
		rowLines := wrapText(label, max(1, l.Middle.Dx()-2*l.Pad), l.Font)
		rh := max(l.Font*5/2, len(rowLines)*l.LineHeight+l.Pad)
		rect := image.Rect(l.Body.Min.X, l.Body.Min.Y+content, l.Body.Max.X, l.Body.Min.Y+content+rh)
		r := GlassRow{Index: i, Rect: rect, Lines: rowLines, Kind: kind, Enabled: on, Value: value}
		r.TextRect = image.Rect(l.Middle.Min.X+l.Pad, rect.Min.Y+l.Pad/2, l.Middle.Max.X-l.Pad, rect.Max.Y-l.Pad/2)
		l.Rows = append(l.Rows, r)
		content += rh + rowGap
	}
	content = max(0, content-rowGap)
	l.Maximum = max(0, content-l.Body.Dy())
	l.Scroll = max(0, min(scroll, l.Maximum))
	delta := image.Pt(0, l.Scroll)
	for i := range l.Rows {
		r := &l.Rows[i]
		r.Rect = r.Rect.Sub(delta)
		r.TextRect = r.TextRect.Sub(delta)
		r.ValueRect = r.ValueRect.Sub(delta)
		r.Control = r.Control.Sub(delta)
	}
	return l
}

// LayoutStatistics uses the full content width. Data rows are never hit targets;
// only Back and the scroll controls accept touch input.
func LayoutStatistics(w, h int, lines []string, scroll int) GlassLayout {
	var l GlassLayout
	if w < 160 || h < 160 || w > 4096 || h > 4096 || len(lines) < 2 || len(lines) > 64 {
		return l
	}
	short := min(w, h)
	l.Statistics = true
	l.Scale = max(1, short/360)
	l.Font = fontSize(max(12, short/32))
	l.TitleFont = fontSize(l.Font * 4 / 3)
	l.LineHeight = l.Font * 5 / 4
	l.Pad = max(10, short/32)
	l.Panel = image.Rect(max(8, w/10), max(8, h/10), w-max(8, w/10), h-max(8, h/10))
	headerH := max(l.TitleFont*3, short/7)
	l.Top = image.Rect(l.Panel.Min.X+l.Pad, l.Panel.Min.Y+l.Pad/2, l.Panel.Max.X-l.Pad, l.Panel.Min.Y+headerH)
	backW := max(l.Font*4, textWidth("Back", l.Font)+l.Pad*2)
	l.Back = image.Rect(l.Top.Max.X-backW, l.Top.Min.Y, l.Top.Max.X, l.Top.Min.Y+l.Font*2)
	l.Middle = image.Rect(l.Panel.Min.X+l.Pad, l.Top.Max.Y, l.Panel.Max.X-l.Pad, l.Panel.Max.Y-l.Pad)
	l.Title = wrapText(displayLabel(lines[0]), max(1, l.Top.Dx()-backW-l.Pad*2), l.TitleFont)
	l.MiddleTitleRect = image.Rect(l.Top.Min.X, l.Top.Min.Y, l.Back.Min.X-l.Pad, l.Top.Max.Y)
	indicatorH := max(l.Font, l.Pad)
	l.Up = image.Rect(l.Middle.Min.X, l.Middle.Min.Y, l.Middle.Max.X, l.Middle.Min.Y+indicatorH)
	l.Down = image.Rect(l.Middle.Min.X, l.Middle.Max.Y-indicatorH, l.Middle.Max.X, l.Middle.Max.Y)
	l.Body = image.Rect(l.Middle.Min.X, l.Up.Max.Y, l.Middle.Max.X, l.Down.Min.Y)
	if l.Body.Dy() < l.Font*2 {
		return GlassLayout{}
	}
	labelW := max(l.Font*5, l.Body.Dx()*38/100)
	labelW = min(labelW, l.Body.Dx()/2)
	valueX := l.Body.Min.X + labelW + l.Pad
	content := 0
	rowGap := max(3, l.Scale*2)
	for i := 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "BACK:") {
			continue
		}
		label, value := splitGlassValue(lines[i])
		if strings.TrimSpace(label) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		labels := wrapText(label, max(1, labelW-l.Pad), l.Font)
		values := wrapText(value, max(1, l.Body.Max.X-valueX-l.Pad), l.Font)
		rh := max(l.Font*2, max(len(labels), len(values))*l.LineHeight+l.Pad)
		r := GlassRow{Index: i, Kind: "stat", Lines: labels, ValueLines: values,
			Rect: image.Rect(l.Body.Min.X, l.Body.Min.Y+content, l.Body.Max.X, l.Body.Min.Y+content+rh)}
		r.TextRect = image.Rect(l.Body.Min.X+l.Pad/2, r.Rect.Min.Y+l.Pad/2, valueX-l.Pad/2, r.Rect.Max.Y)
		r.ValueRect = image.Rect(valueX, r.Rect.Min.Y+l.Pad/2, l.Body.Max.X-l.Pad/2, r.Rect.Max.Y)
		l.Rows = append(l.Rows, r)
		content += rh + rowGap
	}
	content = max(0, content-rowGap)
	l.Maximum = max(0, content-l.Body.Dy())
	l.Scroll = max(0, min(scroll, l.Maximum))
	for i := range l.Rows {
		delta := image.Pt(0, l.Scroll)
		l.Rows[i].Rect = l.Rows[i].Rect.Sub(delta)
		l.Rows[i].TextRect = l.Rows[i].TextRect.Sub(delta)
		l.Rows[i].ValueRect = l.Rows[i].ValueRect.Sub(delta)
	}
	return l
}

func applyGlassDetails(l *GlassLayout, status MenuStatus) {
	l.Install, l.InstallLabel = image.Rectangle{}, ""
	if l.Scale == 0 || l.Statistics || l.PowerOnly || status.DetailCount < 0 || status.DetailCount > len(status.DetailOptions) {
		return
	}
	if status.InstallStatus != "" || status.InstallAction != "" {
		height := l.Top.Max.Y - l.Rotate.Max.Y - 4*l.Scale
		top := l.Top.Min.Y + (l.Top.Dy()-height)/2
		l.Install = image.Rect(l.Right.Min.X, top, l.Top.Max.X, top+height)
		l.InstallLabel = status.InstallAction
	}
	detailFont := fontSize(max(12, l.Font*4/5))
	title := displayLabel(status.DetailTitle)
	if status.DetailSlider {
		if status.DetailValueText != "" {
			title = title + " " + status.DetailValueText
		} else {
			title = fmt.Sprintf("%s %d%%", title, status.DetailSliderValue)
		}
	}
	l.DetailTitle = wrapText(title, max(1, l.Right.Dx()-2*l.Pad), detailFont)
	if status.DetailHint != "" {
		l.DetailHint = wrapText(displayLabel(status.DetailHint), max(1, l.Right.Dx()-2*l.Pad), fontSize(l.Font*3/4))
	}
	detailLineHeight := detailFont * 5 / 4
	y := l.Right.Min.Y + l.Pad/2
	titleH := max(detailFont*2, len(l.DetailTitle)*detailLineHeight)
	l.DetailTitleRect = image.Rect(l.Right.Min.X+l.Pad, y, l.Right.Max.X-l.Pad, min(l.Right.Max.Y, y+titleH))
	y = l.DetailTitleRect.Max.Y + l.Pad/2
	gap := max(4, l.Font/3)
	if status.DetailSlider {
		r := GlassRow{Index: GlassDetailBase, Rect: image.Rect(l.Right.Min.X, y, l.Right.Max.X, l.Right.Max.Y-l.Pad/2), Kind: "slider", Value: status.DetailSliderValue}
		r.Minimum, r.Maximum, r.Step = status.DetailSliderMin, status.DetailSliderMax, status.DetailSliderStep
		if r.Maximum == 0 {
			r.Minimum, r.Maximum, r.Step = 50, 200, 5
		}
		r.Control = r.Rect.Inset(max(2, l.Pad/3))
		l.Details = append(l.Details, r)
		y = r.Rect.Max.Y + gap
	}
	for i := 0; i < status.DetailCount; i++ {
		lines := wrapText(displayLabel(status.DetailOptions[i]), max(1, l.Right.Dx()-2*l.Pad), detailFont)
		rh := max(detailFont*5/2, len(lines)*detailLineHeight+l.Pad)
		if y+rh > l.Right.Max.Y {
			break
		}
		r := GlassRow{Index: GlassDetailBase + i, Rect: image.Rect(l.Right.Min.X, y, l.Right.Max.X, y+rh), Lines: lines}
		r.TextRect = r.Rect.Inset(l.Pad / 2)
		l.Details = append(l.Details, r)
		y += rh + gap
	}
	if len(l.DetailHint) > 0 && y < l.Right.Max.Y {
		h := len(l.DetailHint) * max(1, fontSize(l.Font*3/4)*5/4)
		l.DetailHintRect = image.Rect(l.Right.Min.X+l.Pad, max(y, l.Right.Max.Y-h-l.Pad/2), l.Right.Max.X-l.Pad, l.Right.Max.Y-l.Pad/2)
	}
}

func unionGlassRect(a, b image.Rectangle) image.Rectangle {
	if a.Empty() {
		return b
	}
	if b.Empty() {
		return a
	}
	return a.Union(b)
}

func glassStatusDirty(l GlassLayout, previous, current MenuStatus, foreground, category, header bool) image.Rectangle {
	var dirty image.Rectangle
	if header {
		dirty = unionGlassRect(dirty, l.Top)
	}
	if category {
		for i, row := range l.Categories {
			if i == previous.Selected || i == current.Selected {
				dirty = unionGlassRect(dirty, row.Rect)
			}
		}
	}
	if foreground {
		dirty = unionGlassRect(dirty, l.Right)
		for _, row := range l.Rows {
			if row.Index == previous.Focused || row.Index == current.Focused {
				dirty = unionGlassRect(dirty, row.Rect.Intersect(l.Body))
			}
		}
	}
	return dirty.Intersect(l.Panel)
}

// SliderValue uses the same painted track as touch input. Endpoints are clamped
// and every value is quantized to a five-percent step.
func (l GlassLayout) SliderValue(index, w, h int, x, y uint16) (int, bool) {
	if w < 2 || h < 2 || x > 32767 || y > 32767 {
		return 0, false
	}
	p := image.Pt(int(x)*(w-1)/32767, int(y)*(h-1)/32767)
	rows := append(append([]GlassRow(nil), l.Rows...), l.Details...)
	for _, r := range rows {
		bounds := l.Body
		if r.Index >= GlassDetailBase {
			bounds = l.Right
		}
		if r.Index == index && r.Kind == "slider" && !r.Control.Empty() && p.In(r.Control.Intersect(bounds)) {
			top, bottom := r.Control.Min.Y+l.Font/2, r.Control.Max.Y-l.Font/2
			lo, hi, step := r.Minimum, r.Maximum, r.Step
			if hi == 0 {
				lo, hi, step = 50, 200, 5
			}
			n := (max(0, min(bottom-top, bottom-p.Y))*(hi-lo) + max(1, bottom-top)/2) / max(1, bottom-top)
			return max(lo, min(hi, lo+((n+step/2)/step)*step)), true
		}
	}
	return 0, false
}
func (l GlassLayout) Hit(w, h int, x, y uint16) int {
	if w < 2 || h < 2 || x > 32767 || y > 32767 || l.Scale == 0 {
		return -1
	}
	p := image.Pt(int(x)*(w-1)/32767, int(y)*(h-1)/32767)
	if l.Statistics && p.In(l.Back) {
		return GlassStatisticsBack
	}
	for _, r := range l.Details {
		if p.In(r.Rect) {
			return r.Index
		}
	}
	for _, r := range l.Categories {
		if p.In(r.Rect) {
			return r.Index
		}
	}
	if p.In(l.Rotate) {
		return GlassRotate
	}
	if l.InstallLabel != "" && p.In(l.Install) {
		return GlassInstallDisk
	}
	if l.Scroll > 0 && p.In(l.Up) {
		return GlassUp
	}
	if l.Scroll < l.Maximum && p.In(l.Down) {
		return GlassDown
	}
	if !p.In(l.Body) {
		return -1
	}
	if l.Statistics {
		return -1
	}
	for _, r := range l.Rows {
		if p.In(r.Rect) {
			return r.Index
		}
	}
	return -1
}

type glassScene struct {
	xSamples, ySamples   []glassSample
	layout               GlassLayout
	lines                []string
	ink                  *glassInk
	coverage, edge       []byte
	coveragePanel        image.Rectangle
	blurTemp, blurOutput []uint32
	pending              []byte
	pendingPTS           int64
	hasPending           bool
	backdrop             []byte
	backdropPTS          int64
	hasBackdrop          bool
	sourceBacked         bool
	sourceRGB            []uint32
	panelPacked          []uint32
	nextLive             time.Time

	previewDocked bool // geometry actually painted; persists until next menu draw

	cachedPanel   image.Rectangle
	cachedRaw     []byte
	stageReady    bool
	lastDirty     image.Rectangle
	lastCopy      image.Rectangle
	raw           []byte
	rgb           []uint32
	width, height int
}

// blurBox uses sliding windows, constant-edge extension and 32-bit integer sums.
// Three separable passes approximate a Gaussian. Work buffers are reused by
// the paced live-menu path; this wrapper remains useful for small one-shot overlays.
func blurBox(src []uint32, w, h, radius int) []uint32 {
	if w <= 0 || h <= 0 || len(src) != w*h || radius < 0 || radius > 32 {
		return nil
	}
	dst := make([]uint32, len(src))
	tmp := make([]uint32, len(src))
	blurBoxInto(src, tmp, dst, w, h, radius)
	return dst
}

// Internal workspace must be disjoint and exactly w*h; callers own validation.
func blurBoxInto(src, tmp, dst []uint32, w, h, radius int) {
	span := 2*radius + 1
	channel := func(p uint32, shift int) int { return int((p >> shift) & 255) }
	for _, vertical := range []bool{false, true} {
		in, out := src, tmp
		if vertical {
			in, out = tmp, dst
		}
		rows, cols := h, w
		if vertical {
			rows, cols = w, h
		}
		idx := func(row, col int) int {
			if vertical {
				return col*w + row
			}
			return row*w + col
		}
		for row := 0; row < rows; row++ {
			rr, gg, bb := 0, 0, 0
			add := func(col, sign int) {
				p := in[idx(row, max(0, min(cols-1, col)))]
				rr += sign * channel(p, 16)
				gg += sign * channel(p, 8)
				bb += sign * channel(p, 0)
			}
			for col := -radius; col <= radius; col++ {
				add(col, 1)
			}
			for col := 0; col < cols; col++ {
				out[idx(row, col)] = uint32(rr/span)<<16 | uint32(gg/span)<<8 | uint32(bb/span)
				add(col-radius, -1)
				add(col+radius+1, 1)
			}
		}
	}
}
func unpackRGB(p uint32, v Variable) (byte, byte, byte) {
	c := func(f BitField) byte {
		if f.Length == 0 {
			return 0
		}
		m := uint32(1<<f.Length) - 1
		return byte(((p >> f.Offset) & m) * 255 / m)
	}
	return c(v.Red), c(v.Green), c(v.Blue)
}
func (b *Buffer) at(x, y int) uint32 {
	x, y = b.Rotation.ToPanel(x, y, int(b.V.X), int(b.V.Y))
	off := (y+int(b.V.YOffset))*int(b.F.LineLength) + (x+int(b.V.XOffset))*int(b.V.Bits/8)
	if b.V.Bits == 32 {
		return binary.LittleEndian.Uint32(b.Data[off:])
	}
	return uint32(binary.LittleEndian.Uint16(b.Data[off:]))
}
func (b *Buffer) beginGlass() error {
	if b.glass != nil {
		return nil
	}
	if len(b.Data) != int(b.F.MemoryLength) || len(b.Data) > 64<<20 {
		return fmt.Errorf("unsafe glass snapshot geometry")
	}
	b.restoreIndicators()
	b.restorePreview()
	sc := &glassScene{raw: append([]byte(nil), b.Data...), width: min(480, b.Width)}
	sc.height = max(1, b.Height*sc.width/b.Width)
	if sc.height > 480 {
		sc.height = 480
		sc.width = max(1, b.Width*sc.height/b.Height)
	}
	sc.rgb = make([]uint32, sc.width*sc.height)
	sc.blurTemp = make([]uint32, len(sc.rgb))
	sc.blurOutput = make([]uint32, len(sc.rgb))
	b.sampleGlassBackdrop(sc)
	b.glass = sc
	return nil
}

// beginGlassSource creates the production menu from a decoder-owned frame or
// pure black. It never reads uncached framebuffer pixels back into RAM.
func (b *Buffer) beginGlassSource(source *media.Image) error {
	if b.glass != nil {
		return nil
	}
	if len(b.Data) != int(b.F.MemoryLength) || len(b.Data) > 64<<20 {
		return fmt.Errorf("unsafe glass snapshot geometry")
	}
	b.restoreIndicators()
	b.restorePreview()
	sc := &glassScene{sourceBacked: true, width: min(480, b.Width)}
	sc.height = max(1, b.Height*sc.width/b.Width)
	if sc.height > 480 {
		sc.height = 480
		sc.width = max(1, b.Width*sc.height/b.Height)
	}
	sc.sourceRGB = make([]uint32, sc.width*sc.height)
	sc.rgb = make([]uint32, len(sc.sourceRGB))
	sc.blurTemp = make([]uint32, len(sc.rgb))
	sc.blurOutput = make([]uint32, len(sc.rgb))
	if source != nil {
		if err := validateSourceImage(*source); err != nil {
			return err
		}
		sc.storeBackdrop(*source)
		b.sampleImageBackdrop(sc, *source)
	}
	b.glass = sc
	return nil
}

// roundedInside uses integer geometry. Only the card is tinted; black areas of
// the AMOLED monitor remain unchanged whenever the menu is closed.
func roundedInside(r image.Rectangle, x, y, radius int) bool {
	if !image.Pt(x, y).In(r) {
		return false
	}
	rad := min(radius, min(r.Dx(), r.Dy())/2)
	cx := max(r.Min.X+rad, min(x, r.Max.X-rad-1))
	cy := max(r.Min.Y+rad, min(y, r.Max.Y-rad-1))
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= rad*rad
}
func (b *Buffer) rounded(rect image.Rectangle, radius int, color uint32) {
	rect = rect.Intersect(image.Rect(0, 0, b.Width, b.Height))
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			if roundedInside(rect, x, y, radius) {
				b.point(x, y, color)
			}
		}
	}
}
func (b *Buffer) clippedText(x, y int, s string, scale int, color uint32, clip image.Rectangle) {
	for _, c := range s {
		g, ok := glyph[c]
		if !ok {
			g = glyph['?']
		}
		for row, bits := range g {
			for col := 0; col < 5; col++ {
				if bits&(1<<(4-col)) != 0 {
					r := image.Rect(x+col*scale, y+row*scale, x+(col+1)*scale, y+(row+1)*scale).Intersect(clip)
					for py := r.Min.Y; py < r.Max.Y; py++ {
						for px := r.Min.X; px < r.Max.X; px++ {
							b.point(px, py, color)
						}
					}
				}
			}
		}
		x += 6 * scale
	}
}
func (b *Buffer) GlassMenu(lines []string, scroll int) (GlassLayout, error) {
	return b.glassMenu(lines, scroll, nil, false)
}

// GlassMenuSource is the production path. A nil source means no signal and a
// true black AMOLED backdrop; a frame produces local blur without FB readback.
func (b *Buffer) GlassMenuSource(lines []string, scroll int, source *media.Image) (GlassLayout, error) {
	return b.glassMenu(lines, scroll, source, true)
}

func (b *Buffer) glassMenu(lines []string, scroll int, source *media.Image, sourceBacked bool) (GlassLayout, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return GlassLayout{}, os.ErrClosed
	}
	if e := b.Validate(); e != nil {
		return GlassLayout{}, e
	}
	for _, line := range lines {
		if !utf8.ValidString(line) || len(line) > 8192 {
			return GlassLayout{}, fmt.Errorf("invalid/oversized menu line")
		}
	}
	l := LayoutGlass(b.Width, b.Height, lines, scroll)
	b.bootOrbit = false
	if b.menuStatus.PowerOnly {
		l = LayoutPower(b.Width, b.Height, lines)
	} else if b.menuStatus.Statistics {
		l = LayoutStatistics(b.Width, b.Height, lines, scroll)
	} else if b.preview != nil {
		l = LayoutGlassPreview(b.Width, b.Height, lines, scroll, true, b.preview.options)
	}
	applyGlassDetails(&l, b.menuStatus)
	if l.Scale == 0 {
		return l, fmt.Errorf("menu geometry rejected")
	}
	if b.hardwareGlassUnsafe {
		return l, fimg2d.ErrComposerPoisoned
	}
	if b.s7BootVisual {
		if b.file == nil || b.scanout == nil {
			return l, fmt.Errorf("native GPU display unavailable; CPU drawing disabled")
		}
		if used, err := b.hardwareGlassMenu(lines, source, l); used {
			return l, err
		}
		return l, fmt.Errorf("native GPU menu declined scene; CPU drawing disabled")
	}
	if sc := b.glass; sc != nil && !b.menuStatusDirty && sc.sourceBacked == sourceBacked && slices.Equal(sc.lines, lines) && sc.layout.Panel == l.Panel && sc.layout.Scroll == l.Scroll && (!sc.hasPending || sc.sourceBacked) {
		return l, nil
	}
	var mapped []byte
	if sourceBacked {
		mapped = b.beginMenuStage()
		defer func() {
			if mapped != nil {
				b.Data = mapped
			}
		}()
	}
	b.restoreFeedback()
	if b.glass != nil && b.glass.sourceBacked != sourceBacked {
		return l, fmt.Errorf("menu backdrop mode changed while open")
	}
	var beginErr error
	if sourceBacked {
		beginErr = b.beginGlassSource(source)
	} else {
		beginErr = b.beginGlass()
	}
	if beginErr != nil {
		return l, beginErr
	}
	sc := b.glass
	linesMatch := slices.Equal(sc.lines, lines)
	if b.menuStatusDirty && !l.Statistics {
		linesMatch = sameGlassStructure(sc.lines, lines)
	}
	sameStructure := sc.ink != nil && linesMatch && sc.layout.Panel == l.Panel && sc.layout.Scroll == l.Scroll
	if l.Statistics && !sameStructure {
		sc.cachedRaw = nil
	}
	firstStage := sc.sourceBacked && !sc.stageReady
	overlayRefresh := sc.sourceBacked && (b.preview != nil || b.feedback != nil)
	cacheReady := sc.cachedPanel == l.Panel && len(sc.panelPacked) == l.Panel.Dx()*l.Panel.Dy()
	dirty, copyRect := l.Panel, l.Panel
	paintHeader := true
	if sc.sourceBacked && !firstStage && !overlayRefresh && sameStructure && cacheReady {
		dirty = glassStatusDirty(l, b.menuPreviousStatus, b.menuStatus, b.menuForegroundDirty, b.menuCategoryDirty, b.menuHeaderDirty)
		copyRect = dirty
		paintHeader = b.menuHeaderDirty
		if dirty.Empty() {
			b.Data = mapped
			mapped = nil
			return l, nil
		}
	}
	var flushedAt time.Time
	if sc.hasPending && !sc.sourceBacked && !time.Now().Before(sc.nextLive) {
		flushedAt = time.Now()
		if sc.sourceBacked {
			sc.storeBackdrop(sc.pendingImage())
			b.sampleImageBackdrop(sc, sc.pendingImage())
		} else {
			if err := b.writeImageLocked(sc.pendingImage()); err != nil {
				return l, err
			}
			b.updateGlassSource(sc)
		}
		sc.hasPending = false
	}
	sc.previewDocked = b.preview != nil
	if sc.sourceBacked {
		if firstStage || overlayRefresh {
			copyRect = image.Rect(0, 0, b.Width, b.Height)
			if sc.hasBackdrop {
				if err := b.writeImageLocked(sc.backdropImage()); err != nil {
					return l, err
				}
			} else {
				b.clearVisibleLocked()
			}
		}
	} else {
		copy(b.Data, sc.raw)
	}
	if b.preview != nil {
		b.preview.backup = b.preview.backup[:0]
		b.preview.docked = true
	}
	if !sameStructure || b.menuForegroundDirty {
		sc.ink = newGlassInk(l.Panel)
		b.inkTarget = sc.ink
		b.rasterGlassForeground(lines, l)
		b.inkTarget = nil
		sc.lines = append(sc.lines[:0], lines...)
		sc.layout = l
		b.liveStats.ForegroundBuilds++
	}
	if sc.sourceBacked {
		b.paintGlassBackdropRegion(sc, l, dirty, !sameStructure || b.menuForegroundDirty || firstStage || overlayRefresh)
	} else {
		b.paintGlassBackdrop(sc, l)
	}
	if paintHeader && !l.Statistics && !l.PowerOnly {
		b.paintGlassStatus(l)
	}
	b.drawPreview()
	b.invalidateFeedbackBackdrop()
	b.drawFeedback()
	if sourceBacked {
		if stageErr := b.finishMenuStageRegion(mapped, copyRect); stageErr != nil {
			return l, stageErr
		}
		mapped = nil
	}
	err := b.commit()
	if err == nil {
		b.menuStatusDirty = false
		b.menuForegroundDirty = false
		b.menuCategoryDirty = false
		b.menuHeaderDirty = false
		b.menuPreviousStatus = b.menuStatus
		sc.stageReady = sc.sourceBacked
		sc.lastDirty, sc.lastCopy = dirty, copyRect
	}
	if !flushedAt.IsZero() {
		cost := time.Since(flushedAt)
		sc.nextLive = time.Now().Add(max(liveGlassMinimumGap, cost))
		b.liveStats.LastComposeUS = cost.Microseconds()
		b.liveStats.MaxComposeUS = max(b.liveStats.MaxComposeUS, cost.Microseconds())
		if err == nil {
			b.liveStats.PendingFlushes++
		}
	}
	return l, err
}
func (b *Buffer) rasterGlassForeground(lines []string, l GlassLayout) {
	if l.PowerOnly {
		b.rasterPower(l)
		return
	}
	if l.Statistics {
		b.rasterStatistics(l)
		return
	}
	ink, muted, gold := uint32(0xd9f2f2), uint32(0x85acb4), uint32(0xffc857)
	b.glassOutline(l.Panel, max(1, l.Scale), gold, 225, l.Panel)
	brandFont := fontSize(l.TitleFont * 5 / 4)
	b.smoothText(l.Rotate.Max.X+l.Pad, l.Top.Min.Y, "S7", brandFont, ink, l.Top)
	stroke := max(1, l.Scale)
	b.glassCard(image.Rect(l.Panel.Min.X, l.Top.Max.Y-stroke, l.Panel.Max.X, l.Top.Max.Y), 0, gold, 185, l.Panel)
	b.glassCard(image.Rect(l.Left.Max.X, l.Left.Min.Y, l.Left.Max.X+stroke, l.Left.Max.Y), 0, gold, 135, l.Panel)
	b.glassCard(image.Rect(l.Right.Min.X, l.Right.Min.Y, l.Right.Min.X+stroke, l.Right.Max.Y), 0, gold, 135, l.Panel)
	for i, category := range l.Categories {
		clip := category.Rect.Intersect(l.Left)
		if clip.Empty() {
			continue
		}
		color := muted
		if i == b.menuStatus.Selected {
			color = gold
			b.glassOutline(category.IconRect, max(1, l.Scale), gold, 225, clip)
		}
		b.glassCategoryIcon(category.Kind, category.IconRect.Inset(max(2, 2*l.Scale)), color, max(1, l.Scale))
		font := fontSize(max(12, l.Font*2/3))
		for _, line := range category.Lines {
			x := (category.TextRect.Min.X + category.TextRect.Max.X - textWidth(line, font)) / 2
			b.smoothText(x, category.TextRect.Min.Y, line, font, color, clip)
		}
	}
	for i, title := range l.Title {
		b.smoothText(l.MiddleTitleRect.Min.X, l.MiddleTitleRect.Min.Y+i*l.LineHeight, title, l.Font, gold, l.MiddleTitleRect)
	}
	for _, row := range l.Rows {
		clip := row.Rect.Intersect(l.Body)
		if clip.Empty() {
			continue
		}
		if row.Index == b.menuStatus.Focused {
			b.glassOutline(row.Rect.Inset(max(1, l.Scale)).Intersect(l.Body), max(1, l.Scale), gold, 225, clip)
		}
		color := ink
		if strings.HasPrefix(lines[row.Index], "BACK:") {
			color = gold
		}
		for j, line := range row.Lines {
			b.smoothText(row.TextRect.Min.X, row.TextRect.Min.Y+j*l.LineHeight, line, l.Font, color, l.Body.Intersect(row.TextRect))
		}
	}
	titleFont := fontSize(max(12, l.Font*4/5))
	for j, line := range l.DetailTitle {
		b.smoothText(l.DetailTitleRect.Min.X, l.DetailTitleRect.Min.Y+j*titleFont*5/4, line, titleFont, gold, l.DetailTitleRect)
	}
	detailFont := fontSize(max(12, l.Font*4/5))
	detailLineHeight := detailFont * 5 / 4
	for i, row := range l.Details {
		clip := row.Rect.Intersect(l.Right)
		if clip.Empty() {
			continue
		}
		if row.Kind == "slider" {
			const segments = 24
			gap := max(2, l.Scale)
			full := row.Control
			level := max(0, min(row.Maximum-row.Minimum, row.Value-row.Minimum))
			fillTop := full.Max.Y - full.Dy()*level/max(1, row.Maximum-row.Minimum)
			for segment := 0; segment < segments; segment++ {
				bar := image.Rect(full.Min.X, full.Min.Y+segment*full.Dy()/segments+gap/2,
					full.Max.X, full.Min.Y+(segment+1)*full.Dy()/segments-gap/2)
				b.glassCard(bar, max(1, l.Scale), 0xbabfc7, 48, clip)
				lit := bar
				lit.Min.Y = max(lit.Min.Y, fillTop)
				if !lit.Empty() {
					b.glassCard(lit, max(1, l.Scale), gold, 235, clip)
				}
			}
			continue
		}
		selected := b.menuStatus.DetailHasSelection && i == b.menuStatus.DetailSelected
		if selected {
			b.glassOutline(row.Rect.Inset(max(1, l.Scale)), max(1, l.Scale), gold, 225, clip)
		}
		color := muted
		if selected {
			color = gold
		}
		for j, line := range row.Lines {
			b.smoothText(row.TextRect.Min.X, row.TextRect.Min.Y+j*detailLineHeight, line, detailFont, color, clip)
		}
	}
	hintFont := fontSize(l.Font * 3 / 4)
	hintLine := max(1, hintFont*5/4)
	for j, line := range l.DetailHint {
		b.smoothText(l.DetailHintRect.Min.X, l.DetailHintRect.Min.Y+j*hintLine, line, hintFont, muted, l.DetailHintRect)
	}
	up, down := uint32(0x948d82), uint32(0x948d82)
	if l.Scroll > 0 {
		up = ink
	}
	if l.Scroll < l.Maximum {
		down = ink
	}
	b.glassChevron(l.Up, false, up, l.Scale)
	b.glassChevron(l.Down, true, down, l.Scale)
}

func (b *Buffer) rasterStatistics(l GlassLayout) {
	gold, ink, muted := uint32(0xffc857), uint32(0xd9f2f2), uint32(0x85acb4)
	b.glassOutline(l.Panel, max(1, l.Scale), gold, 225, l.Panel)
	for i, title := range l.Title {
		b.smoothText(l.MiddleTitleRect.Min.X, l.MiddleTitleRect.Min.Y+i*l.TitleFont*5/4, title, l.TitleFont, gold, l.MiddleTitleRect)
	}
	b.glassOutline(l.Back, max(1, l.Scale), gold, 225, l.Top)
	b.smoothText(l.Back.Min.X+l.Pad, l.Back.Min.Y+l.Pad/2, "Back", l.Font, ink, l.Back)
	stroke := max(1, l.Scale)
	b.glassCard(image.Rect(l.Panel.Min.X, l.Top.Max.Y-stroke, l.Panel.Max.X, l.Top.Max.Y), 0, gold, 185, l.Panel)
	for _, row := range l.Rows {
		clip := row.Rect.Intersect(l.Body)
		if clip.Empty() {
			continue
		}
		for i, label := range row.Lines {
			b.smoothText(row.TextRect.Min.X, row.TextRect.Min.Y+i*l.LineHeight, label, l.Font, muted, clip.Intersect(row.TextRect))
		}
		for i, value := range row.ValueLines {
			b.smoothText(row.ValueRect.Min.X, row.ValueRect.Min.Y+i*l.LineHeight, value, l.Font, ink, clip.Intersect(row.ValueRect))
		}
	}
	up, down := uint32(0x948d82), uint32(0x948d82)
	if l.Scroll > 0 {
		up = ink
	}
	if l.Scroll < l.Maximum {
		down = ink
	}
	b.glassChevron(l.Up, false, up, l.Scale)
	b.glassChevron(l.Down, true, down, l.Scale)
}

func (b *Buffer) glassCategoryIcon(kind string, rect image.Rectangle, color uint32, stroke int) {
	segments := icon(kind)
	if kind == "rotate" {
		segments = []segment{{2, 5, 11, 5}, {11, 5, 9, 3}, {11, 5, 9, 7}, {14, 11, 5, 11}, {5, 11, 7, 9}, {5, 11, 7, 13}}
	}
	for _, seg := range segments {
		x1, y1 := rect.Min.X+seg.X1*rect.Dx()/16, rect.Min.Y+seg.Y1*rect.Dy()/16
		x2, y2 := rect.Min.X+seg.X2*rect.Dx()/16, rect.Min.Y+seg.Y2*rect.Dy()/16
		if b.gpuTarget != nil {
			b.gpuTarget.add(gpumenu.Line, rect, color, 255, x1, y1, x2, y2, stroke)
			continue
		}
		steps := max(1, max(abs(x2-x1), abs(y2-y1)))
		for i := 0; i <= steps; i++ {
			x, y := x1+(x2-x1)*i/steps, y1+(y2-y1)*i/steps
			for dy := 0; dy < stroke; dy++ {
				for dx := 0; dx < stroke; dx++ {
					if image.Pt(x+dx, y+dy).In(rect) {
						b.blendPoint(x+dx, y+dy, color, 255)
					}
				}
			}
		}
	}
}

func (b *Buffer) glassChevron(rect image.Rectangle, down bool, color uint32, scale int) {
	cx, cy := (rect.Min.X+rect.Max.X)/2, (rect.Min.Y+rect.Max.Y)/2
	size := max(4, min(rect.Dy()/3, b.Width/180))
	if b.gpuTarget != nil {
		edge, point := cy+size/2, cy-size/2
		if down {
			edge, point = cy-size/2, cy+size/2
		}
		b.gpuTarget.add(gpumenu.Line, rect, color, 215, cx-size, edge, cx, point, max(1, scale))
		b.gpuTarget.add(gpumenu.Line, rect, color, 215, cx+size, edge, cx, point, max(1, scale))
		return
	}
	for i := 0; i < size; i++ {
		y := cy + size/2 - i
		if down {
			y = cy - size/2 + i
		}
		for t := 0; t < max(1, scale); t++ {
			b.blendPoint(cx-size+i, y+t, color, 215)
			b.blendPoint(cx+size-i, y+t, color, 215)
		}
	}
}

func (b *Buffer) paintGlassStatus(l GlassLayout) {
	gold := uint32(0x7fdbda)
	rotateColor := uint32(0xbabfc7)
	if b.menuStatus.AutoRotate {
		rotateColor = 0xffc857
		b.glassCard(l.Rotate, 0, rotateColor, 48, l.Top)
	}
	b.glassOutline(l.Rotate, max(1, l.Scale), rotateColor, 225, l.Top)
	b.glassCategoryIcon("rotate", l.Rotate.Inset(max(3, 2*l.Scale)), rotateColor, max(1, l.Scale))
	statusClip := image.Rect(l.Rotate.Max.X+l.Pad, l.Top.Min.Y, l.Top.Max.X, l.Top.Max.Y).Intersect(l.Top)
	if !l.Install.Empty() {
		statusClip.Max.X = l.Install.Min.X - l.Pad
	}
	brandFont := fontSize(l.TitleFont * 5 / 4)
	status := []string{b.menuStatus.Mode, b.menuStatus.Rate, b.menuStatus.Codec, b.menuStatus.Battery, b.menuStatus.Temperature, b.menuStatus.Load}
	small := fontSize(l.Font * 3 / 4)
	startX := statusClip.Min.X + textWidth("S7", brandFont) + l.Pad*2
	for small > 12 {
		width := 0
		for _, v := range status {
			if v != "" {
				width += textWidth(displayLabel(v), small) + l.Pad
			}
		}
		if width <= statusClip.Max.X-startX {
			break
		}
		small = fontSize(small - 4)
	}
	x := startX
	y := l.Top.Min.Y + l.Font/3
	statusLineH := small*5/4 + l.Scale
	for _, value := range status {
		if value == "" {
			continue
		}
		label := displayLabel(value)
		if x > startX && x+textWidth(label, small) > statusClip.Max.X {
			x = startX
			y += statusLineH
		}
		b.smoothText(x, y, label, small, gold, statusClip)
		x += textWidth(label, small) + l.Pad
	}
	x = startX
	y += statusLineH + l.Scale
	size := max(16, l.Font)
	for _, item := range b.menuStatus.Functions {
		if item.Kind == "" {
			continue
		}
		color := uint32(0xbabfc7)
		if item.State == Active {
			color = 0xb6ef72
		} else if item.State == Off {
			color = 0x516568
		}
		b.glassCategoryIcon(item.Kind, image.Rect(x, y, x+size, y+size).Intersect(statusClip), color, max(1, l.Scale))
		x += size + l.Pad
	}
	b.smoothText(x+l.Pad, y, displayLabel(b.menuStatus.USB), small, gold, statusClip)
	if !l.Install.Empty() {
		r := l.Install
		labels := []string{b.menuStatus.InstallStatus}
		if b.menuStatus.InstallAction != "" {
			labels = append(labels, b.menuStatus.InstallAction)
			b.glassCard(r, 0, 0, 230, l.Top)
			b.glassOutline(r, max(1, l.Scale/2), 0xffd166, 255, l.Top)
		}
		inner := r.Inset(max(2, l.Scale))
		for i, text := range labels {
			line := image.Rect(inner.Min.X, inner.Min.Y+i*inner.Dy()/len(labels), inner.Max.X, inner.Min.Y+(i+1)*inner.Dy()/len(labels))
			size := fontSize(max(12, line.Dy()*3/4))
			label := displayLabel(text)
			for size > 12 && textWidth(label, size) > line.Dx()-8 {
				size = fontSize(size - 4)
			}
			b.smoothText(line.Min.X+(line.Dx()-textWidth(label, size))/2, line.Min.Y+(line.Dy()-size)/2, label, size, 0xffd166, line)
		}
	}
}

func (b *Buffer) glassOutline(rect image.Rectangle, width int, rgb uint32, alpha int, clip image.Rectangle) {
	for _, edge := range []image.Rectangle{
		image.Rect(rect.Min.X, rect.Min.Y, rect.Max.X, rect.Min.Y+width),
		image.Rect(rect.Min.X, rect.Max.Y-width, rect.Max.X, rect.Max.Y),
		image.Rect(rect.Min.X, rect.Min.Y, rect.Min.X+width, rect.Max.Y),
		image.Rect(rect.Max.X-width, rect.Min.Y, rect.Max.X, rect.Max.Y),
	} {
		b.glassCard(edge, 0, rgb, alpha, clip)
	}
}

func (b *Buffer) EndGlass() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return os.ErrClosed
	}
	if b.glass == nil {
		return nil
	}
	if b.hardwareGlassUnsafe {
		return fimg2d.ErrComposerPoisoned
	}
	b.discardFeedback()
	sc := b.glass
	if b.s7BootVisual && b.scanout != nil && b.scanout.UsesLayers() {
		b.glass = nil
		b.layerMenuFD = 0
		return b.commitLayersLocked(nil)
	}
	if b.menuComposer != nil {
		if err := b.menuComposer.Close(); err != nil {
			return err
		}
		b.menuComposer = nil
		b.menuComposerRect = image.Rectangle{}
		b.glass = nil
		if b.scanout != nil {
			if b.menuVideo != nil {
				fd, err := b.scanout.CopyToBack(int(b.menuVideo.Fd()))
				if err != nil {
					return err
				}
				b.directFrameFD = fd
				b.directFrameReady = true
			}
		} else {
			b.clearVisibleLocked()
		}
		if b.preview != nil {
			b.preview.backup = b.preview.backup[:0]
			b.preview.docked = false
		}
		b.drawPreview()
		b.drawIndicators()
		b.drawFeedback()
		return b.commit()
	}
	var mapped []byte
	if sc.sourceBacked {
		mapped = b.beginMenuStage()
		defer func() {
			if mapped != nil {
				b.Data = mapped
			}
		}()
	}
	if sc.sourceBacked {
		if sc.hasBackdrop {
			if err := b.writeImageLocked(sc.backdropImage()); err != nil {
				return err
			}
		} else {
			b.clearVisibleLocked()
		}
	} else {
		copy(b.Data, sc.raw)
	}
	b.glass = nil
	if sc.hasPending {
		if err := b.writeImageLocked(sc.pendingImage()); err != nil {
			return err
		}
		b.liveStats.CloseRefreshes++
	}
	if b.preview != nil {
		b.preview.backup = b.preview.backup[:0]
		b.preview.docked = false
	}
	b.drawPreview()
	b.drawIndicators()
	b.invalidateFeedbackBackdrop()
	b.drawFeedback()
	if sc.sourceBacked {
		if err := b.finishMenuStage(mapped); err != nil {
			return err
		}
		mapped = nil
	}
	return b.commit()
}

func roundCoverage(rect image.Rectangle, x, y int, radius float64) int {
	if rect.Empty() {
		return 0
	}
	r := math.Min(radius, float64(min(rect.Dx(), rect.Dy()))/2)
	cx, cy := float64(rect.Min.X+rect.Max.X)/2, float64(rect.Min.Y+rect.Max.Y)/2
	qx := math.Abs(float64(x)+.5-cx) - (float64(rect.Dx())/2 - r)
	qy := math.Abs(float64(y)+.5-cy) - (float64(rect.Dy())/2 - r)
	dist := math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
	return max(0, min(255, int((.5-dist)*255)))
}
func (b *Buffer) glassCard(rect image.Rectangle, radius int, rgb uint32, alpha int, clip image.Rectangle) {
	bounds := rect.Intersect(clip).Intersect(image.Rect(0, 0, b.Width, b.Height))
	if b.gpuTarget != nil {
		b.gpuTarget.add(gpumenu.Rectangle, bounds, rgb, alpha, rect.Min.X, rect.Min.Y, rect.Max.X, rect.Max.Y, radius)
		return
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			b.blendPoint(x, y, rgb, alpha*roundCoverage(rect, x, y, float64(radius))/255)
		}
	}
}
func sampleBackdrop(sc *glassScene, x, y, w, h int) uint32 {
	return sampleGlassPixels(sc.rgb, sc.width, sc.height, x, y, w, h)
}
func sampleGlassPixels(pixels []uint32, width, height, x, y, w, h int) uint32 {
	if width <= 0 || height <= 0 || len(pixels) != width*height {
		return 0
	}
	// Fixed-point bilinear sampling; edge coordinates never escape the snapshot.
	sx, sy := x*(width-1)*256/max(1, w-1), y*(height-1)*256/max(1, h-1)
	x0, y0 := sx/256, sy/256
	x1, y1 := min(x0+1, width-1), min(y0+1, height-1)
	fx, fy := sx%256, sy%256
	a, b, c, d := pixels[y0*width+x0], pixels[y0*width+x1], pixels[y1*width+x0], pixels[y1*width+x1]
	out := uint32(0)
	for _, shift := range []uint{0, 8, 16} {
		v := (int(a>>shift&255)*(256-fx)*(256-fy) + int(b>>shift&255)*fx*(256-fy) + int(c>>shift&255)*(256-fx)*fy + int(d>>shift&255)*fx*fy + 32768) / 65536
		out |= uint32(v) << shift
	}
	return out
}

// Keep source colour/detail under a neutral black glass layer. Gold belongs to
// the frame and selection, not a brown wash over the whole AMOLED menu.
func champagneTint(p uint32) uint32 {
	r := int(p>>16&255) * 90 / 255
	g := int(p>>8&255) * 90 / 255
	b := int(p&255) * 90 / 255
	peak := max(r, max(g, b))
	if peak > 84 {
		target := 84 + (peak-84)/8
		r, g, b = r*target/peak, g*target/peak, b*target/peak
	}
	return uint32(r)<<16 | uint32(g)<<8 | uint32(b)
}

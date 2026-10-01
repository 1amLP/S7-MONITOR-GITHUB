package appliance

import (
	"perimode/native/internal/fb"
	"perimode/native/pkg/hid"
	"image"
	"slices"
	"strings"
	"time"
)

// menuInput maps taps and scrolling to the exact wrapped glass row geometry.
// Multi-finger input or a drag cancels the tap; it never reaches Windows HID.
func (u *UI) menuInput(contacts []hid.Contact) {
	if u.screen == nil {
		return
	}
	w, h, _ := u.screen.Geometry()
	lines, l := u.menuLines, u.menuLayout
	if l.Scale == 0 || len(lines) == 0 {
		u.inputTiming.discarded(inputNoMenuLayout)
		return
	}
	if len(contacts) > 1 {
		u.menuSlider = 0
		u.touchDown = true
		u.menuDragged = true
		u.menuMultitouch = true
		u.touchRow = -1
		return
	}
	if len(contacts) == 1 && !u.menuMultitouch {
		c := contacts[0]
		hit := l.Hit(w, h, c.X, c.Y)
		if !u.touchDown {
			u.touchDown = true
			u.menuDragged = false
			u.touchRow = hit
			u.menuStartX, u.menuStartY = c.X, c.Y
			u.menuStartTime = time.Now()
			u.menuHoldFired = false
			u.menuStartScroll = l.Scroll
			u.menuScrollGesture = image.Pt(int(c.X)*(w-1)/32767, int(c.Y)*(h-1)/32767).In(l.Body)
			u.menuTapBounds, u.menuTapLabel = menuTarget(l, lines, hit)
			if value, ok := l.SliderValue(hit, w, h, c.X, c.Y); ok {
				if !u.renderedDetailCurrent() {
					u.menuDragged, u.touchRow = true, -1
					u.draw()
					return
				}
				u.menuSlider = hit
				u.menuDragged = true
				u.applyDetailSlider(value)
			}
		} else if u.menuSlider > 0 {
			if !u.renderedDetailCurrent() {
				u.menuSlider, u.touchRow = 0, -1
				u.menuDragged = true
				u.draw()
				return
			}
			if value, ok := l.SliderValue(u.menuSlider, w, h, c.X, c.Y); ok {
				u.applyDetailSlider(value)
			}
		} else if u.menuHoldFired {
			return
		} else {
			dy := (int(u.menuStartY) - int(c.Y)) * h / 32767
			dx := (int(u.menuStartX) - int(c.X)) * w / 32767
			// Scale the finger-motion allowance with the panel, not raw pixels.
			threshold := max(8, min(w, h)*8/360)
			// A held header button has no scroll gesture. Keep it armed while
			// the finger remains inside the same button, even if it moves.
			if u.touchRow != fb.GlassRotate && (absMenu(dy) > threshold || absMenu(dx) > threshold) {
				u.menuDragged = true
				u.touchRow = -1
			}
			if u.menuDragged && u.menuScrollGesture {
				next := max(0, min(l.Maximum, u.menuStartScroll+dy))
				if next != u.menuScroll {
					u.menuScroll = next
					u.menuNeedsDraw = true
				}
			} else if hit != u.touchRow {
				u.touchRow = -1
			}
		}
	}
	if len(contacts) == 0 && u.touchDown {
		row := u.touchRow
		held := u.menuHoldFired
		u.menuHoldFired = false
		wasSlider := u.menuSlider > 0
		u.menuSlider = 0
		dragged := u.menuDragged || u.menuMultitouch
		multitouch := u.menuMultitouch
		u.menuMultitouch = false
		nowRect, nowLabel := menuTarget(l, lines, row)
		targetChanged := nowRect != u.menuTapBounds || nowLabel != u.menuTapLabel
		if targetChanged {
			dragged = true
		}
		u.touchDown = false
		u.menuDragged = false
		u.touchRow = -1
		if held {
			return
		}
		if dragged {
			if !wasSlider {
				u.inputTiming.menuTapCancelled(multitouch, row < 0 || !targetChanged)
			}
			u.flushMenuScroll()
			return
		}
		if nowLabel == "" || nowRect == [4]int{} {
			return
		}
		if row == fb.GlassRotate {
			u.inputTiming.menuTapAccepted()
			u.rotateFromMenu(time.Since(u.menuStartTime) >= menuHoldDelay)
			return
		}
		// Feedback follows a real action, not the initial contact or a cancelled tap.
		_, _, oldPage := u.state.Current()
		if row >= fb.GlassDetailBase && !u.renderedDetailCurrent() {
			u.inputTiming.menuTapCancelled(false, false)
			u.draw()
			return
		}
		if row >= fb.GlassDetailBase && !u.menuDetail(oldPage, u.menuFocus).Action {
			return
		}
		u.inputTiming.menuTapAccepted()
		oldFocus, oldPending := u.menuFocus, u.pendingAction
		oldLines := u.lines()
		defer func() {
			_, _, newPage := u.state.Current()
			if oldPage != newPage || oldFocus != u.menuFocus || oldPending != u.pendingAction || (!isCamera3APage(oldPage) && !slices.Equal(oldLines, u.lines())) {
				u.feedback()
			}
		}()
		if oldPage == "POWER_MENU" {
			u.selectPowerMenu(row)
			return
		}
		if row == fb.GlassUp || row == fb.GlassDown {
			delta := l.Body.Dy() * 3 / 4
			if row == fb.GlassUp {
				delta = -delta
			}
			u.menuScroll = max(0, min(l.Maximum, l.Scroll+delta))
			u.menuNeedsDraw = true
			u.flushMenuScroll()
		} else if row >= fb.GlassDetailBase {
			u.selectDetail(row - fb.GlassDetailBase)
		} else if row == fb.GlassStatisticsBack {
			_, _, page := u.state.Current()
			if statisticsPage(page) {
				u.menu(u.parentMenu(page))
			}
		} else if row == fb.GlassInstallDisk {
			u.activateUSBHeader(nowLabel)
		} else if row >= fb.GlassCategoryBase {
			u.selectCategory(row - fb.GlassCategoryBase)
		} else if row >= 0 {
			u.middleSelection(row)
		}
	}
}

// A worker can change the control list before its new frame is painted. Do not
// reinterpret an old Reset button or slider as the new control at the same index.
func (u *UI) renderedDetailCurrent() bool {
	_, _, page := u.state.Current()
	p := u.menuPaintStatus
	if page != u.menuPaintPage || p.Focused != u.menuFocus {
		return false
	}
	d := u.menuDetail(page, u.menuFocus)
	if p.DetailTitle != d.Title || p.DetailAction != d.Action || p.DetailSlider != d.Slider {
		return false
	}
	if d.Slider {
		lo, hi, step := d.SliderMin, d.SliderMax, d.SliderStep
		if hi == 0 {
			lo, hi, step = 50, 200, 5
		}
		return p.DetailSliderMin == lo && p.DetailSliderMax == hi && p.DetailSliderStep == step
	}
	count := min(len(d.Options), len(p.DetailOptions))
	if p.DetailCount != count {
		return false
	}
	return slices.Equal(p.DetailOptions[:count], d.Options[:count])
}

const menuHoldDelay = 650 * time.Millisecond

// Called by the UI event loop, including its idle tick. A stationary contact
// need not produce another evdev event for a long press to take effect.
func (u *UI) updateMenuHold(now time.Time) {
	if !u.touchDown || u.touchRow != fb.GlassRotate || u.menuHoldFired ||
		u.menuDragged || u.menuMultitouch || u.inputAwaitAllUp ||
		now.Sub(u.menuStartTime) < menuHoldDelay {
		return
	}
	_, _, page := u.state.Current()
	rect, label := menuTarget(u.menuLayout, u.menuLines, fb.GlassRotate)
	if page == "" || page == "POWER_MENU" || rect == [4]int{} ||
		rect != u.menuTapBounds || label != u.menuTapLabel {
		return
	}
	u.menuHoldFired = true
	u.inputTiming.menuTapAccepted()
	u.rotateFromMenu(true)
}

func (u *UI) selectPowerMenu(row int) {
	if row < 1 || row > 3 {
		return
	}
	action := MachineAction(row)
	if u.pendingAction == action && time.Now().Before(u.powerConfirm) {
		u.confirmPower(time.Now())
	} else {
		u.requestPower(action)
	}
}

func (u *UI) middleSelection(row int) {
	_, _, page := u.state.Current()
	if statisticsPage(page) {
		return
	}
	lines := u.lines()
	if row <= 0 || row >= len(lines) {
		return
	}
	if u.middleOnlyRow(page, row, lines) {
		u.selectRow(row)
		_, _, target := u.state.Current()
		if target != page && target != "" && !strings.HasPrefix(lines[row], "BACK:") && !strings.HasPrefix(lines[row], "CONFIRM") {
			if u.menuParents == nil {
				u.menuParents = make(map[string]string)
			}
			u.menuParents[target] = page
		}
		return
	}
	u.focusRow(row)
}

func (u *UI) parentMenu(page string) string {
	if parent, ok := u.menuParents[page]; ok {
		return parent
	}
	return menuParent(page)
}

func (u *UI) flushMenuScroll() {
	if u.menuNeedsDraw {
		u.draw()
	}
}
func absMenu(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// A status line can reflow while the finger is down. Do not dispatch an action
// that moved or changed meaning beneath a stationary finger.
func menuTarget(l fb.GlassLayout, lines []string, row int) ([4]int, string) {
	r := l.Up
	label := "UP"
	if row == fb.GlassDown {
		r = l.Down
		label = "DOWN"
	} else if row == fb.GlassStatisticsBack {
		r = l.Back
		label = "BACK"
	} else if row == fb.GlassRotate {
		r = l.Rotate
		label = "ROTATE"
	} else if row == fb.GlassInstallDisk {
		r = l.Install
		label = l.InstallLabel
	} else if row >= fb.GlassDetailBase {
		for _, detail := range l.Details {
			if detail.Index == row {
				label := detail.Kind
				if len(detail.Lines) > 0 {
					label = detail.Lines[0]
				}
				return [4]int{detail.Rect.Min.X, detail.Rect.Min.Y, detail.Rect.Max.X, detail.Rect.Max.Y}, label
			}
		}
		return [4]int{}, ""
	} else if row >= fb.GlassCategoryBase {
		for _, category := range l.Categories {
			if category.Index == row {
				return [4]int{category.Rect.Min.X, category.Rect.Min.Y, category.Rect.Max.X, category.Rect.Max.Y}, category.Lines[0]
			}
		}
		return [4]int{}, ""
	} else if row != fb.GlassUp {
		if row <= 0 || row >= len(lines) || row > len(l.Rows) {
			return [4]int{}, ""
		}
		r = l.Rows[row-1].Rect
		label = lines[row]
	}
	return [4]int{r.Min.X, r.Min.Y, r.Max.X, r.Max.Y}, label
}

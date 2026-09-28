package appliance

import (
	"strings"

	"perimode/native/internal/fb"
	"perimode/native/pkg/hid"
)

type menuDetail struct {
	Title, Hint                      string
	Options                          []string
	Selected                         int
	HasSelected                      bool
	Action                           bool
	Slider                           bool
	SliderValue                      int
	SliderText                       string
	SliderMin, SliderMax, SliderStep int
}

func splitMenuLine(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, ":"); i >= 0 {
		return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+1:])
	}
	return raw, ""
}

func (u *UI) menuDetail(page string, focus int) menuDetail {
	if statisticsPage(page) {
		return menuDetail{}
	}
	lines := u.lines()
	if focus <= 0 || focus >= len(lines) {
		return menuDetail{Title: "SELECT A SETTING", Hint: "CHOOSE AN ITEM IN THE MIDDLE COLUMN"}
	}
	if strings.TrimSpace(lines[focus]) == "" {
		return menuDetail{}
	}
	label, value := splitMenuLine(lines[focus])
	if u.middleOnlyRow(page, focus, lines) {
		return menuDetail{Title: label}
	}
	if d, ok := u.deviceLeafDetail(page, focus); ok {
		return d
	}
	if d, ok := u.monitorLeafDetail(page, focus, label, value); ok {
		return d
	}
	if d, ok := u.audioLeafDetail(page, focus, label, value); ok {
		return d
	}
	if d, ok := u.touchLeafDetail(page, focus, label, value); ok {
		return d
	}
	if d, ok := u.functionMenuDetail(page, focus, label, value); ok {
		return d
	}
	if page == "INDICATOR_SETTINGS" {
		return u.indicatorMenuDetail(focus, label, value)
	}
	if d, ok := u.commandMenuDetail(page, focus, label, value); ok {
		return d
	}
	if value != "" {
		return informationDetail(label, value)
	}
	return menuDetail{Title: label}
}

func (u *UI) commandMenuDetail(page string, row int, label, value string) (menuDetail, bool) {
	command := ""
	switch page {
	case "SCREEN_ROTATION":
		switch row {
		case 1:
			command = "TOGGLE AUTOMATIC"
		case 2, 3, 4, 5:
			command = "SET " + value
		case 6:
			command = "LOCK CURRENT ORIENTATION"
		case 9:
			command = "RETRY SENSOR"
		}
	case "DISPLAY":
		switch row {
		case 2:
			command = "DIMMER"
		case 3:
			command = "BRIGHTER"
		case 4:
			command = "TOGGLE AUTOMATIC"
		}
	case "DISPLAY_AUTO":
		switch row {
		case 1:
			command = "TOGGLE AUTOMATIC"
		case 2, 3:
			command = "INCREASE LIMIT"
		case 4:
			command = "DECREASE BIAS"
		case 5:
			command = "INCREASE BIAS"
		case 9:
			command = "RETRY LIGHT SENSOR"
		}
	case "CAMERA_TRIAL":
		switch row {
		case 3:
			command = "STOP LOCAL TEST"
		}
	case "CAMERA_FOCUS", "CAMERA_EXPOSURE", "CAMERA_WB":
		return u.cameraControlDetail(page, row, label, value)
	}
	if command == "" {
		return menuDetail{}, false
	}
	return choicesDetail(label, []string{command}, -1), true
}

func (u *UI) applyInputMode(kind byte) {
	if kind != 0 && kind != hid.Touchscreen && kind != hid.Touchpad {
		return
	}
	if u.transport != nil && u.transport.touch != nil {
		u.transport.touch.blockInput()
	}
	u.pad.Reset()
	u.previewGesture = false
	u.cameraHUDDragging = false
	u.cameraHUDSelection = ""
	if u.lastInputCount > 0 {
		u.inputAwaitAllUp = true
	}
	u.suppressed = 0
	u.state.mu.Lock()
	if kind == 0 && u.state.TouchKind != 0 {
		u.state.TouchResumeKind = u.state.TouchKind
	}
	u.state.TouchKind = kind
	u.state.mu.Unlock()
	if kind == 0 {
		u.notify("INPUT OFF")
	} else if kind == hid.Touchpad {
		u.notify("PAD ON")
	} else {
		u.notify("TOUCH ON")
	}
	u.draw()
}

func (u *UI) applyInputEnabled(on bool) {
	u.state.mu.Lock()
	kind, resume := u.state.TouchKind, u.state.TouchResumeKind
	u.state.mu.Unlock()
	if !on {
		u.applyInputMode(0)
		return
	}
	if kind == 0 {
		kind = resume
	}
	if kind == 0 {
		kind = hid.Touchpad
	}
	u.applyInputMode(kind)
}

func (u *UI) applyPollingRate(hz int) {
	if !validPadPolling(hz) {
		return
	}
	if u.transport != nil && u.transport.touch != nil {
		if err := u.transport.touch.setPollingRate(hz); err != nil {
			u.state.Error(err)
			return
		}
	}
	u.state.mu.Lock()
	u.state.PadPollingHz = hz
	u.state.mu.Unlock()
	u.draw()
}

func (u *UI) applyPadSensitivity(value int) {
	u.state.mu.Lock()
	u.state.PadSensitivity = value
	u.state.mu.Unlock()
	u.draw()
}

func (u *UI) applyPadAcceleration(enabled bool) {
	u.state.mu.Lock()
	u.state.PadAcceleration = enabled
	u.state.mu.Unlock()
	u.draw()
}

func (u *UI) indicatorMenuDetail(row int, label, value string) menuDetail {
	u.state.mu.Lock()
	on, scale, corner := u.state.Indicators, u.state.IndicatorScale, u.state.Corner&3
	u.state.mu.Unlock()
	d := menuDetail{Title: label, Action: true}
	switch row {
	case 1:
		d.Options = []string{"OFF", "ON"}
		d.Selected, d.HasSelected = 0, true
		if on {
			d.Selected = 1
		}
		d.Hint = "SHOW OR HIDE STATUS INDICATORS"
	case 2:
		d.Slider, d.SliderValue = true, scale
		d.Action = false
		d.Hint = "DRAG TO SET INDICATOR SIZE"
	case 3:
		d.Options = []string{"TOP LEFT", "TOP RIGHT", "BOTTOM LEFT", "BOTTOM RIGHT"}
		d.Selected, d.HasSelected = corner, true
		d.Hint = "SELECT INDICATOR CORNER"
	case 4:
		d.Options, d.Hint = []string{"RESET TO 100%"}, "RESTORE DEFAULT SIZE"
	default:
		d.Action = false
		d.Options = []string{value}
		d.Selected, d.HasSelected = 0, value != ""
		d.Hint = "INFORMATION"
	}
	return d
}

func (u *UI) currentDetailStatus(page string) (string, string, [fb.GlassDetailLimit]string, int, int, bool, bool, int) {
	d := u.menuDetail(page, u.menuFocus)
	var options [fb.GlassDetailLimit]string
	count := min(len(d.Options), len(options))
	copy(options[:], d.Options[:count])
	selected, has := d.Selected, d.HasSelected && d.Selected >= 0 && d.Selected < count
	return d.Title, "", options, count, selected, has, d.Slider, d.SliderValue
}

func (u *UI) focusRow(row int) {
	lines := u.lines()
	if row <= 0 || row >= len(lines) || u.menuFocus == row {
		return
	}
	u.menuFocus = row
	u.draw()
}

func (u *UI) selectDetail(option int) {
	_, _, page := u.state.Current()
	if statisticsPage(page) {
		return
	}
	lines := u.lines()
	if u.middleOnlyRow(page, u.menuFocus, lines) {
		return
	}
	d := u.menuDetail(page, u.menuFocus)
	if !d.Action || option < 0 || option >= len(d.Options) {
		return
	}
	if u.selectDeviceDetail(page, u.menuFocus, option) {
		return
	}
	if u.selectMonitorLeaf(page, u.menuFocus, option) || u.selectAudioLeaf(page, u.menuFocus, option) {
		return
	}
	if u.selectTouchLeaf(page, u.menuFocus, option) {
		return
	}
	if u.selectFunctionDetail(page, u.menuFocus, option) {
		return
	}
	if page != "MONITOR" {
		if page == "INDICATOR_SETTINGS" {
			u.state.mu.Lock()
			switch u.menuFocus {
			case 1:
				u.state.Indicators = option == 1
			case 3:
				u.state.Corner = option
			case 4:
				u.state.IndicatorScale = 100
			default:
				u.state.mu.Unlock()
				return
			}
			u.state.mu.Unlock()
			u.draw()
			return
		}
		if option == 0 {
			u.applyDetailCommand(page, u.menuFocus)
		}
		return
	}
}

// These commands only adjust the focused setting or retry its worker.
// Navigation and confirmations that return to a parent have no entry here.
func (u *UI) applyDetailCommand(page string, row int) {
	switch page {
	case "SCREEN_ROTATION":
		u.selectRotation(row)
	case "DISPLAY":
		if row >= 2 && row <= 4 {
			u.selectDisplay(row)
		}
	case "DISPLAY_AUTO":
		u.selectAutoBrightness(row)
	case "CAMERA_FOCUS", "CAMERA_EXPOSURE", "CAMERA_WB":
		return
	case "CAMERA_TRIAL":
		if row == 3 {
			u.cancelCameraTrial()
			u.draw()
		}
	}
}

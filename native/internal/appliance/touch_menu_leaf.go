package appliance

import (
	"fmt"

	"perimode/native/pkg/hid"
)

func (u *UI) touchLeafLines(page string) ([]string, bool) {
	if page != "INPUT" && page != "TOUCH_STAT" {
		return nil, false
	}
	u.state.mu.Lock()
	kind, sensitivity, acceleration := u.state.TouchKind, u.state.PadSensitivity, u.state.PadAcceleration
	sensorStatus := u.state.TouchSensorRateStatus
	u.state.mu.Unlock()
	mode := "OFF"
	if kind == hid.Touchscreen {
		mode = "TOUCH"
	} else if kind == hid.Touchpad {
		mode = "PAD"
	}
	if page == "INPUT" {
		lines := []string{"TOUCH", fmt.Sprintf("ENABLED: %t", kind != 0)}
		if kind != 0 {
			lines = append(lines, "MODE: "+mode)
		}
		if kind == hid.Touchpad {
			lines = append(lines, fmt.Sprintf("SENSITIVITY: %d%%", sensitivity), fmt.Sprintf("ACCELERATION: %t", acceleration))
		}
		return append(lines, "STATISTICS", "BACK: CLOSE MENU"), true
	}
	input := u.inputTiming.snapshot()
	usb := TouchTimingSnapshot{}
	if u.transport != nil && u.transport.touch != nil {
		usb = u.transport.touch.touchTiming()
	}
	lines := []string{"Statistics", "FUNCTION: TOUCH", "MODE: " + mode}
	if kind != 0 && input.ReadFrames > 0 {
		lines = append(lines,
			fmt.Sprintf("INPUT FRAMES: %d", input.ReadFrames),
			fmt.Sprintf("ACTIVE FRAMES: %d", input.ActiveFrames))
	}
	if input.EvdevIntervals.Count > 0 {
		lines = append(lines, "EVENT INTERVAL: "+touchMeanUS(input.EvdevIntervals))
	}
	if input.ReadToUI.Count > 0 {
		lines = append(lines, "READ TO UI: "+touchMeanUS(input.ReadToUI))
	}
	if input.UIProcessing.Count > 0 {
		lines = append(lines, "UI PROCESSING: "+touchMeanUS(input.UIProcessing))
	}
	if kind != 0 && usb.WrittenReports > 0 {
		lines = append(lines, fmt.Sprintf("USB REPORTS: %d", usb.WrittenReports))
		if usb.WriteErrors > 0 {
			lines = append(lines, fmt.Sprintf("USB ERRORS: %d", usb.WriteErrors))
		}
		if kind == hid.Touchpad {
			lines = append(lines, fmt.Sprintf("COALESCED: %d", usb.CoalescedFrames), fmt.Sprintf("QUEUE PEAK: %d", usb.QueuePeak))
		}
	}
	if sensorStatus != "" && sensorStatus != "NOT REQUESTED" {
		lines = append(lines, "SENSOR REQUEST: "+sensorStatus)
	}
	return append(lines, "BACK: TOUCH"), true
}

func touchMeanUS(s InputStageTiming) string {
	if s.Count == 0 {
		return "N/A"
	}
	return fmt.Sprintf("%.1f MS", float64(s.TotalUS)/float64(s.Count)/1000)
}

func (u *UI) touchLeafDetail(page string, row int, label, value string) (menuDetail, bool) {
	if page != "INPUT" && page != "TOUCH_STAT" {
		return menuDetail{}, false
	}
	if page == "TOUCH_STAT" {
		return menuDetail{}, true
	}
	u.state.mu.Lock()
	kind, sensitivity, acceleration := u.state.TouchKind, u.state.PadSensitivity, u.state.PadAcceleration
	u.state.mu.Unlock()
	switch row {
	case 1:
		return switchDetail("ENABLED", kind != 0), true
	case 2:
		if kind == 0 {
			return informationDetail(label, value), true
		}
		selected := -1
		if kind == hid.Touchscreen || kind == hid.Touchpad {
			selected = int(kind) - 1
		}
		return choicesDetail("MODE", []string{"TOUCH", "PAD"}, selected), true
	case 3:
		if kind == hid.Touchpad {
			return sliderDetail("SENSITIVITY", sensitivity, 25, 300, 5), true
		}
	case 4:
		if kind == hid.Touchpad {
			return switchDetail("ACCELERATION", acceleration), true
		}
	}
	return informationDetail(label, value), true
}

func (u *UI) selectTouchLeaf(page string, row, option int) bool {
	if page == "TOUCH_STAT" {
		return true
	}
	if page != "INPUT" {
		return false
	}
	if row == 1 && option >= 0 && option < 2 {
		u.applyInputEnabled(option == 1)
		return true
	}
	u.state.mu.Lock()
	kind := u.state.TouchKind
	u.state.mu.Unlock()
	if row == 2 && kind != 0 && option >= 0 && option < 2 {
		u.applyInputMode(byte(option + 1))
		return true
	}
	if kind == hid.Touchpad && row == 4 && option >= 0 && option < 2 {
		u.applyPadAcceleration(option == 1)
		return true
	}
	return row == 3 && kind == hid.Touchpad
}

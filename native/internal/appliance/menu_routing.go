package appliance

import "strings"

func statisticsPage(page string) bool {
	switch page {
	case "MONITOR_STAT", "AUDIO_STAT", "TOUCH_STAT", "DEVICE_STAT", "CAMERA_METRICS":
		return true
	}
	return false
}

// Page navigation belongs to the middle column. The same gate is checked by
// right-column dispatch, including calls made with a stale rendered hit target.
func (u *UI) middleOnlyRow(page string, row int, lines []string) bool {
	if row <= 0 || row >= len(lines) || strings.TrimSpace(lines[row]) == "" {
		return false
	}
	if strings.HasPrefix(lines[row], "BACK:") {
		return true
	}
	if (page == "INPUT" && lines[row] == "STATISTICS") ||
		(page == "MONITOR" && (lines[row] == "STATISTICS" || lines[row] == "SNIPER MODE")) ||
		(page == "AUDIO" && lines[row] == "STATISTICS") {
		return true
	}
	switch page {
	case "CAMERA":
		return row >= 3 && row <= 6
	case "CAMERA_PICTURE":
		return row >= 1 && row <= 3 || lines[row] == "IMAGE SETTINGS"
	case "DEVICE":
		return row >= 1 && row < len(lines)-1 && row != 5
	case "DISPLAY":
		return row == 2 || row == 3
	case "STORAGE":
		return row == 1 && u.cache == nil
	case "DIAGNOSTICS":
		return row == 1 || row == 2 || row == 7
	case "CAMERA_TRIAL":
		return row == 1 || row == 2
	case "CAMERA_MODES":
		return row <= len(u.state.CameraCatalog())
	case "STORAGE_CONFIRM", "DIAGNOSTICS_ENABLE", "DIAGNOSTICS_CLEAR", "DIAGNOSTICS_DISABLE", "CAMERA_TRIAL_CONFIRM", "POWER":
		return row == 1
	}
	return false
}

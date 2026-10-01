package appliance

import (
	"fmt"
	"strings"
	"time"
)

var monitorBitrates = [...]uint32{2_000_000, 4_000_000, 8_000_000, 12_000_000, 16_000_000, 20_000_000, 30_000_000}
var monitorKeyframes = [...]uint32{1, 2, 5}

// monitorLeafLines owns both monitor pages. Row positions may change when video
// is disabled, so detail and action dispatch below use labels rather than indexes.
func (u *UI) monitorLeafLines(page string) ([]string, bool) {
	if page != "MONITOR" && page != "MONITOR_STAT" && page != "SNIPER" {
		return nil, false
	}
	s := u.state
	s.mu.Lock()
	settings := s.Settings
	if page == "SNIPER" {
		s.mu.Unlock()
		lines := []string{"SNIPER MODE", fmt.Sprintf("ENABLED: %t", settings.SniperEnabled)}
		return append(lines, "BACK: CLOSE MENU"), true
	}
	if page == "MONITOR" {
		s.mu.Unlock()
		lines := []string{"MONITOR / H264", fmt.Sprintf("ENABLED: %t", settings.Enabled)}
		if settings.Enabled {
			w, h := settings.Dimensions()
			lines = append(lines, fmt.Sprintf("RESOLUTION: %d X %d / 60 HZ", w, h))
			lines = append(lines, fmt.Sprintf("BITRATE: %d MBIT/S", settings.Bitrate/1_000_000), fmt.Sprintf("KEYFRAME: %d S", settings.GOPSeconds))
		}
		return append(lines, "STATISTICS", "BACK: CLOSE MENU"), true
	}
	link := s.monitorLinkLocked(time.Now())
	lines := []string{
		"Statistics",
		"FUNCTION: MONITOR",
		fmt.Sprintf("ENABLED: %t", settings.Enabled),
		"LINK: " + strings.ToUpper(link.Phase),
	}
	if failure := s.LastDriverFailure; !failure.At.IsZero() {
		lines = append(lines, fmt.Sprintf("LAST DRIVER ERROR: 0X%08X", failure.Code), fmt.Sprintf("DRIVER FAILURES: %d", failure.Count))
	}
	if settings.Enabled {
		w, h := settings.Dimensions()
		lines = append(lines,
			fmt.Sprintf("FORMAT: %d X %d / H264", w, h),
			fmt.Sprintf("CONFIGURED RATE: %d HZ", settings.FPS))
		if rate := s.frameRate.value; rate.WindowMS > 0 {
			lines = append(lines, fmt.Sprintf("RECEIVED: %.1f FPS", rate.Received), fmt.Sprintf("DECODED: %.1f FPS", rate.Decoded), fmt.Sprintf("DRAWN: %.1f FPS", rate.Presented))
		}
		if s.Received > 0 {
			lines = append(lines,
				fmt.Sprintf("FRAMES RECEIVED: %d", s.Received),
				fmt.Sprintf("FRAMES DECODED: %d", s.Decoded),
				fmt.Sprintf("FRAMES DRAWN: %d", s.Blitted),
				fmt.Sprintf("FRAMES DROPPED: %d", s.Dropped),
				fmt.Sprintf("USB TOTAL: %.1f MIB", float64(s.Bytes)/(1024*1024)))
		}
		if summary := s.metrics.receiveBlit.summary(); summary.Window > 0 {
			lines = append(lines, "P95 RECEIVE TO DRAW: "+monitorP95(summary))
		}
		if s.ThermalPaused {
			lines = append(lines, "THERMAL: PAUSED")
		}
	}
	s.mu.Unlock()
	return append(lines, "BACK: MONITOR"), true
}

func monitorP95(v DurationSummary) string {
	if v.Window == 0 {
		return "--"
	}
	return fmt.Sprintf("%.1f MS", float64(v.P95US)/1000)
}

func monitorTemperature(milliC int64) string {
	if milliC < 0 {
		return "--"
	}
	return fmt.Sprintf("%.1f C", float64(milliC)/1000)
}

func (u *UI) monitorLeafDetail(page string, row int, label, value string) (menuDetail, bool) {
	lines, ok := u.monitorLeafLines(page)
	if !ok || row <= 0 || row >= len(lines) {
		return menuDetail{}, false
	}
	if page == "MONITOR_STAT" {
		return menuDetail{}, true
	}
	settings, _, _ := u.state.Current()
	if page == "SNIPER" {
		if label != "ENABLED" {
			return menuDetail{Title: label}, true
		}
		switch label {
		case "ENABLED":
			return switchDetail(label, settings.SniperEnabled), true
		case "SCALING":
			selected := 0
			if settings.SniperStretch {
				selected = 1
			}
			return menuDetail{Title: label, Action: true, Options: []string{"FIT", "STRETCH"}, Selected: selected, HasSelected: true}, true
		case "ZOOM":
			return sliderDetail(label, settings.Zoom(), 100, 1600, 1), true
		case "X SCALE", "Y SCALE":
			return menuDetail{Title: label + " " + value, Action: true, Options: []string{"-", "+", "RESET 100%"}}, true
		case "X", "Y":
			return menuDetail{Title: label + " " + value, Action: true, Options: []string{"-", "+", "CENTER"}}, true
		case "ROTATION":
			return menuDetail{Title: label + " " + value, Action: true, Options: []string{"-", "+", "RESET 0"}}, true
		case "MIRROR":
			return switchDetail(label, settings.SniperMirror), true
		default:
			return menuDetail{Title: label}, true
		}
	}
	switch label {
	case "ENABLED":
		return switchDetail(label, settings.Enabled), true
	case "RESOLUTION":
		w, _ := settings.Dimensions()
		selected := 0
		if w == 2560 {
			selected = 1
		}
		return menuDetail{Title: label, Action: true, Options: []string{"1280 X 720 / 60 HZ", "2560 X 1440 / 60 HZ"}, Selected: selected, HasSelected: true}, true
	case "BITRATE":
		d := menuDetail{Title: label, Action: true}
		for i, rate := range monitorBitrates {
			d.Options = append(d.Options, fmt.Sprintf("%d MBIT/S", rate/1_000_000))
			if settings.Bitrate == rate {
				d.Selected, d.HasSelected = i, true
			}
		}
		return d, true
	case "KEYFRAME":
		d := menuDetail{Title: label, Action: true}
		for i, seconds := range monitorKeyframes {
			d.Options = append(d.Options, fmt.Sprintf("%d S", seconds))
			if settings.GOPSeconds == seconds {
				d.Selected, d.HasSelected = i, true
			}
		}
		return d, true
	case "STATISTICS", "SNIPER MODE", "BACK":
		return menuDetail{Title: label}, true
	}
	return informationDetail(label, value), true
}

// option == -1 means a middle-column tap. Non-negative options come only from
// the right column; they never open another page.
func (u *UI) selectMonitorLeaf(page string, row, option int) bool {
	lines, ok := u.monitorLeafLines(page)
	if !ok || row <= 0 || row >= len(lines) {
		return false
	}
	label, _ := splitMenuLine(lines[row])
	if option == -1 {
		if page == "MONITOR" && label == "STATISTICS" {
			u.menu("MONITOR_STAT")
		} else if page == "MONITOR" && label == "SNIPER MODE" {
			u.menu("SNIPER")
		} else if label == "BACK" {
			if page == "MONITOR_STAT" {
				u.menu("MONITOR")
			} else {
				u.menu(u.parentMenu(page))
			}
		}
		return true
	}
	if option < 0 || page == "MONITOR_STAT" {
		return true
	}
	settings, _, _ := u.state.Current()
	if page == "SNIPER" {
		if label != "ENABLED" {
			return true
		}
		if label == "X" || label == "Y" || label == "ROTATION" || label == "MIRROR" {
			if option > 2 || (label == "MIRROR" && option > 1) {
				return true
			}
			u.state.mu.Lock()
			v := &u.state.Settings
			switch label {
			case "X":
				if option == 2 {
					v.SniperX = 0
				} else {
					v.SniperX = int16(max(-5000, min(5000, int(v.SniperX)+(option*2-1)*100)))
				}
			case "Y":
				if option == 2 {
					v.SniperY = 0
				} else {
					v.SniperY = int16(max(-5000, min(5000, int(v.SniperY)+(option*2-1)*100)))
				}
			case "ROTATION":
				if option == 2 {
					v.SniperRotation = 0
				} else {
					v.SniperRotation = uint16((int(v.SniperRotation) + (option*2-1)*150 + 3600) % 3600)
				}
			case "MIRROR":
				v.SniperMirror = option == 1
			}
			u.state.mu.Unlock()
			u.draw()
			return true
		}
		if option == 2 && (label == "X SCALE" || label == "Y SCALE") {
			u.setSniperScale(label, 100)
			u.draw()
			return true
		}
		if option > 1 {
			return true
		}
		switch label {
		case "ENABLED":
			settings.SniperEnabled = option == 1
		case "SCALING":
			settings.SniperStretch = option == 1
		case "X SCALE", "Y SCALE":
			value := settings.ScaleX()
			if label == "Y SCALE" {
				value = settings.ScaleY()
			}
			u.setSniperScale(label, value+(option*2-1)*5)
			u.draw()
			return true
		default:
			return true
		}
		u.state.Error(u.state.Configure(settings))
		u.draw()
		return true
	}
	switch label {
	case "ENABLED":
		if option > 1 {
			return true
		}
		settings.Enabled = option == 1
	case "RESOLUTION":
		if option > 1 {
			return true
		}
		settings.Width, settings.Height = 0, 0
		if option == 1 {
			settings.Width, settings.Height = 2560, 1440
		}
	case "BITRATE":
		if option >= len(monitorBitrates) {
			return true
		}
		settings.Bitrate = monitorBitrates[option]
	case "KEYFRAME":
		if option >= len(monitorKeyframes) {
			return true
		}
		settings.GOPSeconds = monitorKeyframes[option]
	default:
		return true
	}
	u.state.Error(u.state.Configure(settings))
	u.draw()
	return true
}

func (u *UI) applyMonitorLeafSlider(page string, row, value int) bool {
	if page != "SNIPER" {
		return false
	}
	lines, ok := u.monitorLeafLines(page)
	if !ok || row <= 0 || row >= len(lines) {
		return false
	}
	label, _ := splitMenuLine(lines[row])
	if label != "ZOOM" {
		return false
	}
	if value < 100 || value > 1600 {
		return true
	}
	u.setSniperZoom(value)
	u.draw()
	return true
}

func (u *UI) setMonitorBrightness(percent int) {
	u.asyncLatest(manualBrightnessControl, func() error {
		u.panelMu.Lock()
		defer u.panelMu.Unlock()
		if u.panel == nil {
			return fmt.Errorf("panel brightness unavailable")
		}
		u.state.mu.Lock()
		u.state.Ambient.Settings.Automatic = false
		u.state.Ambient.Revision++
		u.state.Ambient.Status = "MANUAL / SENSOR OFF"
		u.state.mu.Unlock()
		if err := u.panel.Set(percent); err != nil {
			u.state.mu.Lock()
			u.state.BrightnessStatus = "WRITE ERROR: " + err.Error()
			u.state.mu.Unlock()
			return err
		}
		actual, err := u.panel.Read()
		if err != nil {
			return err
		}
		u.state.mu.Lock()
		u.state.Brightness = percent
		u.state.BrightnessActual = actual
		u.state.Ambient.Fallback = percent
		u.state.BrightnessStatus = "MANUAL / SAVED IF CACHE ENABLED"
		u.state.mu.Unlock()
		return nil
	})
}

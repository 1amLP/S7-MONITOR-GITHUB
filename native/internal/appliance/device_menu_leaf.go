package appliance

import (
	"fmt"
	"perimode/native/internal/orientation"
)

var orientationChoices = []string{"0\u00b0", "90\u00b0", "180\u00b0", "270\u00b0"}

func (u *UI) deviceLeafLines(page string) ([]string, bool) {
	switch page {
	case "DEVICE":
		u.state.mu.Lock()
		strength := u.state.HapticPercent
		u.state.mu.Unlock()
		return []string{"DEVICE", "DISPLAY", "SETTINGS", "POWER SETTINGS", "INSTALL DRIVER", "HAPTIC FEEDBACK: " + hapticLabel(strength), "STATISTICS", "BACK: CLOSE MENU"}, true
	case "POWER_SETTINGS":
		u.state.mu.Lock()
		eco := u.state.CPUEco
		u.state.mu.Unlock()
		mode := "AUTO"
		if eco {
			mode = "ECO"
		}
		return []string{"POWER SETTINGS", "CPU MODE: " + mode, "BACK: DEVICE"}, true
	case "DRIVER_INSTALL":
		if installerUpdating(u.state.EndpointSnapshot()) {
			return []string{"DRIVER", autoUpdateStatus, "BACK: DEVICE"}, true
		}
		return []string{"DRIVER", "INSTALL DRIVER", "RETURN TO DEVICES", "BACK: DEVICE"}, true
	case "DISPLAY":
		_, actual := u.state.ambientCurrent()
		return []string{"DEVICE / DISPLAY", fmt.Sprintf("BRIGHTNESS: %d PCT", actual), "ORIENTATION", "AUTO BRIGHTNESS", "BACK: DEVICE"}, true
	case "SCREEN_ROTATION":
		s, _ := u.state.RotationCurrent()
		return []string{"DISPLAY / ORIENTATION", fmt.Sprintf("ROTATION: %d\u00b0", u.rotationMenuValue(s)), fmt.Sprintf("AUTO ROTATE: %t", s.Automatic), "BACK: DISPLAY"}, true
	case "DISPLAY_AUTO":
		a, _ := u.state.ambientCurrent()
		lines := []string{"DISPLAY / AUTO BRIGHTNESS", fmt.Sprintf("ENABLED: %t", a.Settings.Automatic)}
		if a.Settings.Automatic {
			lines = append(lines, fmt.Sprintf("MINIMUM: %d PCT", a.Settings.Minimum), fmt.Sprintf("MAXIMUM: %d PCT", a.Settings.Maximum), fmt.Sprintf("BIAS: %d PCT", a.Settings.Bias))
		}
		return append(lines, "BACK: DISPLAY"), true
	case "STORAGE":
		if u.cache != nil {
			return []string{"DEVICE / SETTINGS", "SAVE SETTINGS: ENABLED", "BACK: DEVICE"}, true
		}
		return []string{"DEVICE / SETTINGS", "ENABLE SAVING", "BACK: DEVICE"}, true
	}
	return nil, false
}

func (u *UI) deviceLeafDetail(page string, row int) (menuDetail, bool) {
	switch page {
	case "POWER_SETTINGS":
		u.state.mu.Lock()
		eco := u.state.CPUEco
		u.state.mu.Unlock()
		selected := 0
		if eco {
			selected = 1
		}
		if row == 1 {
			d := choicesDetail("CPU MODE", []string{"AUTO", "ECO / 4 CORES"}, selected)
			p := u.state.PowerSnapshot()
			d.Hint = p.Online
			if p.CPUError != "" {
				d.Hint = p.CPUError
			}
			return d, true
		}
		return menuDetail{}, true
	case "DRIVER_INSTALL":
		if installerUpdating(u.state.EndpointSnapshot()) {
			return menuDetail{Title: autoUpdateStatus}, true
		}
		if row == 1 {
			d := choicesDetail("INSTALL DRIVER", []string{"INSTALL"}, -1)
			u.state.mu.Lock()
			d.Hint = u.state.InstallerStatus
			u.state.mu.Unlock()
			if u.state.EndpointSnapshot().PackageVerified {
				d.Hint = "DRIVER PACKAGE VERIFIED"
			} else if d.Hint == "" {
				d.Hint = "DRIVER PACKAGE NOT VERIFIED"
			}
			return d, true
		}
		if row == 2 {
			return choicesDetail("USB", []string{"CONNECT DEVICES"}, -1), true
		}
		return menuDetail{}, true
	case "DEVICE":
		if row == 5 {
			u.state.mu.Lock()
			strength := u.state.HapticPercent
			u.state.mu.Unlock()
			d := sliderDetail("HAPTIC FEEDBACK", strength, 0, 100, 5)
			d.SliderText = hapticLabel(strength)
			return d, true
		}
		return menuDetail{}, true
	case "DISPLAY":
		if row == 1 {
			_, actual := u.state.ambientCurrent()
			return sliderDetail("BRIGHTNESS", actual, 5, 100, 5), true
		}
		return menuDetail{}, true
	case "SCREEN_ROTATION":
		s, _ := u.state.RotationCurrent()
		if row == 1 {
			return choicesDetail("ROTATION", orientationChoices, int(u.rotationMenuValue(s))/90), true
		}
		if row == 2 {
			return switchDetail("AUTO ROTATE", s.Automatic), true
		}
		return menuDetail{}, true
	case "DISPLAY_AUTO":
		a, _ := u.state.ambientCurrent()
		if row == 1 {
			return switchDetail("ENABLED", a.Settings.Automatic), true
		}
		if a.Settings.Automatic {
			switch row {
			case 2:
				return sliderDetail("MINIMUM", a.Settings.Minimum, 5, a.Settings.Maximum, 5), true
			case 3:
				return sliderDetail("MAXIMUM", a.Settings.Maximum, a.Settings.Minimum, 100, 5), true
			case 4:
				return sliderDetail("BIAS", a.Settings.Bias, -30, 30, 5), true
			}
		}
		return menuDetail{}, true
	case "STORAGE":
		return informationDetail("SAVE SETTINGS", "ENABLED"), true
	}
	return menuDetail{}, false
}

func (u *UI) selectDeviceNavigation(page string, row int) bool {
	switch page {
	case "DEVICE":
		pages := []string{"", "DISPLAY", "STORAGE", "POWER_SETTINGS", "DRIVER_INSTALL", "", "DEVICE_STAT"}
		if row > 0 && row < len(pages) && pages[row] != "" {
			u.menu(pages[row])
		}
		return true
	case "DISPLAY":
		if row == 2 {
			u.menu("SCREEN_ROTATION")
		}
		if row == 3 {
			u.menu("DISPLAY_AUTO")
		}
		return true
	case "SCREEN_ROTATION", "DISPLAY_AUTO":
		return true
	case "STORAGE":
		if row == 1 && u.cache == nil {
			u.menu("STORAGE_CONFIRM")
		}
		return true
	}
	return false
}

func (u *UI) selectDeviceDetail(page string, row, option int) bool {
	switch page {
	case "POWER_SETTINGS":
		if row == 1 && option >= 0 && option < 2 {
			u.state.mu.Lock()
			u.state.CPUEco = option == 1
			u.state.mu.Unlock()
			u.draw()
		}
		return true
	case "DRIVER_INSTALL":
		if option == 0 && row >= 1 && row <= 2 {
			u.async(func() error {
				var err error
				if row == 1 {
					err = u.startInstallerMedia()
				} else {
					err = u.stopInstallerMedia()
				}
				if err != nil {
					u.installerMessage(err.Error())
				}
				return err
			})
		}
		return true
	case "SCREEN_ROTATION":
		s, _ := u.state.RotationCurrent()
		if row == 1 && option >= 0 && option < 4 {
			s.Manual = orientation.Degrees(option * 90)
			s.Automatic = false
		} else if row == 2 {
			if option == 0 {
				s.Manual = u.rotationMenuValue(s)
			}
			s.Automatic = option == 1
		} else {
			return true
		}
		u.state.Error(u.configureRotation(s))
		u.draw()
		return true
	case "DISPLAY_AUTO":
		if row == 1 {
			u.async(func() error {
				a, _ := u.state.ambientCurrent()
				s := a.Settings
				s.Automatic = option == 1
				return u.configureAutoBrightness(s)
			})
		}
		u.draw()
		return true
	}
	return false
}

func (u *UI) rotationMenuValue(s orientation.Settings) orientation.Degrees {
	if !s.Automatic {
		return s.Manual
	}
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	return u.state.ActiveRotation
}

func (u *UI) deviceLeafSlider(page string, row, value int) bool {
	if page == "DEVICE" && row == 5 {
		u.state.mu.Lock()
		u.state.HapticPercent = max(0, min(100, value))
		u.state.mu.Unlock()
		u.draw()
		return true
	}
	if page == "DISPLAY" && row == 1 {
		u.setMonitorBrightness(value)
		u.draw()
		return true
	}
	if page != "DISPLAY_AUTO" || row < 2 || row > 4 {
		return false
	}
	u.async(func() error {
		a, _ := u.state.ambientCurrent()
		s := a.Settings
		if !s.Automatic {
			return nil
		}
		switch row {
		case 2:
			s.Minimum = value
		case 3:
			s.Maximum = value
		case 4:
			s.Bias = value
		}
		return u.configureAutoBrightness(s)
	})
	u.draw()
	return true
}

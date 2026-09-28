package appliance

import (
	"fmt"

	"perimode/native/internal/camera"
)

func cameraResolutionModes(sensor camera.Sensor) []camera.Mode {
	var out []camera.Mode
	seen := map[[2]uint32]bool{}
	for _, mode := range camera.VisibleModes(sensor) {
		key := [2]uint32{mode.Width, mode.Height}
		if !seen[key] {
			out = append(out, mode)
			seen[key] = true
		}
	}
	return out
}

func cameraFPSModes(sensor camera.Sensor, selected camera.Mode) []camera.Mode {
	var out []camera.Mode
	for _, mode := range camera.VisibleModes(sensor) {
		if mode.Width == selected.Width && mode.Height == selected.Height {
			out = append(out, mode)
		}
	}
	return out
}

func cameraModeForResolution(sensor camera.Sensor, current, target camera.Mode) camera.Mode {
	options := cameraFPSModes(sensor, target)
	for _, mode := range options {
		if mode.FPS == current.FPS {
			return mode
		}
	}
	for _, mode := range options {
		if mode.FPS == 30 {
			return mode
		}
	}
	if len(options) > 0 {
		return options[0]
	}
	return current
}

func cameraAspect(mode camera.Mode) string {
	a, b := mode.Width, mode.Height
	if a == 0 || b == 0 {
		return "UNKNOWN"
	}
	for b != 0 {
		a, b = b, a%b
	}
	return fmt.Sprintf("%d:%d", mode.Width/a, mode.Height/a)
}

func (u *UI) cameraDeviceLines() []string {
	s := u.state.CameraCurrent().Settings
	return []string{
		"DEVICE SETTINGS",
		fmt.Sprintf("RESOLUTION: %d X %d", s.Mode.Width, s.Mode.Height),
		fmt.Sprintf("FPS: %d", s.Mode.FPS),
		fmt.Sprintf("ORIENTATION: %d DEG", s.Image.Rotation),
		fmt.Sprintf("MIRROR: %t", s.Image.Mirror),
		fmt.Sprintf("BITRATE: %d MBIT/S", s.Bitrate/1_000_000),
		fmt.Sprintf("KEYFRAME: %d S", s.GOPSeconds),
		"BACK: CAMERA",
	}
}

func (u *UI) cameraPictureLines() []string {
	s := u.state.CameraCurrent().Settings
	lines := []string{"PICTURE SETTINGS", "FOCUS", "EXPOSURE / ISO", "WHITE BALANCE", fmt.Sprintf("ZOOM: %d%%", s.Image.Zoom())}
	if s.Sensor == camera.Rear {
		lines = append(lines, fmt.Sprintf("FLASHLIGHT: %t", u.state.TorchCurrent().Wanted))
	}
	if u.state.Camera3ACurrent().View.Descriptor.Limits.ISPImageControls {
		lines = append(lines, "IMAGE SETTINGS")
	}
	return append(lines, "BACK: CAMERA")
}

func (u *UI) cameraDeviceDetail(row int, label, value string) menuDetail {
	s := u.state.CameraCurrent().Settings
	switch row {
	case 1:
		options := cameraResolutionModes(s.Sensor)
		d := menuDetail{Title: "RESOLUTION", Action: true}
		for i, mode := range options {
			d.Options = append(d.Options, fmt.Sprintf("%d X %d", mode.Width, mode.Height))
			if mode.Width == s.Mode.Width && mode.Height == s.Mode.Height {
				d.Selected, d.HasSelected = i, true
			}
		}
		return d
	case 2:
		options := cameraFPSModes(s.Sensor, s.Mode)
		d := menuDetail{Title: "FPS", Action: true}
		for i, mode := range options {
			d.Options = append(d.Options, fmt.Sprintf("%d FPS", mode.FPS))
			if mode == s.Mode {
				d.Selected, d.HasSelected = i, true
			}
		}
		return d
	case 3:
		return choicesDetail("ORIENTATION", []string{"0 DEG", "90 DEG", "180 DEG", "270 DEG"}, int(s.Image.Rotation)/90)
	case 4:
		return switchDetail("MIRROR", s.Image.Mirror)
	case 5:
		d := menuDetail{Title: "BITRATE", Action: true}
		for i, rate := range cameraMenuBitrates {
			d.Options = append(d.Options, fmt.Sprintf("%d MBIT/S", rate/1_000_000))
			if rate == s.Bitrate {
				d.Selected, d.HasSelected = i, true
			}
		}
		return d
	case 6:
		selected := -1
		for i, seconds := range []uint32{1, 2, 5} {
			if s.GOPSeconds == seconds {
				selected = i
			}
		}
		return choicesDetail("KEYFRAME", []string{"1 S", "2 S", "5 S"}, selected)
	}
	return menuDetail{Title: label}
}

func (u *UI) cameraPictureDetail(row int, label string) menuDetail {
	switch row {
	case 4:
		return sliderDetail("ZOOM", u.state.CameraCurrent().Settings.Image.Zoom(), 100, 400, 25)
	case 5:
		if u.state.CameraCurrent().Settings.Sensor == camera.Rear {
			return switchDetail("FLASHLIGHT", u.state.TorchCurrent().Wanted)
		}
	}
	return menuDetail{Title: label}
}

func (u *UI) selectCameraDeviceDetail(row, option int) bool {
	v := u.state.CameraCurrent().Settings
	switch row {
	case 1:
		options := cameraResolutionModes(v.Sensor)
		if option >= len(options) {
			return false
		}
		v.Mode = cameraModeForResolution(v.Sensor, v.Mode, options[option])
	case 2:
		options := cameraFPSModes(v.Sensor, v.Mode)
		if option >= len(options) {
			return false
		}
		v.Mode = options[option]
	case 3:
		if option >= 4 {
			return false
		}
		v.Image.Rotation = uint16(option * 90)
	case 4:
		if option >= 2 {
			return false
		}
		v.Image.Mirror = option == 1
	case 5:
		if option >= len(cameraMenuBitrates) {
			return false
		}
		v.Bitrate = cameraMenuBitrates[option]
	case 6:
		if option >= 3 {
			return false
		}
		v.GOPSeconds = []uint32{1, 2, 5}[option]
	default:
		return false
	}
	u.state.Error(u.configureCamera(v))
	u.draw()
	return true
}

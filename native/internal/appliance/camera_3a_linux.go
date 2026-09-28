//go:build linux && (amd64 || arm64)

package appliance

import (
	"perimode/native/internal/camera"
	"perimode/native/internal/camera/fimcshot"
)

type cameraControlService interface {
	ControlState(camera.Settings) (camera.LiveControlState, error)
	SetControls(camera.Settings, camera.ControlSelection) error
	TriggerFocus(camera.Settings, fimcshot.FocusTrigger) error
	ControlPreferences() camera.ControlPreferences
}

type Camera3ARuntime struct {
	View  camera.LiveControlState `json:"view"`
	Ready bool                    `json:"ready"`
	Error string                  `json:"error,omitempty"`
}

func (s *State) Camera3ACurrent() Camera3ARuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Camera3A
}
func (u *UI) refreshCamera3A() error {
	settings := u.state.CameraCurrent().Settings
	service, ok := u.cameraProvider.(cameraControlService)
	r := Camera3ARuntime{}
	var e error
	if !ok {
		e = camera.ErrControlsUnsupported
	} else {
		r.View, e = service.ControlState(settings)
		r.Ready = e == nil
	}
	if e != nil {
		r.Error = e.Error()
	}
	u.state.mu.Lock()
	if camera.Key(u.state.Camera.Settings) == camera.Key(settings) {
		u.state.Camera3A = r
	}
	u.state.mu.Unlock()
	if ok {
		p := service.ControlPreferences()
		u.state.mu.Lock()
		u.state.Camera3APreferences = p
		u.state.mu.Unlock()
	}
	return e
}
func focusName(m fimcshot.FocusMode) string {
	switch m {
	case fimcshot.FocusOff:
		return "MANUAL"
	case fimcshot.FocusSingle:
		return "AUTO"
	case fimcshot.FocusMacro:
		return "MACRO"
	case fimcshot.FocusContinuousVideo:
		return "CONTINUOUS VIDEO"
	case fimcshot.FocusContinuousPicture:
		return "CONTINUOUS PHOTO"
	}
	return "UNKNOWN"
}
func wbName(m fimcshot.WhiteBalance) string {
	switch m {
	case fimcshot.WBAuto:
		return "AUTO"
	case fimcshot.WBIncandescent:
		return "INCANDESCENT"
	case fimcshot.WBFluorescent:
		return "FLUORESCENT"
	case fimcshot.WBDaylight:
		return "DAYLIGHT"
	case fimcshot.WBCloudy:
		return "CLOUDY"
	case fimcshot.WBCustomK:
		return "MANUAL"
	}
	return "UNSUPPORTED"
}
func (u *UI) camera3ALines(page string) []string {
	r := u.state.Camera3ACurrent()
	title := map[string]string{"CAMERA_FOCUS": "FOCUS", "CAMERA_EXPOSURE": "EXPOSURE", "CAMERA_WB": "WHITE BALANCE", "CAMERA_TONE": "IMAGE SETTINGS"}[page]
	lines := []string{title}
	if r.Ready && r.View.Descriptor.Key == camera.Key(u.state.CameraCurrent().Settings) {
		for _, field := range cameraControlFields(r, page) {
			line := field.Name
			if field.Value != "" {
				line += ": " + field.Value
			}
			lines = append(lines, line)
		}
	}
	return append(lines, "BACK: PICTURE SETTINGS")
}
func isCamera3APage(page string) bool {
	return page == "CAMERA_FOCUS" || page == "CAMERA_EXPOSURE" || page == "CAMERA_WB" || page == "CAMERA_TONE"
}

// Select the next supported shutter without assuming that the source list is
// ordered (the mode's frame limit can fall between two standard presets).
func stepShutter(values []uint64, current, lo, hi uint64, up bool) uint64 {
	best := lo
	if up {
		best = hi
	}
	for _, v := range values {
		if v < lo || v > hi {
			continue
		}
		if up && v > current && v < best {
			best = v
		}
		if !up && v < current && v > best {
			best = v
		}
	}
	return best
}

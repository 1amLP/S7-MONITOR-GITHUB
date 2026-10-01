package appliance

import (
	"fmt"
	"strings"
	"time"

	"perimode/native/internal/camera"
	"perimode/native/internal/camera/fimcshot"
)

func switchDetail(title string, on bool) menuDetail {
	d := menuDetail{Title: title, Options: []string{"OFF", "ON"}, HasSelected: true, Action: true}
	if on {
		d.Selected = 1
	}
	return d
}

func sliderDetail(title string, value, low, high, step int) menuDetail {
	return menuDetail{Title: title, Slider: true, SliderValue: value, SliderMin: low, SliderMax: high, SliderStep: step}
}

func keyedSliderDetail(key, title string, value, low, high, step int) menuDetail {
	d := sliderDetail(title, value, low, high, step)
	d.ControlKey = key
	return d
}

func choicesDetail(title string, options []string, selected int) menuDetail {
	return menuDetail{Title: title, Options: options, Selected: selected, HasSelected: selected >= 0 && selected < len(options), Action: true}
}

func informationDetail(title, value string) menuDetail {
	if value == "" {
		value = "NONE"
	}
	words := strings.Fields(value)
	d := menuDetail{Title: title}
	line := ""
	for _, word := range words {
		if len(line)+len(word)+1 > 46 && line != "" {
			d.Options = append(d.Options, line)
			line = ""
		}
		if len(word) > 46 {
			word = word[:46]
		}
		if line != "" {
			line += " "
		}
		line += word
	}
	if line != "" {
		d.Options = append(d.Options, line)
	}
	return d
}

func (u *UI) functionMenuDetail(page string, row int, label, value string) (menuDetail, bool) {
	if isCamera3APage(page) {
		return u.cameraControlDetail(page, row, label, value)
	}
	switch page {
	case "DEVICE_POWER":
		var action MachineAction
		title := "MENU"
		switch row {
		case 1:
			action, title = ActionPowerOff, "POWER OFF"
		case 2:
			action, title = ActionReboot, "RESTART"
		case 3:
			action, title = ActionRecovery, "RECOVERY"
		}
		if action != ActionNone {
			if u.pendingAction == action && time.Now().Before(u.powerConfirm) {
				return choicesDetail(title, []string{"CONFIRM", "CANCEL"}, -1), true
			}
			return choicesDetail(title, []string{title}, -1), true
		}
	case "DEVICE":
		switch row {
		case 1:
			status := "DISCONNECTED"
			if u.transport != nil && u.transport.Bound() {
				status = "CONNECTED"
			}
			return informationDetail("USB", status), true
		case 2, 3, 4, 5, 7, 8:
			return informationDetail(label, value), true
		}
	case "CAMERA":
		c := u.state.CameraCurrent()
		switch row {
		case 1:
			return switchDetail("ENABLED", c.Enabled), true
		case 2:
			selected := 0
			if c.Settings.Sensor == camera.Front {
				selected = 1
			}
			return choicesDetail("CAMERA", []string{"REAR", "FRONT"}, selected), true
		}
	case "CAMERA_DEVICE":
		return u.cameraDeviceDetail(row, label, value), true
	case "CAMERA_PICTURE":
		return u.cameraPictureDetail(row, label), true
	case "CAMERA_IMAGE":
		c := u.state.CameraCurrent().Settings.Image
		switch row {
		case 1:
			return choicesDetail("ROTATION", []string{"0 DEG", "90 DEG", "180 DEG", "270 DEG"}, int(c.Rotation)/90), true
		case 2:
			return switchDetail("MIRROR", c.Mirror), true
		case 6:
			return sliderDetail("ZOOM", c.Zoom(), 100, 400, 25), true
		case 3, 4, 5, 7:
			return informationDetail(label, value), true
		}
	case "CAMERA_PREVIEW":
		p := u.state.PreviewCurrent()
		if p.Options.Fullscreen && row > 2 {
			return menuDetail{}, false
		}
		switch row {
		case 1:
			return switchDetail("PREVIEW", p.Enabled), true
		case 2:
			selected := 0
			if p.Options.Fullscreen {
				selected = 1
			}
			return choicesDetail("WINDOW", []string{"SMALL", "FULLSCREEN"}, selected), true
		case 3:
			selected := -1
			for i, n := range []int{25, 33, 40} {
				if p.Options.WidthPercent == n {
					selected = i
				}
			}
			return choicesDetail("WINDOW SIZE", []string{"25%", "33%", "40%"}, selected), true
		case 4:
			return choicesDetail("WINDOW POSITION", []string{"TOP LEFT", "TOP RIGHT", "BOTTOM LEFT", "BOTTOM RIGHT"}, p.Options.Corner&3), true
		}
	case "CAMERA_TORCH":
		if row == 1 {
			return switchDetail("FLASHLIGHT", u.state.TorchCurrent().Wanted), true
		}
		if row < 8 {
			return informationDetail(label, value), true
		}
	}
	return menuDetail{}, false
}

var cameraMenuBitrates = []uint32{2_000_000, 4_000_000, 8_000_000, 16_000_000, 25_000_000, 40_000_000, 60_000_000}

func (u *UI) retryCameraFromMenu() {
	u.async(func() error {
		if u.transport == nil {
			return fmt.Errorf("USB camera unavailable")
		}
		return u.transport.RetryCamera()
	})
	u.draw()
}

func (u *UI) setAudioEnabled(microphone, on bool) {
	u.async(func() (err error) {
		defer func() {
			if err != nil {
				u.state.mu.Lock()
				u.state.AudioError = err.Error()
				u.state.mu.Unlock()
			}
		}()
		if on {
			if u.transport == nil {
				return fmt.Errorf("USB audio unavailable")
			}
			if err := u.transport.EnsureAudio(); err != nil {
				return err
			}
		}
		u.state.mu.Lock()
		defer u.state.mu.Unlock()
		if microphone {
			u.state.MicrophoneEnabled = on
		} else {
			u.state.SpeakerEnabled = on
		}
		u.state.AudioRetrySerial++
		u.state.AudioError = ""
		return nil
	})
}

func (u *UI) setWebcamEnabled(on bool) {
	// Enabled is the requested state. Capture readiness/errors are separate;
	// a failed provider must not silently undo a user's selection.
	if err := u.state.cameraEnabled(on); err != nil {
		u.state.Error(err)
		return
	}
	if !on {
		return
	}
	u.async(func() error {
		if u.cameraProvider == nil {
			return camera.ErrSensorGraph
		}
		settings := u.state.CameraCurrent().Settings
		u.state.RefreshCameraCatalog(u.cameraProvider)
		if err := u.cameraProvider.Available(settings); err != nil {
			u.state.cameraStatus("CAMERA UNAVAILABLE", err)
			return err
		}
		if u.transport == nil {
			return fmt.Errorf("USB camera unavailable")
		}
		if err := u.transport.EnsureCamera(u.cameraProvider, settings); err != nil {
			u.state.cameraStatus("CAMERA UNAVAILABLE", err)
			return err
		}
		return nil
	})
}

func (u *UI) selectFunctionDetail(page string, row, option int) bool {
	if isCamera3APage(page) {
		return u.selectCameraControlDetail(page, row, option)
	}
	switch page {
	case "DEVICE_POWER":
		actions := [...]MachineAction{ActionNone, ActionPowerOff, ActionReboot, ActionRecovery}
		if row < 1 || row >= len(actions) {
			return false
		}
		action := actions[row]
		if u.pendingAction == action && time.Now().Before(u.powerConfirm) {
			if option == 0 {
				u.confirmPower(time.Now())
			} else {
				u.pendingAction = ActionNone
				u.powerConfirm = time.Time{}
				u.draw()
			}
		} else {
			u.requestPower(action)
		}
		return true
	case "CAMERA":
		v := u.state.CameraCurrent().Settings
		switch row {
		case 1:
			u.setWebcamEnabled(option == 1)
			u.draw()
			return true
		case 2:
			v.Sensor = []camera.Sensor{camera.Rear, camera.Front}[option]
			if !v.Mode.Valid(v.Sensor) {
				v.Mode = camera.Mode{Width: 1920, Height: 1080, FPS: 30}
			}
		default:
			return false
		}
		u.state.Error(u.configureCamera(v))
	case "CAMERA_DEVICE":
		return u.selectCameraDeviceDetail(row, option)
	case "CAMERA_PICTURE":
		if row != 5 || u.state.CameraCurrent().Settings.Sensor != camera.Rear {
			return false
		}
		u.state.Error(u.state.RequestTorch(option == 1))
	case "CAMERA_IMAGE":
		v := u.state.CameraCurrent().Settings
		switch row {
		case 1:
			v.Image.Rotation = uint16(option * 90)
		case 2:
			v.Image.Mirror = option == 1
		default:
			return false
		}
		u.state.Error(u.configureCamera(v))
	case "CAMERA_PREVIEW":
		switch row {
		case 1:
			u.state.Error(u.enablePreview(option == 1))
		case 2:
			u.setPreviewFullscreen(option == 1)
		case 3, 4:
			u.state.mu.Lock()
			if row == 3 {
				u.state.Preview.Options.WidthPercent = []int{25, 33, 40}[option]
			} else {
				u.state.Preview.Options.Corner = option
			}
			u.state.mu.Unlock()
		default:
			return false
		}
	case "CAMERA_TORCH":
		if row != 1 {
			return false
		}
		u.state.Error(u.state.RequestTorch(option == 1))
	default:
		return false
	}
	u.draw()
	return true
}

func (u *UI) cameraControlDetail(page string, row int, label, value string) (menuDetail, bool) {
	if label == cameraResetLabel && u.cameraResetRow(page, row) {
		return choicesDetail(cameraResetLabel, []string{"RESET FOR THIS MODE"}, -1), true
	}
	r := u.state.Camera3ACurrent()
	if !r.Ready || r.View.Descriptor.Key != camera.Key(u.state.CameraCurrent().Settings) {
		return informationDetail(label, value), true
	}
	fields := cameraControlFields(r, page)
	if row <= 0 || row > len(fields) {
		return informationDetail(label, value), true
	}
	f := fields[row-1]
	if f.Slider {
		d := keyedSliderDetail(cameraControlKey(page, f.Name), f.Name, f.Number, f.Min, f.Max, 1)
		d.SliderText = f.Value
		return d, true
	}
	if len(f.Options) == 0 {
		return informationDetail(f.Name, f.Value), true
	}
	labels := make([]string, len(f.Options))
	for i := range f.Options {
		labels[i] = f.Options[i].Label
	}
	return choicesDetail(f.Name, labels, f.Selected), true
}

func cameraControlKey(page, name string) string { return "camera/" + page + "/" + name }

func (u *UI) submitCameraControl(key string, apply func(*fimcshot.Controls), trigger fimcshot.FocusTrigger) {
	settings := u.state.CameraCurrent().Settings
	fn := func() error {
		u.cameraControlsMu.Lock()
		defer u.cameraControlsMu.Unlock()
		if camera.Key(settings) != camera.Key(u.state.CameraCurrent().Settings) {
			return fmt.Errorf("camera mode changed")
		}
		service, ok := u.cameraProvider.(cameraControlService)
		if !ok {
			return camera.ErrControlsUnsupported
		}
		view, err := service.ControlState(settings)
		if err != nil {
			return err
		}
		if trigger != fimcshot.FocusIdle {
			err = service.TriggerFocus(settings, trigger)
		} else {
			if apply == nil {
				return fmt.Errorf("camera control update missing")
			}
			next := view.Effective
			apply(&next)
			if next == view.Effective {
				return u.refreshCamera3A()
			}
			err = service.SetControls(settings, camera.ControlSelection{Custom: true, Values: next})
		}
		if err != nil {
			return err
		}
		err = u.refreshCamera3A()
		if err == nil {
			u.feedback()
		}
		return err
	}
	if trigger != fimcshot.FocusIdle {
		u.async(fn)
	} else {
		u.asyncLatest(key, fn)
	}
}

func (u *UI) selectCameraControlDetail(page string, row, option int) bool {
	if u.cameraResetRow(page, row) {
		if option != 0 {
			return false
		}
		u.resetCameraControls()
		return true
	}
	r := u.state.Camera3ACurrent()
	if !r.Ready || r.View.Descriptor.Key != camera.Key(u.state.CameraCurrent().Settings) {
		return false
	}
	fields := cameraControlFields(r, page)
	if row <= 0 || row > len(fields) || option < 0 || option >= len(fields[row-1].Options) {
		return false
	}
	value := fields[row-1].Options[option]
	key := cameraControlKey(page, fields[row-1].Name)
	u.submitCameraControl(key, value.Apply, value.Trigger)
	return true
}

func (u *UI) cameraControlSlider(page string, row, value int) bool {
	if !isCamera3APage(page) {
		return false
	}
	r := u.state.Camera3ACurrent()
	if !r.Ready || r.View.Descriptor.Key != camera.Key(u.state.CameraCurrent().Settings) {
		return true
	}
	fields := cameraControlFields(r, page)
	if row <= 0 || row > len(fields) {
		return true
	}
	f := fields[row-1]
	if !f.Slider || value < f.Min || value > f.Max {
		return true
	}
	var apply func(*fimcshot.Controls)
	switch f.Name {
	case "BRIGHTNESS":
		apply = func(c *fimcshot.Controls) { c.Brightness = int32(value) }
	case "CONTRAST":
		apply = func(c *fimcshot.Controls) { c.Contrast = uint32(value) }
	case "GAMMA":
		apply = func(c *fimcshot.Controls) { c.Gamma = uint32(value) }
	case "SHARPNESS":
		apply = func(c *fimcshot.Controls) { c.Sharpness = uint32(value) }
	case "TEMPERATURE":
		apply = func(c *fimcshot.Controls) { c.WBTemperature = uint32(value * 100) }
	case "DISTANCE":
		apply = func(c *fimcshot.Controls) { c.FocusDioptres = float32(value) / 10 }
	case "EXPOSURE COMPENSATION":
		apply = func(c *fimcshot.Controls) { c.Compensation = int32(value) }
	default:
		return true
	}
	u.submitCameraControl(cameraControlKey(page, f.Name), apply, fimcshot.FocusIdle)
	return true
}

func (u *UI) applyDetailSlider(value int) {
	_, _, page := u.state.Current()
	d := u.menuDetail(page, u.menuFocus)
	lo, hi, step := d.SliderMin, d.SliderMax, d.SliderStep
	if hi == 0 {
		lo, hi, step = 50, 200, 5
	}
	if !d.Slider || value < lo || value > hi || (value-lo)%step != 0 {
		return
	}
	pending := u.controls.pending(d.ControlKey)
	if d.ControlKey == manualBrightnessControl {
		a, _ := u.state.ambientCurrent()
		pending = pending || a.Settings.Automatic || u.controls.pending(autoBrightnessControl)
	}
	if value == d.SliderValue && strings.HasPrefix(d.ControlKey, "camera/") {
		if u.cameraControlsMu.TryLock() {
			u.cameraControlsMu.Unlock()
		} else {
			// A PC control update may not have published its UI snapshot yet.
			pending = true
		}
	}
	if value == d.SliderValue && !pending {
		return
	}
	defer func() {
		if after := u.menuDetail(page, u.menuFocus); after.Slider && after.SliderValue != d.SliderValue {
			u.feedback()
		}
	}()
	if u.deviceLeafSlider(page, u.menuFocus, value) {
		return
	}
	if u.cameraControlSlider(page, u.menuFocus, value) {
		return
	}
	switch page {
	case "MONITOR", "SNIPER":
		u.applyMonitorLeafSlider(page, u.menuFocus, value)
	case "INPUT":
		if u.menuFocus == 3 {
			u.applyPadSensitivity(value)
		}
	case "AUDIO", "SPEAKER", "MICROPHONE":
		u.applyAudioLeafSlider(page, u.menuFocus, value)
	case "CAMERA_IMAGE", "CAMERA_PICTURE":
		if page == "CAMERA_PICTURE" && u.menuFocus != 4 {
			return
		}
		v := u.state.CameraCurrent().Settings
		v.Image.ZoomPercent = uint16(value)
		u.state.Error(u.configureCamera(v))
		u.draw()
	case "INDICATOR_SETTINGS":
		u.state.mu.Lock()
		u.state.IndicatorScale = value
		u.state.mu.Unlock()
		u.draw()
	}
}

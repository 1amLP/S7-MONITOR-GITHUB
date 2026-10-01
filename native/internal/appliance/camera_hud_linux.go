//go:build linux && (amd64 || arm64)

package appliance

import (
	"fmt"
	"perimode/native/internal/camera"
	"perimode/native/internal/fb"
	"image"
	"time"
)

type cameraHUDControl struct {
	page, key, label string
	row              int
	field            cameraControlField
}

func (u *UI) cameraHUDControls() []cameraHUDControl {
	r := u.state.Camera3ACurrent()
	rear := u.state.CameraCurrent().Settings.Sensor == camera.Rear
	fieldLimit := 15
	if rear {
		fieldLimit--
	}
	var out []cameraHUDControl
	if r.Ready && r.View.Descriptor.Key == camera.Key(u.state.CameraCurrent().Settings) {
		for _, page := range []string{"CAMERA_EXPOSURE", "CAMERA_WB", "CAMERA_FOCUS", "CAMERA_TONE"} {
			for i, f := range cameraControlFields(r, page) {
				if f.Name == "AUTOFOCUS" || (!f.Slider && len(f.Options) < 2) {
					continue
				}
				label := f.Name
				if label == "MODE" {
					if page == "CAMERA_EXPOSURE" {
						label = "EXPOSURE"
					} else if page == "CAMERA_WB" {
						label = "WB MODE"
					} else {
						label = "FOCUS"
					}
				}
				if label == "EXPOSURE COMPENSATION" {
					label = "EV"
				}
				if label == "PRESET" {
					label = "WB"
				}
				if len(out) < fieldLimit {
					out = append(out, cameraHUDControl{page: page, key: page + "/" + f.Name, label: label, row: i + 1, field: f})
				}
			}
		}
	}
	zoom := u.state.CameraCurrent().Settings.Image.Zoom()
	out = append(out, cameraHUDControl{key: "ZOOM", label: "ZOOM", field: cameraControlField{Value: fmt.Sprintf("%.2f X", float64(zoom)/100)}})
	if rear {
		value := "OFF"
		torch := u.state.TorchCurrent()
		if torch.Wanted {
			value = "STARTING"
			if torch.Accepted {
				value = "ON"
			}
		}
		if torch.LastError != "" {
			value = "ERROR"
		}
		out = append(out, cameraHUDControl{key: "TORCH", label: "FLASHLIGHT", field: cameraControlField{Value: value}})
	}
	return out
}

func (u *UI) toggleCameraHUDTorch() {
	u.state.Error(u.state.RequestTorch(!u.state.TorchCurrent().Wanted))
	u.cameraHUDSelection = ""
}

func (u *UI) currentCameraHUD() *fb.CameraHUD {
	if hud := u.sniperHUD(); hud != nil {
		return hud
	}
	p := u.state.PreviewForDisplay()
	cam := u.state.CameraCurrent()
	if !p.Enabled || !p.Options.Fullscreen || !cam.Enabled {
		if u.noticeText != "" {
			return &fb.CameraHUD{Notice: u.noticeText, Selected: -1, OptionSelected: -1}
		}
		return nil
	}
	r := u.state.Camera3ACurrent()
	if (!r.Ready || r.View.Descriptor.Key != camera.Key(cam.Settings)) && time.Since(u.cameraHUDRefresh) > time.Second {
		u.cameraHUDRefresh = time.Now()
		u.async(u.refreshCamera3A)
	}
	v := &fb.CameraHUD{Selected: -1, OptionSelected: -1, Notice: u.noticeText}
	for i, c := range u.cameraHUDControls() {
		v.Count++
		v.Titles[i], v.Values[i] = c.label, c.field.Value
		if c.key != u.cameraHUDSelection {
			continue
		}
		v.Selected = i
		if c.key == "ZOOM" {
			for j, n := range []int{100, 150, 200, 300, 400} {
				v.Options[j] = fmt.Sprintf("%.2f X", float64(n)/100)
				v.OptionCount++
				if cam.Settings.Image.Zoom() == n {
					v.OptionSelected = j
				}
			}
		} else if c.field.Slider {
			v.Slider = true
			v.Value, v.Min, v.Max = c.field.Number, c.field.Min, c.field.Max
		} else {
			for j, o := range c.field.Options {
				if j >= len(v.Options) {
					break
				}
				v.Options[j] = o.Label
				v.OptionCount++
			}
			v.OptionSelected = c.field.Selected
		}
	}
	if !u.cameraHUDFocusAt.IsZero() && time.Since(u.cameraHUDFocusAt) < 1500*time.Millisecond {
		v.Focus = true
		v.FocusX, v.FocusY = u.cameraHUDFocusX, u.cameraHUDFocusY
	}
	return v
}

// On-screen controls consume their own gestures in both input modes.
func (u *UI) cameraHUDInput(x, y uint16, drag bool) bool {
	if u.state.ActiveView() == ViewSniper {
		return false
	}
	p := u.state.PreviewForDisplay()
	if !u.cameraHUDVisible || !p.Enabled || !p.Options.Fullscreen || u.screen == nil {
		return false
	}
	w, h := u.screen.InputSize()
	v := u.cameraHUDPaint
	g := fb.LayoutCameraHUD(w, h, v)
	point := image.Pt(int(x)*(w-1)/32767, int(y)*(h-1)/32767)
	fields := u.cameraHUDControls()
	if !drag {
		for i := 0; i < v.Count; i++ {
			if point.In(g.Fields[i]) {
				if i >= len(fields) || fields[i].label != v.Titles[i] {
					return true
				}
				key := fields[i].key
				if key == "TORCH" {
					u.toggleCameraHUDTorch()
					u.draw()
					return true
				}
				if u.cameraHUDSelection == key {
					key = ""
				}
				u.cameraHUDSelection = key
				u.cameraHUDLastValue = -1
				u.draw()
				return true
			}
		}
	}
	if point.In(g.Panel) && v.Selected >= 0 && v.Selected < len(fields) {
		c := fields[v.Selected]
		if c.key != u.cameraHUDSelection {
			return true
		}
		if v.Slider && point.In(g.Slider) {
			if !c.field.Slider || c.field.Min != v.Min || c.field.Max != v.Max {
				return true
			}
			value := v.Min + (g.Slider.Max.Y-1-point.Y)*(v.Max-v.Min)/max(1, g.Slider.Dy()-1)
			u.cameraHUDDragging = true
			if value != u.cameraHUDLastValue {
				u.cameraHUDLastValue = value
				u.cameraControlSlider(c.page, c.row, value)
				u.draw()
			}
			return true
		}
		if !drag {
			for i := 0; i < v.OptionCount; i++ {
				if point.In(g.Values[i]) {
					if c.key == "ZOOM" {
						s := u.state.CameraCurrent().Settings
						s.Image.ZoomPercent = uint16([]int{100, 150, 200, 300, 400}[i])
						u.state.Error(u.configureCamera(s))
					} else {
						if i >= len(c.field.Options) || c.field.Options[i].Label != v.Options[i] {
							return true
						}
						u.selectCameraControlDetail(c.page, c.row, i)
					}
					u.cameraHUDSelection = ""
					u.draw()
					return true
				}
			}
		}
		return true
	}
	if !drag && u.cameraHUDSelection != "" {
		u.cameraHUDSelection = ""
		u.draw()
	}
	return false
}

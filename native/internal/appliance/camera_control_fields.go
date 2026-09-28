package appliance

import (
	"fmt"
	"perimode/native/internal/camera/fimcshot"
)

type cameraControlOption struct {
	Label    string
	Controls fimcshot.Controls
	Trigger  fimcshot.FocusTrigger
}
type cameraControlField struct {
	Name, Value      string
	Options          []cameraControlOption
	Selected         int
	Slider           bool
	Number, Min, Max int
}

// The same fields drive the middle rows, right-hand values and dispatch.
func cameraControlFields(r Camera3ARuntime, page string) []cameraControlField {
	if !r.Ready {
		return nil
	}
	c, l := r.View.Effective, r.View.Descriptor.Limits
	var fields []cameraControlField
	add := func(name, value string) *cameraControlField {
		fields = append(fields, cameraControlField{Name: name, Value: value, Selected: -1})
		return &fields[len(fields)-1]
	}
	choice := func(f *cameraControlField, label string, selected bool, change func(*fimcshot.Controls)) {
		v := c
		if change != nil {
			change(&v)
		}
		if selected {
			f.Selected = len(f.Options)
		}
		f.Options = append(f.Options, cameraControlOption{Label: label, Controls: v})
	}
	switch page {
	case "CAMERA_EXPOSURE":
		mode := "AUTO"
		if c.ExposureNS != 0 {
			mode = "MANUAL"
		}
		f := add("MODE", mode)
		choice(f, "AUTO", c.ExposureNS == 0, func(v *fimcshot.Controls) { v.ExposureNS = 0; v.ISO = 0 })
		if l.ManualExposure {
			choice(f, "MANUAL", c.ExposureNS != 0, func(v *fimcshot.Controls) {
				v.AELocked = false
				v.Compensation = 0
				v.AERegion = fimcshot.Region{}
				if v.ExposureNS == 0 {
					v.ExposureNS = max(l.ExposureMinNS, min(uint64(16666666), min(l.ExposureMaxNS, l.FrameDurationNS)))
					v.ISO = l.ISOMin
				}
			})
		}
		if c.ExposureNS != 0 && l.ManualExposure {
			f = add("SHUTTER", fmt.Sprintf("%.3f MS", float64(c.ExposureNS)/1e6))
			for _, ns := range []uint64{250000, 500000, 1000000, 2000000, 4000000, 8000000, 16666666, min(l.ExposureMaxNS, l.FrameDurationNS)} {
				if ns < l.ExposureMinNS || ns > l.ExposureMaxNS || ns > l.FrameDurationNS {
					continue
				}
				value := ns
				choice(f, fmt.Sprintf("%.3f MS", float64(ns)/1e6), c.ExposureNS == ns, func(v *fimcshot.Controls) { v.ExposureNS = value })
			}
			if l.ManualISO {
				f = add("ISO", fmt.Sprint(c.ISO))
				values := []uint32{l.ISOMin, 100, 200, 400, 800, l.ISOMax}
				seen := map[uint32]bool{}
				for _, iso := range values {
					if iso < l.ISOMin || iso > l.ISOMax || seen[iso] {
						continue
					}
					seen[iso] = true
					value := iso
					choice(f, fmt.Sprint(iso), iso == c.ISO, func(v *fimcshot.Controls) { v.ISO = value })
				}
			}
		} else {
			f = add("EXPOSURE COMPENSATION", fmt.Sprintf("%+.1f EV", float32(c.Compensation)*r.View.Descriptor.EVStep))
			f.Slider = true
			f.Number = int(c.Compensation)
			f.Min = int(l.CompensationMin)
			f.Max = int(l.CompensationMax)
			if l.AELock {
				f = add("AE LOCK", fmt.Sprint(c.AELocked))
				choice(f, "OFF", !c.AELocked, func(v *fimcshot.Controls) { v.AELocked = false })
				choice(f, "ON", c.AELocked, func(v *fimcshot.Controls) { v.AELocked = true })
			}
		}
		f = add("ANTI-FLICKER", []string{"AUTO", "OFF", "50 HZ", "60 HZ", "AUTO"}[c.AntiBand])
		for _, mode := range []uint32{4, 1, 2, 3} {
			value := mode
			choice(f, []string{"", "OFF", "50 HZ", "60 HZ", "AUTO"}[mode], c.AntiBand == mode || (c.AntiBand == 0 && mode == 4), func(v *fimcshot.Controls) { v.AntiBand = value })
		}
	case "CAMERA_FOCUS":
		f := add("MODE", focusName(c.Focus))
		if l.FocusModes == 1<<fimcshot.FocusOff {
			f.Value = "FIXED"
			return fields
		}
		for m := fimcshot.FocusOff; m <= fimcshot.FocusContinuousPicture; m++ {
			if l.FocusModes&(1<<m) == 0 {
				continue
			}
			mode := m
			choice(f, focusName(m), m == c.Focus, func(v *fimcshot.Controls) {
				v.Focus = mode
				v.FocusDioptres = -1
				v.AFRegion = fimcshot.Region{}
				if mode == fimcshot.FocusOff && l.ManualFocus {
					v.FocusDioptres = 0
				}
			})
		}
		if c.Focus == fimcshot.FocusOff && l.ManualFocus {
			f = add("DISTANCE", fmt.Sprintf("%.1f D", c.FocusDioptres))
			f.Slider = true
			f.Number = int(c.FocusDioptres * 10)
			f.Max = int(l.MaxFocusDioptres * 10)
		} else if c.Focus != fimcshot.FocusOff {
			f = add("AUTOFOCUS", "")
			f.Options = []cameraControlOption{{Label: "START", Controls: c, Trigger: fimcshot.FocusStart}, {Label: "CANCEL", Controls: c, Trigger: fimcshot.FocusCancel}}
		}
	case "CAMERA_TONE":
		if !l.ISPImageControls {
			return fields
		}
		contrast, gamma := c.Contrast, c.Gamma
		if contrast == 0 {
			contrast = 100
		}
		if gamma == 0 {
			gamma = 100
		}
		for _, entry := range []struct {
			name            string
			value, min, max int
		}{
			{"BRIGHTNESS", int(c.Brightness), -100, 100}, {"CONTRAST", int(contrast), 10, 200}, {"GAMMA", int(gamma), 50, 300}, {"SHARPNESS", int(c.Sharpness), 0, 10},
		} {
			f := add(entry.name, fmt.Sprint(entry.value))
			f.Slider = true
			f.Number, f.Min, f.Max = entry.value, entry.min, entry.max
		}
	case "CAMERA_WB":
		if l.WBModeMask&(1<<fimcshot.WBCustomK) != 0 {
			mode := "AUTO"
			if c.WhiteBalance != fimcshot.WBAuto {
				mode = "MANUAL"
			}
			f := add("MODE", mode)
			choice(f, "AUTO", c.WhiteBalance == fimcshot.WBAuto, func(v *fimcshot.Controls) { v.WhiteBalance = fimcshot.WBAuto; v.AWBLocked = false })
			choice(f, "MANUAL", c.WhiteBalance == fimcshot.WBCustomK, func(v *fimcshot.Controls) {
				v.WhiteBalance = fimcshot.WBCustomK
				v.AWBLocked = false
				if v.WBTemperature == 0 {
					v.WBTemperature = 6500
				}
			})
			if c.WhiteBalance == fimcshot.WBCustomK {
				f = add("TEMPERATURE", fmt.Sprintf("%d K", c.WBTemperature))
				f.Slider = true
				f.Number = int(c.WBTemperature) / 100
				f.Min, f.Max = 20, 100
				return fields
			}
		}
		f := add("PRESET", wbName(c.WhiteBalance))
		for _, wb := range []fimcshot.WhiteBalance{fimcshot.WBAuto, fimcshot.WBIncandescent, fimcshot.WBFluorescent, fimcshot.WBDaylight, fimcshot.WBCloudy} {
			if l.WBModeMask&(1<<wb) == 0 {
				continue
			}
			value := wb
			choice(f, wbName(wb), wb == c.WhiteBalance, func(v *fimcshot.Controls) { v.WhiteBalance = value; v.AWBLocked = false })
		}
		if l.AWBLock && c.WhiteBalance == fimcshot.WBAuto {
			f = add("AWB LOCK", fmt.Sprint(c.AWBLocked))
			choice(f, "OFF", !c.AWBLocked, func(v *fimcshot.Controls) { v.AWBLocked = false })
			choice(f, "ON", c.AWBLocked, func(v *fimcshot.Controls) { v.AWBLocked = true })
		}
	}
	return fields
}

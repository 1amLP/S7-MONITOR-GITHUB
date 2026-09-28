//go:build linux && (amd64 || arm64)

package camera

import (
	"fmt"
	"perimode/native/internal/camera/fimcshot"
)

// controlLimits translates static fields of the pinned vendor HAL. Evidence and
// constructor offsets: hardware/source-config/camera-control-evidence.json.
// A range is not proof of live operation. It never changes the selected FPS.
func controlLimits(sensor Sensor, module uint32, mode Mode, crop fimcshot.Rect) (fimcshot.Limits, error) {
	if !knownModule(sensor, module) || !mode.Valid(sensor) {
		return fimcshot.Limits{}, fmt.Errorf("unknown control profile")
	}
	l := fimcshot.Limits{Crop: crop, FrameDurationNS: 1_000_000_000 / uint64(mode.FPS),
		CompensationMin: -20, CompensationMax: 20, AELock: true, AWBLock: true,
		FocusModes: 1 << fimcshot.FocusOff,
		WBModeMask: 1<<fimcshot.WBAuto | 1<<fimcshot.WBIncandescent | 1<<fimcshot.WBFluorescent | 1<<fimcshot.WBDaylight | 1<<fimcshot.WBCloudy}
	if sensor == Rear {
		l.FocusModes = 0x3e
		l.ManualFocus = true
		l.MaxFocusDioptres = 10
		l.ManualExposure = true
		l.ManualISO = true
		l.ExposureMinNS = 22000
		l.ExposureMaxNS = min(uint64(100000000), l.FrameDurationNS)
		l.ISOMin = 64
		l.ISOMax = 1600
	}
	return l, l.Validate()
}

func (p HerolteProvider) DescribeControls(s Settings) (ControlDescriptor, error) {
	if e := s.Validate(); e != nil {
		return ControlDescriptor{}, e
	}
	plan, e := p.rt().lookup(s)
	if e != nil {
		return ControlDescriptor{}, e
	}
	if plan.Sensor != s.Sensor || plan.Mode != s.Mode {
		return ControlDescriptor{}, fmt.Errorf("controls for a different camera mode")
	}
	if e = plan.Validate(); e != nil {
		return ControlDescriptor{}, e
	}
	return ControlDescriptor{Key: Key(s), ModuleID: plan.ModuleID, Limits: plan.Limits, Defaults: plan.Controls, EVStep: plan.Profile.CompensationStep}, nil
}

package camera

import (
	"errors"
	"fmt"
	"perimode/native/pkg/usb/uvcmode"
)

// ModeStatus distinguishes a requested target from a mode admitted by a native
// provider. Admitted is a software capability claim, NOT hardware acceptance.
type ModeStatus struct {
	Sensor   Sensor `json:"sensor"`
	Mode     Mode   `json:"mode"`
	Admitted bool   `json:"admitted_by_provider"`
	Reason   string `json:"reason,omitempty"`
}

var ErrWebcamUnverified = errors.New("hidden: high-FPS S7 to Windows webcam mode is unverified")

type Catalog []ModeStatus

func InspectModes(p Provider, base Settings) Catalog {
	out := Catalog{}
	for _, sensor := range []Sensor{Rear, Front} {
		for _, mode := range Modes(sensor) {
			candidate := base
			candidate.Sensor = sensor
			candidate.Mode = mode
			item := ModeStatus{Sensor: sensor, Mode: mode}
			err := candidate.Validate()
			if err == nil && !mode.WebcamEligible() {
				err = ErrWebcamUnverified
			}
			if err == nil {
				if p == nil {
					err = ErrSensorGraph
				} else {
					err = p.Available(candidate)
				}
			}
			item.Admitted = err == nil
			if err != nil {
				item.Reason = err.Error()
			}
			out = append(out, item)
		}
	}
	return out
}
func (c Catalog) Check(s Settings) error {
	for _, v := range c {
		if v.Sensor == s.Sensor && v.Mode == s.Mode {
			if v.Admitted && v.Mode.WebcamEligible() {
				return nil
			}
			return fmt.Errorf("%s %s unavailable: %s", s.Sensor, s.Mode, v.Reason)
		}
	}
	return fmt.Errorf("camera target absent from mode catalog")
}
func (c Catalog) Table() (uvcmode.Table, error) {
	var modes []uvcmode.Mode
	seen := map[Mode]bool{}
	for _, v := range c {
		if v.Admitted && v.Mode.WebcamEligible() && !seen[v.Mode] {
			modes = append(modes, uvcmode.Mode(v.Mode))
			seen[v.Mode] = true
		}
	}
	return uvcmode.New(modes)
}

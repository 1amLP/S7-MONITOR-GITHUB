// Package orientation owns the local panel coordinate system. It does not
// change the H.264/USB mode, panel timings, or the camera image orientation.
package orientation

import "fmt"

// Degrees is the clockwise rotation of logical content onto the natural panel.
// 0 is natural portrait on herolte; 270 is upright landscape for the S7 panel.
type Degrees int

func (d Degrees) Valid() bool   { return d == 0 || d == 90 || d == 180 || d == 270 }
func (d Degrees) Swapped() bool { return d == 90 || d == 270 }
func (d Degrees) Size(panelW, panelH int) (int, int) {
	if d.Swapped() {
		return panelH, panelW
	}
	return panelW, panelH
}

// ToPanel and FromPanel are exact inverses, including the last pixel on each axis.
func (d Degrees) ToPanel(x, y, panelW, panelH int) (int, int) {
	switch d {
	case 90:
		return panelW - 1 - y, x
	case 180:
		return panelW - 1 - x, panelH - 1 - y
	case 270:
		return y, panelH - 1 - x
	}
	return x, y
}
func (d Degrees) FromPanel(x, y, panelW, panelH int) (int, int) {
	switch d {
	case 90:
		return y, panelW - 1 - x
	case 180:
		return panelW - 1 - x, panelH - 1 - y
	case 270:
		return panelH - 1 - y, x
	}
	return x, y
}

// NormalizedFromPanel transforms evdev's normalized natural-panel coordinates.
func (d Degrees) NormalizedFromPanel(x, y uint16) (uint16, uint16) {
	switch d {
	case 90:
		return y, 32767 - x
	case 180:
		return 32767 - x, 32767 - y
	case 270:
		return 32767 - y, x
	}
	return x, y
}

type Settings struct {
	Automatic bool    `json:"automatic"`
	Manual    Degrees `json:"manual_degrees"`
}

func DefaultSettings() Settings { return Settings{Manual: 270} }
func (s Settings) Validate() error {
	if !s.Manual.Valid() {
		return fmt.Errorf("rotation must be 0, 90, 180 or 270 degrees")
	}
	return nil
}

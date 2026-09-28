// Package cameramode defines the current product targets, not measured hardware capabilities.
// Ordering is part of camera selection protocol v2 and preferences schema10.
package cameramode

import "fmt"

type Sensor uint8

const (
	Rear Sensor = iota
	Front
)

func (s Sensor) String() string {
	if s == Rear {
		return "REAR"
	}
	if s == Front {
		return "FRONT"
	}
	return "INVALID"
}

type Mode struct {
	Width  uint32 `json:"width"`
	Height uint32 `json:"height"`
	FPS    uint32 `json:"fps"`
}

func (m Mode) String() string { return fmt.Sprintf("%d X %d / %d FPS", m.Width, m.Height, m.FPS) }
func Modes(s Sensor) []Mode {
	switch s {
	case Rear:
		return []Mode{{2560, 1440, 30}, {1920, 1080, 60}, {1920, 1080, 30}, {1280, 720, 240}, {1280, 720, 120}, {1280, 720, 60}, {1280, 720, 30}}
	case Front:
		return []Mode{{2560, 1440, 30}, {1920, 1080, 30}, {1280, 720, 30}}
	}
	return nil
}
func (m Mode) Valid(s Sensor) bool {
	for _, v := range Modes(s) {
		if m == v {
			return true
		}
	}
	return false
}
func (m Mode) Known() bool { return m.Valid(Rear) || m.Valid(Front) }
func (m Mode) Interval100ns() uint32 {
	if m.FPS == 0 {
		return 0
	}
	return 10_000_000 / m.FPS
}

// Eligibility only describes the wire/menu matrix. Native provider admission
// still requires the exact sensor profile, firmware and kernel ownership.
func (m Mode) WebcamEligible() bool { return m.Known() }
func VisibleModes(s Sensor) []Mode {
	var out []Mode
	for _, m := range Modes(s) {
		if m.WebcamEligible() {
			out = append(out, m)
		}
	}
	return out
}
func Union() []Mode {
	var out []Mode
	seen := map[Mode]bool{}
	for _, s := range []Sensor{Rear, Front} {
		for _, m := range Modes(s) {
			if !seen[m] {
				out = append(out, m)
				seen[m] = true
			}
		}
	}
	return out
}

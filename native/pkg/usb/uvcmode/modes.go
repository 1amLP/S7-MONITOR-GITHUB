// Package uvcmode is the single source of frame indices and intervals for the
// configfs descriptors and UVC PROBE/COMMIT responder. It contains no USB I/O.
package uvcmode

import (
	"fmt"
	"perimode/native/pkg/cameramode"
	"sort"
)

type Mode = cameramode.Mode
type Frame struct {
	Index         byte
	Width, Height uint32
	FPS           []uint32
}
type Table struct{ frames []Frame }

func Allowed(m Mode) bool { return m.WebcamEligible() }
func NewPublic() (Table, error) {
	var modes []Mode
	for _, mode := range cameramode.Union() {
		if mode.WebcamEligible() {
			modes = append(modes, mode)
		}
	}
	return New(modes)
}
func New(modes []Mode) (Table, error) {
	if len(modes) == 0 || len(modes) > len(cameramode.Union()) {
		return Table{}, fmt.Errorf("one to seven supported UVC modes required")
	}
	sorted := append([]Mode(nil), modes...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Width != b.Width {
			return a.Width < b.Width
		}
		if a.Height != b.Height {
			return a.Height < b.Height
		}
		return a.FPS > b.FPS
	})
	t := Table{}
	for i, m := range sorted {
		if !Allowed(m) || (i > 0 && m == sorted[i-1]) {
			return Table{}, fmt.Errorf("invalid/duplicate UVC mode: %+v", m)
		}
		n := len(t.frames)
		if n == 0 || t.frames[n-1].Width != m.Width || t.frames[n-1].Height != m.Height {
			t.frames = append(t.frames, Frame{Index: byte(n + 1), Width: m.Width, Height: m.Height})
		}
		f := &t.frames[len(t.frames)-1]
		f.FPS = append(f.FPS, m.FPS)
	}
	return t, nil
}
func (t Table) Frames() []Frame {
	out := append([]Frame(nil), t.frames...)
	for i := range out {
		out[i].FPS = append([]uint32(nil), out[i].FPS...)
	}
	return out
}
func (t Table) Contains(m Mode) bool { _, e := t.Index(m); return e == nil }
func (t Table) Index(m Mode) (byte, error) {
	for _, f := range t.frames {
		if f.Width == m.Width && f.Height == m.Height {
			for _, fps := range f.FPS {
				if fps == m.FPS {
					return f.Index, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("mode not in advertised UVC table: %+v", m)
}
func (t Table) Default() Mode {
	if len(t.frames) == 0 {
		return Mode{}
	}
	f := t.frames[0]
	return Mode{Width: f.Width, Height: f.Height, FPS: f.FPS[len(f.FPS)-1]}
}
func (t Table) Maximum() Mode {
	if len(t.frames) == 0 {
		return Mode{}
	}
	f := t.frames[len(t.frames)-1]
	return Mode{Width: f.Width, Height: f.Height, FPS: f.FPS[len(f.FPS)-1]}
}

// Negotiate clamps a PROBE interval within the advertised discrete intervals.
// Zero means unspecified and selects the slowest advertised rate. A COMMIT must
// still match the canonical PROBE exactly; this function does not commit it.
func (t Table) Negotiate(index byte, interval uint32) (Mode, error) {
	for _, f := range t.frames {
		if f.Index != index {
			continue
		}
		fps := f.FPS[len(f.FPS)-1]
		if interval > 0 {
			for _, candidate := range f.FPS {
				if 10_000_000/candidate >= interval {
					fps = candidate
					break
				}
			}
		}
		return Mode{Width: f.Width, Height: f.Height, FPS: fps}, nil
	}
	return Mode{}, fmt.Errorf("UVC frame index %d was not advertised", index)
}

package uvcmode

import "perimode/native/pkg/cameramode"

// NewUSBTrial is deliberately distinct from New. A production catalog cannot
// acquire high-FPS modes by passing a different list to New. The trial advertises
// exactly one resolution and one rate, so PROBE cannot fall back to 30/60.
func NewUSBTrial(fps uint32) (Table, error) {
	m, err := cameramode.USBTrialMode(fps)
	if err != nil {
		return Table{}, err
	}
	return Table{frames: []Frame{{Index: 1, Width: m.Width, Height: m.Height, FPS: []uint32{fps}}}}, nil
}

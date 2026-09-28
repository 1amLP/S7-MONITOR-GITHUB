package cameramode

import "fmt"

// USBTrialMode is a separate engineering build, never a public mode unlock.
// Only the two original rear 720p high-frame-rate targets are admitted.
func USBTrialMode(fps uint32) (Mode, error) {
	if fps != 120 && fps != 240 {
		return Mode{}, fmt.Errorf("USB camera trial requires rear 720p120 or 720p240")
	}
	return Mode{Width: 1280, Height: 720, FPS: fps}, nil
}

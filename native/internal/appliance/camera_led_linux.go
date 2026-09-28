//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"os"
	"time"

	"perimode/native/internal/linuxio"
)

const cameraLEDPath = "/sys/class/sec/led/led_g"

func cameraLEDWanted(streaming bool, owner uint64, uncertain bool) bool {
	return (streaming && owner != 0) || uncertain
}

type CameraLEDRuntime struct {
	Available bool   `json:"driver_available"`
	On        bool   `json:"requested_on"`
	Error     string `json:"error,omitempty"`
}

func (s *State) CameraLEDSnapshot() CameraLEDRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.CameraLED
}

func (s *State) wakeCameraLEDLocked() {
	select {
	case s.cameraLEDWake <- struct{}{}:
	default:
	}
}

func (u *UI) startCameraLED() error {
	wake := make(chan struct{}, 1)
	u.state.mu.Lock()
	u.state.cameraLEDWake = wake
	u.state.mu.Unlock()
	return u.workers.Start("camera-activity-led", func(ctx context.Context) error {
		if _, err := os.Stat(cameraLEDPath); err != nil {
			u.state.mu.Lock()
			u.state.CameraLED.Error = err.Error()
			u.state.mu.Unlock()
			return nil
		}
		defer func() { _ = linuxio.WriteAttr(cameraLEDPath, "0\n") }()
		previous, initialized := false, false
		for {
			u.state.mu.Lock()
			s := u.state
			owner := s.cameraSelection.Owner(s.cameraEnvironmentLocked(), time.Now())
			// An unresolved DMA stop must not imply the camera has stopped.
			on := cameraLEDWanted(s.cameraUSBStreaming, owner, s.cameraOwnershipFault)
			u.state.mu.Unlock()
			if !initialized || on != previous {
				value := "0\n"
				if on {
					value = "12\n"
				}
				err := linuxio.WriteAttr(cameraLEDPath, value)
				u.state.mu.Lock()
				u.state.CameraLED = CameraLEDRuntime{Available: err == nil, On: on}
				if err != nil {
					u.state.CameraLED.Error = err.Error()
				}
				u.state.mu.Unlock()
				if err != nil {
					return nil
				}
				previous, initialized = on, true
			}
			select {
			case <-ctx.Done():
				return nil
			case <-wake:
			}
		}
	})
}

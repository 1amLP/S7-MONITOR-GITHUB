//go:build linux && (amd64 || arm64)

package appliance

import (
	"fmt"
	"perimode/native/internal/camera"
	"perimode/native/pkg/cameractl"
	"perimode/native/pkg/camerawire"
	"perimode/native/pkg/usb/uvcmode"
	"time"
)

func (s *State) cameraEnvironmentLocked() cameractl.Environment {
	e := cameractl.Environment{Sensor: byte(s.Camera.Settings.Sensor), Mode: cameractl.Mode(s.Camera.Settings.Mode), Generation: s.Camera.Epoch}
	if s.Camera.Enabled {
		e.Flags |= cameractl.Enabled
	}
	if s.cameraUSBReady {
		e.Flags |= cameractl.USBReady
	}
	if s.cameraUSBStreaming {
		e.Flags |= cameractl.Streaming
	}
	if s.ThermalPaused || s.CameraTrial.Active {
		e.Flags |= cameractl.Paused
	}
	if s.cameraOwnershipFault {
		e.Flags |= cameractl.OwnershipFault
	}
	// Admission cannot be inferred from a mode name or from another sensor.
	for _, v := range s.CameraModes {
		if v.Admitted && v.Mode.WebcamEligible() && v.Sensor <= camera.Front {
			b := cameractl.ModeBit(byte(v.Sensor), cameractl.Mode(v.Mode))
			e.Masks[v.Sensor] |= b & s.cameraPublished[v.Sensor]
		}
	}
	return e
}
func (s *State) CameraControlStatus() [cameractl.ReportBytes]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cameraSelection.Snapshot(s.cameraEnvironmentLocked(), time.Now()).Marshal()
}

// CameraControlCommand does not start hardware or acquire Transport/touch locks.
// GET_REPORT returns an explicit command result. EP0 completion is not success.
func (s *State) CameraControlCommand(data []byte) error {
	c, err := cameractl.Parse(data)
	if err != nil {
		return err
	}
	if c.Kind == cameractl.Status {
		return fmt.Errorf("camera status report is read-only")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	env := s.cameraEnvironmentLocked()
	previousOwner := s.cameraSelection.Owner(env, now)
	previousEpoch := s.Camera.Epoch
	change, _ := s.cameraSelection.Apply(c, env, now)
	if change {
		v := s.Camera.Settings
		v.Sensor = camera.Sensor(c.Sensor)
		v.Mode = camera.Mode(c.Mode)
		s.configureCameraLocked(v)
		// A fresh Windows capture is an explicit retry even at identical mode.
		// Repeated Acquire from its current owner must not restart live capture.
		if previousOwner != c.Token && s.Camera.Epoch == previousEpoch {
			s.Camera.Epoch++
			s.Camera.Counters = camera.Counters{}
			s.Camera.LastActivity = time.Time{}
		}
		s.Camera.Status = "WINDOWS SELECTED " + v.Sensor.String() + " / WAITING FOR UVC"
	}
	s.wakeCameraLEDLocked()
	return nil
}
func (s *State) cameraPublishedTable(c camera.Catalog, t uvcmode.Table) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.CameraModes = append(camera.Catalog(nil), c...)
	s.cameraPublished = [2]byte{}
	for _, v := range c {
		if v.Admitted && v.Mode.WebcamEligible() && v.Sensor <= camera.Front && t.Contains(uvcmode.Mode(v.Mode)) {
			s.cameraPublished[v.Sensor] |= cameractl.ModeBit(byte(v.Sensor), cameractl.Mode(v.Mode))
		}
	}
	s.cameraUSBReady = true
}
func (s *State) cameraStreaming(active bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A failed stop is sticky until reboot; a status bit must never fake DMA release.
	if err != nil {
		s.cameraOwnershipFault = true
		s.wakeCameraLEDLocked()
		return
	}
	if s.cameraUSBStreaming != active {
		s.cameraUSBStreaming = active
		s.wakeCameraLEDLocked()
	}
}
func (s *State) CameraControlDisconnected() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cameraSelection.Reset()
	s.wakeCameraLEDLocked()
	// Streaming/DMA ownership is cleared by the camera worker, not a USB callback.
}

func (s *State) cameraWireAuthorize(c camerawire.Command) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	env := s.cameraEnvironmentLocked()
	if env.Flags&cameractl.Enabled == 0 || env.Flags&(cameractl.Paused|cameractl.OwnershipFault) != 0 ||
		!s.cameraSelection.Fresh(c.Token, time.Now()) || c.Mode != s.Camera.Settings.Mode {
		return fmt.Errorf("camera WinUSB owner or active mode differs")
	}
	return nil
}

func (s *State) cameraWireMode(token uint64) (camera.Mode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	env := s.cameraEnvironmentLocked()
	if env.Flags&cameractl.Enabled == 0 || env.Flags&(cameractl.Paused|cameractl.OwnershipFault) != 0 || !s.cameraSelection.Fresh(token, time.Now()) || !cameractl.Allowed(env.Sensor, env.Mode, env.Masks) {
		return camera.Mode{}, fmt.Errorf("camera WinUSB owner unavailable")
	}
	return camera.Mode(env.Mode), nil
}

func (s *State) cameraUSBRemoved() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cameraSelection.Reset()
	s.cameraPublished = [2]byte{}
	s.cameraUSBReady = false
	s.cameraDynamicFormat = false
}

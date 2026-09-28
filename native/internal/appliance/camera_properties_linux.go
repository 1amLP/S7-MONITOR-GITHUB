//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"math"

	"perimode/native/internal/camera"
	"perimode/native/internal/camera/fimcshot"
	"perimode/native/pkg/cameraprop"
)

type cameraPropertyResult struct {
	request, reply cameraprop.Message
}

func (s *State) rememberCameraPropertyLocked(q, r cameraprop.Message) {
	index := int(s.cameraPropertyResultNext) % len(s.cameraPropertyResults)
	for i, previous := range s.cameraPropertyResults {
		if previous.request.Token == q.Token {
			index = i
			break
		}
	}
	s.cameraPropertyResults[index] = cameraPropertyResult{q, r}
	s.cameraPropertyResultNext = uint8((index + 1) % len(s.cameraPropertyResults))
}

func (s *State) cameraPropertyCommand(data []byte) error {
	m, err := cameraprop.Parse(data)
	if err != nil || m.Kind == cameraprop.Reply {
		return cameraprop.ErrWire
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cameraPropertyJobs == nil {
		return fmt.Errorf("camera property worker unavailable")
	}
	if s.cameraPropertyRequest == m {
		return nil
	}
	// Keep the in-flight ACK intact. Concurrent clients retry after its owner
	// observes completion; a second SET must never replace a pending command.
	if s.cameraPropertyAck.Result == cameraprop.Pending {
		return fmt.Errorf("camera property command busy")
	}
	for _, previous := range s.cameraPropertyResults {
		if previous.request.Token != m.Token || m.Sequence > previous.request.Sequence {
			continue
		}
		if previous.request == m {
			s.cameraPropertyRequest, s.cameraPropertyAck = m, previous.reply
			return nil
		}
		return fmt.Errorf("stale camera property sequence")
	}
	ack := m
	ack.Kind = cameraprop.Reply
	ack.Result = cameraprop.Pending
	select {
	case s.cameraPropertyJobs <- m:
		s.cameraPropertyRequest = m
		s.cameraPropertyAck = ack
	default:
		return fmt.Errorf("camera property queue busy")
	}
	return nil
}
func (s *State) cameraPropertyStatus() [cameraprop.Size]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.cameraPropertyAck
	if r.Kind == 0 {
		r = cameraprop.Message{Kind: cameraprop.Reply, Property: cameraprop.Exposure, Sensor: cameraprop.ActiveSensor, Result: cameraprop.Pending}
	}
	return r.Marshal()
}
func (u *UI) startCameraProperties() error {
	jobs := make(chan cameraprop.Message, 1)
	u.cameraPropertiesChanged = make(chan struct{}, 1)
	u.state.mu.Lock()
	u.state.cameraPropertyJobs = jobs
	u.state.mu.Unlock()
	return u.workers.Start("camera-properties", func(ctx context.Context) error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case q := <-jobs:
				r := u.cameraProperty(q)
				u.state.mu.Lock()
				u.state.rememberCameraPropertyLocked(q, r)
				if u.state.cameraPropertyAck.Sequence == q.Sequence && u.state.cameraPropertyAck.Token == q.Token {
					u.state.cameraPropertyAck = r
				}
				u.state.mu.Unlock()
				if q.Kind == cameraprop.Set && r.Result == cameraprop.OK {
					select {
					case u.cameraPropertiesChanged <- struct{}{}:
					default:
					}
				}
			}
		}
	})
}

func (u *UI) cameraProperty(q cameraprop.Message) cameraprop.Message {
	u.cameraControlsMu.Lock()
	defer u.cameraControlsMu.Unlock()
	r := q
	r.Kind = cameraprop.Reply
	r.Result = cameraprop.Unsupported
	service, ok := u.cameraProvider.(cameraControlService)
	if !ok {
		return r
	}
	observed := u.state.CameraCurrent()
	settings := observed.Settings
	if q.Sensor != cameraprop.ActiveSensor {
		settings.Sensor = camera.Sensor(q.Sensor)
	}
	u.state.mu.Lock()
	if settings.Sensor != u.state.Camera.Settings.Sensor {
		settings.Image = u.state.CameraImages[settings.Sensor]
	}
	u.state.mu.Unlock()
	if !settings.Mode.Valid(settings.Sensor) {
		settings.Mode = camera.DefaultSettings().Mode
	}
	view, err := service.ControlState(settings)
	if err != nil {
		return r
	}
	l, v := view.Descriptor.Limits, view.Effective
	supported := uint32(0)
	current := uint32(cameraprop.Manual)
	r.Min, r.Max, r.Step = 0, 0, 1
	refresh := func() {
		r.Sensor = byte(settings.Sensor)
		switch q.Property {
		case cameraprop.Exposure:
			supported = cameraprop.Auto
			if l.ManualExposure {
				supported |= cameraprop.Manual
			}
			r.Min = int64(l.ExposureMinNS)
			r.Max = int64(min(l.ExposureMaxNS, l.FrameDurationNS))
			r.Value = int64(v.ExposureNS)
			if v.ExposureNS == 0 {
				current = cameraprop.Auto
				if view.Sensor.Available {
					r.Value = int64(view.Sensor.ExposureNS)
				}
			}
		case cameraprop.ISO:
			supported = cameraprop.Auto
			if l.ManualISO && l.ManualExposure {
				supported |= cameraprop.Manual
			}
			r.Min = int64(l.ISOMin)
			r.Max = int64(l.ISOMax)
			r.Value = int64(v.ISO)
			if v.ISO == 0 {
				current = cameraprop.Auto
				if view.Sensor.Available {
					r.Value = int64(view.Sensor.ISO)
				}
			}
		case cameraprop.Focus:
			r.Step = 100 // Same 0.1 dioptre increment as the phone menu.
			if l.ManualFocus {
				supported |= cameraprop.Manual
			}
			if l.FocusModes&(1<<fimcshot.FocusSingle) != 0 {
				supported |= cameraprop.Auto
			}
			if l.FocusModes&(1<<fimcshot.FocusContinuousVideo) != 0 {
				supported |= cameraprop.Continuous
			}
			r.Max = int64(math.Round(float64(l.MaxFocusDioptres) * 1000))
			r.Value = int64(math.Round(float64(max(v.FocusDioptres, 0)) * 1000))
			if v.Focus == fimcshot.FocusContinuousVideo || v.Focus == fimcshot.FocusContinuousPicture {
				current = cameraprop.Continuous
			} else if v.Focus != fimcshot.FocusOff {
				current = cameraprop.Auto
			}
		case cameraprop.WhiteBalance:
			supported = cameraprop.Auto
			if l.WBModeMask & ^uint32(1<<fimcshot.WBAuto) != 0 {
				supported |= cameraprop.Manual
			}
			if l.AWBLock {
				supported |= cameraprop.Locked
			}
			r.Min = 2
			r.Max = 9
			r.Value = int64(v.WhiteBalance)
			if v.WhiteBalance == fimcshot.WBAuto {
				current = cameraprop.Auto
			}
			if v.AWBLocked {
				current |= cameraprop.Locked
			}
		case cameraprop.AELock:
			if l.AELock {
				supported = cameraprop.Manual
			}
			r.Max = 1
			if v.AELocked {
				r.Value = 1
			} else {
				r.Value = 0
			}
		case cameraprop.AWBLock:
			if l.AWBLock {
				supported = cameraprop.Manual
			}
			r.Max = 1
			if v.AWBLocked {
				r.Value = 1
			} else {
				r.Value = 0
			}
		case cameraprop.Compensation:
			supported = cameraprop.Manual
			r.Step = int64(math.Round(float64(view.Descriptor.EVStep) * 1000))
			r.Min = int64(l.CompensationMin) * r.Step
			r.Max = int64(l.CompensationMax) * r.Step
			r.Value = int64(v.Compensation) * r.Step
		case cameraprop.Zoom:
			supported = cameraprop.Manual
			r.Min = 100
			r.Max = 400
			r.Step = 25
			r.Value = int64(settings.Image.Zoom())
		case cameraprop.Mirror:
			supported = cameraprop.Manual
			r.Max = 1
			if settings.Image.Mirror {
				r.Value = 1
			} else {
				r.Value = 0
			}
		case cameraprop.Rotation:
			supported = cameraprop.Manual
			r.Max = 270
			r.Step = 90
			r.Value = int64(settings.Image.Rotation)
		case cameraprop.PowerLine:
			supported = cameraprop.Manual
			r.Max = 3
			r.Value = int64(v.AntiBand) - 1
			if v.AntiBand == 0 {
				r.Value = 3
			}
		case cameraprop.Brightness, cameraprop.Contrast, cameraprop.Gamma, cameraprop.Sharpness:
			if l.ISPImageControls {
				supported = cameraprop.Manual
				switch q.Property {
				case cameraprop.Brightness:
					r.Min, r.Max, r.Step, r.Value = -100, 100, 1, int64(v.Brightness)
				case cameraprop.Contrast:
					r.Min, r.Max, r.Step, r.Value = 10, 200, 1, int64(v.Contrast)
					if r.Value == 0 {
						r.Value = 100
					}
				case cameraprop.Gamma:
					r.Min, r.Max, r.Step, r.Value = 50, 300, 1, int64(v.Gamma)
					if r.Value == 0 {
						r.Value = 100
					}
				case cameraprop.Sharpness:
					r.Max, r.Value = 10, int64(v.Sharpness)
				}
			}
		case cameraprop.Temperature:
			if l.WBModeMask&(1<<fimcshot.WBCustomK) != 0 {
				supported = cameraprop.Auto | cameraprop.Manual
				r.Min, r.Max, r.Step, r.Value = 2000, 10000, 100, int64(v.WBTemperature)
				if r.Value == 0 {
					r.Value = 6500
				}
				if v.WhiteBalance == fimcshot.WBAuto {
					current = cameraprop.Auto
				}
				if v.WhiteBalance != fimcshot.WBAuto && v.WhiteBalance != fimcshot.WBCustomK {
					supported = 0
				}
			}
		}
		r.Flags = current | (supported << 8)
		if q.Property == cameraprop.WhiteBalance {
			r.Flags |= l.WBModeMask << 16
		}
		if q.Property == cameraprop.Focus {
			r.Flags |= l.FocusModes << 16
		}
	}
	refresh()
	if supported == 0 {
		return r
	}
	if q.Kind == cameraprop.Set {
		if q.Flags == 0 || q.Flags & ^supported != 0 {
			r.Result = cameraprop.Invalid
			return r
		}
		if q.Flags&cameraprop.Auto == 0 && q.Flags&cameraprop.Continuous == 0 && (q.Value < r.Min || q.Value > r.Max || r.Step <= 0 || (q.Value-r.Min)%r.Step != 0) {
			r.Result = cameraprop.Invalid
			return r
		}
		manualExposure := func() {
			if v.ExposureNS == 0 {
				v.ExposureNS = max(l.ExposureMinNS, min(l.ExposureMaxNS, l.FrameDurationNS/2))
			}
			if v.ISO == 0 && l.ManualISO {
				v.ISO = max(l.ISOMin, min(l.ISOMax, 100))
			}
			v.AELocked = false
			v.AERegion = fimcshot.Region{}
			v.Compensation = 0
		}
		switch q.Property {
		case cameraprop.Exposure:
			if q.Flags == cameraprop.Auto {
				v.ExposureNS = 0
				v.ISO = 0
			} else if q.Flags == cameraprop.Manual {
				manualExposure()
				v.ExposureNS = uint64(q.Value)
			} else {
				r.Result = cameraprop.Invalid
				return r
			}
		case cameraprop.ISO:
			if q.Flags == cameraprop.Auto {
				v.ExposureNS = 0
				v.ISO = 0
			} else if q.Flags == cameraprop.Manual {
				manualExposure()
				v.ISO = uint32(q.Value)
			} else {
				r.Result = cameraprop.Invalid
				return r
			}
		case cameraprop.Focus:
			v.AFRegion = fimcshot.Region{}
			v.Trigger = fimcshot.FocusIdle
			v.FocusDioptres = -1
			if q.Flags == cameraprop.Manual {
				v.Focus = fimcshot.FocusOff
				v.FocusDioptres = float32(q.Value) / 1000
			} else if q.Flags == cameraprop.Continuous {
				v.Focus = fimcshot.FocusContinuousVideo
			} else if q.Flags == cameraprop.Auto {
				v.Focus = fimcshot.FocusSingle
			} else {
				r.Result = cameraprop.Invalid
				return r
			}
		case cameraprop.WhiteBalance:
			if q.Flags != cameraprop.Auto && q.Flags != cameraprop.Manual && q.Flags != cameraprop.Auto|cameraprop.Locked {
				r.Result = cameraprop.Invalid
				return r
			}
			v.AWBLocked = q.Flags&cameraprop.Locked != 0
			if q.Flags&cameraprop.Auto != 0 {
				v.WhiteBalance = fimcshot.WBAuto
			} else {
				v.WhiteBalance = fimcshot.WhiteBalance(q.Value)
			}
		case cameraprop.AELock:
			v.AELocked = q.Value != 0
		case cameraprop.AWBLock:
			v.AWBLocked = q.Value != 0
		case cameraprop.Compensation:
			v.Compensation = int32(q.Value / r.Step)
		case cameraprop.Zoom:
			settings.Image.ZoomPercent = uint16(q.Value)
		case cameraprop.Mirror:
			settings.Image.Mirror = q.Value != 0
		case cameraprop.Rotation:
			settings.Image.Rotation = uint16(q.Value)
		case cameraprop.PowerLine:
			v.AntiBand = uint32(q.Value) + 1
		case cameraprop.Brightness:
			v.Brightness = int32(q.Value)
		case cameraprop.Contrast:
			v.Contrast = uint32(q.Value)
		case cameraprop.Gamma:
			v.Gamma = uint32(q.Value)
		case cameraprop.Sharpness:
			v.Sharpness = uint32(q.Value)
		case cameraprop.Temperature:
			if q.Flags == cameraprop.Auto {
				v.WhiteBalance = fimcshot.WBAuto
			} else if q.Flags == cameraprop.Manual {
				v.WhiteBalance = fimcshot.WBCustomK
				v.WBTemperature = uint32(q.Value)
			} else {
				r.Result = cameraprop.Invalid
				return r
			}
			v.AWBLocked = false
		}
		if q.Property >= cameraprop.Zoom && q.Property <= cameraprop.Rotation {
			settings, err = u.state.cameraPropertyImage(q.Sensor, q.Property, q.Value)
		} else {
			err = service.SetControls(settings, camera.ControlSelection{Custom: true, Values: v})
		}
		if err == nil && q.Property == cameraprop.Focus && q.Flags == cameraprop.Auto {
			err = service.TriggerFocus(settings, fimcshot.FocusStart)
		}
		if err != nil {
			r.Result = cameraprop.Invalid
			if errors.Is(err, camera.ErrCaptureBusy) || errors.Is(err, camera.ErrFocusPending) {
				r.Result = cameraprop.Busy
			} else if errors.Is(err, camera.ErrOwnership) {
				r.Result = cameraprop.Failed
			}
			return r
		}
		view, err = service.ControlState(settings)
		if err != nil {
			r.Result = cameraprop.Failed
			return r
		}
		v = view.Effective
		l = view.Descriptor.Limits
		current = cameraprop.Manual
		refresh()
	}
	r.Result = cameraprop.OK
	prefs := service.ControlPreferences()
	u.state.mu.Lock()
	if camera.Key(u.state.Camera.Settings) == camera.Key(settings) {
		u.state.Camera3A = Camera3ARuntime{View: view, Ready: true}
	}
	u.state.Camera3APreferences = prefs
	u.state.mu.Unlock()
	return r
}

// Change one field under the state lock. Pre-start settings for the other
// endpoint persist without stopping the active camera or restoring an old mode.
func (s *State) cameraPropertyImage(target, property byte, value int64) (camera.Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.CameraTrial.Active {
		return camera.Settings{}, camera.ErrCaptureBusy
	}
	if target == cameraprop.ActiveSensor {
		target = byte(s.Camera.Settings.Sensor)
	}
	if target > 1 {
		return camera.Settings{}, cameraprop.ErrWire
	}
	v := s.Camera.Settings
	active := byte(v.Sensor) == target
	if !active {
		v.Sensor = camera.Sensor(target)
		v.Image = s.CameraImages[target]
		if !v.Mode.Valid(v.Sensor) {
			v.Mode = camera.DefaultSettings().Mode
		}
	}
	switch property {
	case cameraprop.Zoom:
		if value < 100 || value > 400 {
			return v, cameraprop.ErrWire
		}
		v.Image.ZoomPercent = uint16(value)
	case cameraprop.Mirror:
		if value < 0 || value > 1 {
			return v, cameraprop.ErrWire
		}
		v.Image.Mirror = value != 0
	case cameraprop.Rotation:
		if value < 0 || value > 270 || value%90 != 0 {
			return v, cameraprop.ErrWire
		}
		v.Image.Rotation = uint16(value)
	default:
		return v, cameraprop.ErrWire
	}
	if err := v.Image.Validate(); err != nil {
		return v, err
	}
	s.CameraImages[target] = v.Image
	if active {
		s.configureCameraLocked(v)
	}
	return v, nil
}

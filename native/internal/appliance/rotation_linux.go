//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"perimode/native/internal/orientation"
	"perimode/native/internal/sensorhub"
)

type rotationEvent struct {
	serial         uint64
	sample         orientation.Sample
	hasSample      bool
	source, status string
}

func (s *State) RotationCurrent() (orientation.Settings, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Rotation, s.RotationSerial
}
func (u *UI) rotationLines() []string {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	s := u.state
	mode := "MANUAL"
	if s.Rotation.Automatic {
		mode = "AUTOMATIC"
	}
	lines := []string{"DEVICE / SCREEN ROTATION", "MODE: " + mode + " / TAP TO CHANGE"}
	for _, d := range []int{0, 90, 180, 270} {
		mark := ""
		if !s.Rotation.Automatic && int(s.Rotation.Manual) == d {
			mark = " / SELECTED"
		}
		lines = append(lines, fmt.Sprintf("MANUAL: %d DEG%s", d, mark))
	}
	return append(lines, "LOCK CURRENT ORIENTATION", fmt.Sprintf("ACTIVE: %d DEG / LOCAL PANEL", s.ActiveRotation), "SENSOR: "+s.RotationStatus, "RETRY AUTO SENSOR", u.hubStatus(), "BACK: DEVICE")
}
func (u *UI) selectRotation(row int) {
	s, _ := u.state.RotationCurrent()
	switch row {
	case 1:
		s.Automatic = !s.Automatic
	case 2, 3, 4, 5:
		s.Automatic = false
		s.Manual = orientation.Degrees((row - 2) * 90)
	case 6:
		u.state.mu.Lock()
		s.Manual = u.state.ActiveRotation
		u.state.mu.Unlock()
		s.Automatic = false
	case 9:
		if !s.Automatic {
			u.state.mu.Lock()
			u.state.RotationStatus = "MANUAL / SELECT AUTO TO RETRY"
			u.state.mu.Unlock()
			u.draw()
			return
		}
	default:
		return
	}
	u.state.Error(u.configureRotation(s))
	u.draw()
}
func (u *UI) configureRotation(v orientation.Settings) error {
	if e := v.Validate(); e != nil {
		return e
	}
	u.orientationFilter.Reset()
	u.state.mu.Lock()
	autoChanged := u.state.Rotation.Automatic != v.Automatic
	u.state.Rotation = v
	u.state.RotationSerial++
	u.state.RotationLastSample = time.Time{}
	if v.Automatic {
		u.state.RotationStatus = "WAITING FOR FRESH SSP DATA"
	} else {
		u.state.RotationStatus = "MANUAL / SENSOR OFF"
	}
	u.state.mu.Unlock()
	if autoChanged {
		if v.Automatic {
			u.notify("AUTO ROTATE ON")
		} else {
			u.notify("AUTO ROTATE OFF")
		}
	}
	if !v.Automatic {
		return u.applyRotation(v.Manual)
	}
	return nil
}
func (u *UI) applyRotation(d orientation.Degrees) error {
	if !d.Valid() {
		return fmt.Errorf("invalid device rotation")
	}
	u.presentationMu.Lock()
	defer u.presentationMu.Unlock()
	if u.screen == nil {
		return fmt.Errorf("device rotation: framebuffer unavailable")
	}
	_, _, before := u.screen.Geometry()
	if before == d {
		u.state.mu.Lock()
		u.state.ActiveRotation = d
		u.state.mu.Unlock()
		return nil
	}
	// No change to monitor settings/generation, camera epoch, USB or panel timing.
	if e := u.screen.SetRotation(d); e != nil {
		return e
	}
	u.state.mu.Lock()
	u.state.ActiveRotation = d
	u.state.RotationChanges++
	u.state.mu.Unlock()
	if u.transport != nil {
		u.transport.touch.blockInput()
	}
	u.pad.Reset()
	u.suppressed = 0
	u.touchDown = false
	u.touchRow = -1
	u.inputAwaitAllUp = true
	u.inputEpoch.Add(1)
	for {
		select {
		case <-u.frames:
		default:
			return nil
		}
	}
}
func (u *UI) handleRotationEvent(ev rotationEvent) {
	cfg, serial := u.state.RotationCurrent()
	if !cfg.Automatic || serial != ev.serial {
		return
	}
	if !ev.hasSample {
		u.orientationFilter.Reset()
		u.state.mu.Lock()
		u.state.RotationLastSample = time.Time{}
		u.state.RotationStatus = ev.status
		u.state.RotationSource = ev.source
		u.state.mu.Unlock()
		return
	}
	d, ready, status := u.orientationFilter.Consider(ev.sample, time.Now())
	u.state.mu.Lock()
	u.state.RotationSamples++
	u.state.RotationSource = ev.source
	u.state.RotationStatus = status
	if u.orientationFilter.Fresh(time.Now()) {
		u.state.RotationLastSample = u.orientationFilter.LastReceived()
	}
	active := u.state.ActiveRotation
	u.state.mu.Unlock()
	if ready && d != active {
		if e := u.applyRotation(d); e != nil {
			u.state.Error(e)
		}
		u.draw()
	}
}
func (u *UI) checkRotationFreshness(now time.Time) {
	cfg, _ := u.state.RotationCurrent()
	if !cfg.Automatic {
		return
	}
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	if !u.state.RotationLastSample.IsZero() && now.Sub(u.state.RotationLastSample) > orientation.StaleAfter {
		u.state.RotationStatus = "STALE SENSOR / HOLD LAST ORIENTATION"
	}
}
func (u *UI) sendRotationEvent(ctx context.Context, ev rotationEvent) {
	// Latest sensor sample wins. Old samples are not a queue of rotation commands.
	if ctx.Err() != nil {
		return
	}
	select {
	case u.orientationEvents <- ev:
		return
	default:
	}
	select {
	case <-u.orientationEvents:
	default:
	}
	select {
	case u.orientationEvents <- ev:
	case <-ctx.Done():
	default:
	}
}
func (u *UI) orientationWorker(ctx context.Context) {
	factory := u.sensorFactory
	if factory == nil {
		factory = func() (orientation.SampleSource, error) { return u.openHubOrientation(ctx) }
	}
	var source orientation.SampleSource
	var serial uint64
	haveSerial := false
	attempts := 0
	haveData := false
	closeFailed := false
	var lastData, retryAt time.Time
	closeSource := func() error {
		if source == nil {
			return nil
		}
		e := source.Close()
		closeFailed = e != nil
		if e == nil {
			source = nil
		}
		return e
	}
	defer func() { u.state.Error(closeSource()) }()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		cfg, current := u.state.RotationCurrent()
		if !haveSerial || serial != current {
			restoreErr := closeSource()
			serial = current
			haveSerial = true
			attempts = 0
			if restoreErr != nil {
				attempts = 3
				u.sendRotationEvent(ctx, rotationEvent{serial: current, status: "SENSOR RESTORE ERROR: " + restoreErr.Error()})
			}
			retryAt = time.Time{}
		}
		if !cfg.Automatic || closeFailed {
			continue
		}
		now := time.Now()
		if source == nil {
			if attempts >= 3 || now.Before(retryAt) {
				continue
			}
			attempts++
			s, e := factory()
			now = time.Now()
			currentCfg, currentSerial := u.state.RotationCurrent()
			if ctx.Err() != nil || !currentCfg.Automatic || currentSerial != serial {
				source = s
				u.state.Error(errors.Join(sensorCancelError(e), closeSource()))
				continue // Do not consume one stale sample after a manual switch.
			}
			if e == nil && s == nil {
				e = fmt.Errorf("accelerometer factory returned nil source")
			}
			if e != nil {
				// A partial open may still own a live IIO queue. Keep it for
				// retryable cleanup; never drop the owner on an error result.
				if s != nil {
					source = s
					e = errors.Join(e, closeSource())
				}
				retryAt = now.Add(2 * time.Second)
				u.sendRotationEvent(ctx, rotationEvent{serial: serial, status: fmt.Sprintf("AUTO UNAVAILABLE (%d/3): %v", attempts, e)})
				continue
			}
			source = s
			haveData = false
			lastData = now
			u.sendRotationEvent(ctx, rotationEvent{serial: serial, source: s.Name(), status: "WAITING FOR FRESH SSP DATA"})
		}
		sample, ok, e := source.Poll(now)
		if completed := time.Now(); completed.After(now) {
			now = completed
		}
		limit := 1500 * time.Millisecond
		if !haveData && u.hub != nil {
			limit = sensorhub.StartupGrace
		}
		if e == nil && !ok && now.Sub(lastData) > limit {
			e = fmt.Errorf("no fresh accelerometer samples within %v; inspect hub status", limit)
		}
		if e != nil {
			name := source.Name()
			closeErr := closeSource()
			retryAt = now.Add(2 * time.Second)
			if closeErr != nil {
				e = fmt.Errorf("%w; restore: %v", e, closeErr)
				attempts = 3
			}
			u.sendRotationEvent(ctx, rotationEvent{serial: serial, source: name, status: fmt.Sprintf("AUTO UNAVAILABLE (%d/3): %v", attempts, e)})
			continue
		}
		if ok {
			haveData = true
			lastData = now
			u.sendRotationEvent(ctx, rotationEvent{serial: serial, source: source.Name(), sample: sample, hasSample: true})
		}
	}
}

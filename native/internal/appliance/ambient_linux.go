//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"perimode/native/internal/ambient"
	"perimode/native/internal/sensorhub"
)

// AmbientRuntime contains observations, not saved panel commands. Automatic
// writes never replace the user's saved manual level.
type AmbientRuntime struct {
	Settings                  ambient.Settings
	Revision                  uint64
	Fallback                  int
	Status, Source, LastError string
	LastSample                ambient.Sample
	Writes                    uint64
}

func (s *State) ambientCurrent() (AmbientRuntime, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Ambient, s.BrightnessActual
}

func (u *UI) autoBrightnessLines() []string {
	a, actual := u.state.ambientCurrent()
	lux := "LUX: UNKNOWN"
	age := "SAMPLE: NOT RECEIVED"
	if !a.LastSample.Received.IsZero() {
		lux = fmt.Sprintf("LUX: %d", a.LastSample.Lux)
		d := time.Since(a.LastSample.Received)
		age = fmt.Sprintf("SAMPLE AGE: %.1F S", d.Seconds())
		if d > ambient.StaleAfter || d < 0 {
			age += " / STALE"
		}
	}
	return []string{"DEVICE / DISPLAY / AUTO BRIGHTNESS", fmt.Sprintf("AUTOMATIC: %t / TAP TO CHANGE", a.Settings.Automatic), fmt.Sprintf("MINIMUM: %d PCT / TAP +5", a.Settings.Minimum), fmt.Sprintf("MAXIMUM: %d PCT / TAP +5", a.Settings.Maximum), fmt.Sprintf("AUTO BIAS: %d PCT / DIMMER -5", a.Settings.Bias), "AUTO BIAS: BRIGHTER +5", lux + fmt.Sprintf(" / PANEL: %d PCT", actual), age, "SENSOR: " + a.Source, "RETRY LIGHT SENSOR", "STATUS: " + a.Status, "ERROR: " + a.LastError, "MANUAL LEVEL IS KEPT SEPARATELY", u.hubStatus(), "BACK: DISPLAY"}
}
func (u *UI) selectAutoBrightness(row int) {
	if row != 1 && row != 2 && row != 3 && row != 4 && row != 5 && row != 9 {
		return
	}
	u.async(func() error {
		a, _ := u.state.ambientCurrent()
		v := a.Settings
		switch row {
		case 1:
			v.Automatic = !v.Automatic
		case 2:
			v.Minimum += 5
			if v.Minimum > v.Maximum {
				v.Minimum = 5
			}
		case 3:
			v.Maximum += 5
			if v.Maximum > 100 {
				v.Maximum = v.Minimum
			}
		case 4:
			v.Bias = max(-30, v.Bias-5)
		case 5:
			v.Bias = min(30, v.Bias+5)
		case 9:
			if !v.Automatic {
				return fmt.Errorf("select automatic brightness before retry")
			}
		}
		return u.configureAutoBrightness(v)
	})
}
func (u *UI) configureAutoBrightness(v ambient.Settings) error {
	if e := v.Validate(); e != nil {
		return e
	}
	u.panelMu.Lock()
	defer u.panelMu.Unlock()
	if u.panel == nil {
		return fmt.Errorf("panel backend unavailable; auto brightness not enabled")
	}
	actual, e := u.panel.Read()
	if e != nil {
		return e
	}
	u.state.mu.Lock()
	previous := u.state.Ambient
	fallback := previous.Fallback
	if fallback < 5 || !previous.Settings.Automatic {
		fallback = u.state.Brightness
		if fallback == 0 {
			fallback = max(5, actual)
		}
	}
	u.state.Ambient.Settings = v
	u.state.Ambient.Revision++
	u.state.Ambient.Fallback = fallback
	u.state.Ambient.LastSample = ambient.Sample{}
	u.state.Ambient.LastError = ""
	u.state.Ambient.Source = "NOT OPENED"
	u.state.BrightnessActual = actual
	if v.Automatic {
		u.state.Ambient.Status = "WAITING FOR FRESH LIGHT SENSOR"
	} else {
		u.state.Ambient.Status = "MANUAL / SENSOR OFF"
	}
	u.state.mu.Unlock()
	if !v.Automatic && previous.Settings.Automatic {
		if e = u.panel.Set(fallback); e == nil {
			actual, e = u.panel.Read()
		}
		u.state.mu.Lock()
		defer u.state.mu.Unlock()
		if e != nil {
			u.state.BrightnessStatus = "MANUAL RESTORE ERROR: " + e.Error()
			return e
		}
		u.state.BrightnessActual = actual
		u.state.BrightnessStatus = "MANUAL / RESTORED SAVED LEVEL"
	}
	return nil
}

// panelMu and the revision check serialize manual changes with automatic
// writes. A computed target from an old mode can never land after a manual tap.
func (u *UI) applyAutoBrightness(revision uint64, target int) (int, bool, error) {
	u.panelMu.Lock()
	defer u.panelMu.Unlock()
	a, _ := u.state.ambientCurrent()
	if !a.Settings.Automatic || a.Revision != revision {
		return 0, false, nil
	}
	if u.panel == nil {
		return 0, false, fmt.Errorf("panel backend unavailable")
	}
	if e := u.panel.Set(target); e != nil {
		return 0, false, e
	}
	actual, e := u.panel.Read()
	if e != nil {
		return 0, false, e
	}
	if actual < 5 || actual > 100 {
		return 0, false, fmt.Errorf("invalid automatic panel readback")
	}
	u.state.mu.Lock()
	u.state.BrightnessActual = actual
	u.state.BrightnessStatus = "AUTO / NORMAL RANGE / NO HBM"
	u.state.Ambient.Writes++
	u.state.mu.Unlock()
	return actual, true, nil
}

type ambientSession struct {
	ctx               context.Context
	u                 *UI
	source            ambient.Source
	controller        *ambient.Controller
	revision          uint64
	configured        bool
	attempts          int
	retryAt, lastData time.Time
	sourceError       string
	restoreFailed     bool
	writeFailed       bool
	gotData           bool
}

func (w *ambientSession) closeSource() error {
	if w.source == nil {
		return nil
	}
	e := w.source.Close()
	w.restoreFailed = e != nil
	if e == nil {
		w.source = nil
	}
	return e
}
func (w *ambientSession) report(status, source, detail string, sample *ambient.Sample) {
	w.u.state.mu.Lock()
	defer w.u.state.mu.Unlock()
	a := &w.u.state.Ambient
	if a.Revision != w.revision || !a.Settings.Automatic {
		return
	}
	a.Status = status
	a.Source = source
	a.LastError = detail
	if sample != nil {
		a.LastSample = *sample
	}
}
func (w *ambientSession) step(now time.Time) {
	a, actual := w.u.state.ambientCurrent()
	if !w.configured || w.revision != a.Revision {
		oldErr := w.closeSource()
		w.configured = true
		w.revision = a.Revision
		w.attempts = 0
		w.retryAt = time.Time{}
		w.sourceError = ""
		w.writeFailed = false
		w.controller = nil
		if oldErr != nil {
			w.u.state.Error(oldErr)
			w.u.state.mu.Lock()
			w.u.state.Ambient.LastError = oldErr.Error()
			if !a.Settings.Automatic {
				w.u.state.Ambient.Status = "MANUAL / SENSOR RESTORE ERROR"
			}
			w.u.state.mu.Unlock()
			w.attempts = 3
			w.sourceError = "SENSOR RESTORE FAILED: " + oldErr.Error()
		}
		if a.Settings.Automatic {
			var e error
			w.controller, e = ambient.NewController(a.Settings, actual, now)
			if e != nil {
				w.writeFailed = true
				w.report("AUTO CONTROLLER ERROR", "NONE", e.Error(), nil)
				return
			}
		}
	}
	if !a.Settings.Automatic || w.controller == nil {
		return
	}
	if w.writeFailed {
		return
	} // A failing actuator is latched until an explicit retry/mode change.
	sourceName := "NOT OPENED"
	if w.source == nil && w.attempts < 3 && !now.Before(w.retryAt) {
		factory := w.u.lightFactory
		if factory == nil {
			factory = func() (ambient.Source, error) {
				ctx := w.ctx
				if ctx == nil {
					ctx = w.u.background
				}
				if ctx == nil {
					ctx = context.Background()
				}
				return w.u.openHubLight(ctx)
			}
		}
		w.attempts++
		s, e := factory()
		// Hardware startup can consume the entire grace window. Do not stamp
		// new samples/retries with the ticker's pre-start time.
		if completed := time.Now(); completed.After(now) {
			now = completed
		}
		current, _ := w.u.state.ambientCurrent()
		if (w.ctx != nil && w.ctx.Err() != nil) || current.Revision != w.revision || !current.Settings.Automatic {
			w.source = s
			w.u.state.Error(errors.Join(sensorCancelError(e), w.closeSource()))
			return // A manual setting made during startup wins before any Poll.
		}
		if e != nil || s == nil {
			if e == nil {
				e = fmt.Errorf("light source factory returned nil")
			}
			if s != nil {
				w.source = s
				e = errors.Join(e, w.closeSource())
			}
			w.sourceError = e.Error()
			w.retryAt = now.Add(2 * time.Second)
		} else {
			w.source = s
			w.gotData = false
			w.lastData = now
			w.sourceError = ""
		}
	}
	if w.source != nil && !w.restoreFailed {
		sourceName = w.source.Name()
		sample, ok, e := w.source.Poll(now)
		if completed := time.Now(); completed.After(now) {
			now = completed
		}
		if e == nil && ok {
			e = w.controller.Accept(sample, now)
			if e == nil {
				w.lastData = now
				w.gotData = true
				w.report("FRESH LUX", sourceName, "", &sample)
			}
		}
		limit := ambient.StaleAfter
		if !w.gotData && w.u.hub != nil {
			limit = sensorhub.StartupGrace
		}
		if e == nil && !ok && now.Sub(w.lastData) > limit {
			e = fmt.Errorf("no fresh light samples within %v; inspect hub status", limit)
		}
		if e != nil {
			ce := w.closeSource()
			if ce != nil {
				w.attempts = 3
			}
			w.sourceError = errors.Join(e, ce).Error()
			w.retryAt = now.Add(2 * time.Second)
			w.controller.ResetSamples()
		}
	}
	fallback := a.Fallback
	if fallback < 5 {
		fallback = max(5, actual)
	}
	target, write, status := w.controller.Next(now, fallback)
	detail := w.sourceError
	if detail != "" {
		detail = fmt.Sprintf("ATTEMPT %d/3: %s", w.attempts, detail)
	}
	w.report(status, sourceName, detail, nil)
	if write {
		applied, did, e := w.u.applyAutoBrightness(w.revision, target)
		if e == nil && did {
			e = w.controller.Commit(applied, now)
		}
		if e != nil {
			w.writeFailed = true
			ce := w.closeSource()
			w.report("PANEL ERROR / AUTO WRITES STOPPED", sourceName, errors.Join(e, ce).Error(), nil)
			w.u.state.Error(e)
		}
	}
}
func (u *UI) ambientWorker(ctx context.Context) {
	w := &ambientSession{u: u, ctx: ctx}
	defer func() { u.state.Error(w.closeSource()) }()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.step(time.Now())
		}
	}
}

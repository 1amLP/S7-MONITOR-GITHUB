//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"perimode/native/internal/camera"
	"perimode/native/internal/fb"
	"perimode/native/internal/media"
	"perimode/native/internal/safety"
)

type PreviewRuntime struct {
	Stages    media.CameraStages `json:"source_stages"`
	Sensor    media.SensorResult `json:"sensor_result"`
	Enabled   bool               `json:"enabled"`
	Options   fb.PreviewOptions  `json:"options"`
	Retry     uint64             `json:"retry"`
	Status    string             `json:"status"`
	LastError string             `json:"last_error"`
	Copied    uint64             `json:"copied_thumbnails"`
	LastFrame time.Time          `json:"last_frame"`
}

func (s *State) PreviewCurrent() PreviewRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Preview
}
func (s *State) previewStatus(status string, e error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Preview.Status = status
	s.Preview.LastError = ""
	if e != nil {
		s.Preview.LastError = e.Error()
	}
}
func (u *UI) enablePreview(on bool) error {
	if on {
		if u.screen == nil {
			return fmt.Errorf("preview needs a framebuffer")
		}
		if u.cameraProvider == nil {
			return camera.ErrSensorGraph
		}
		if _, ok := u.cameraProvider.(interface {
			OpenPreview(camera.Settings) (camera.Source, error)
		}); !ok {
			return fmt.Errorf("camera provider has no shared local-preview source")
		}
		// Available may read Samsung sensor sysfs and start firmware loading.
		// Only the preview worker may perform that work; the UI feeds watchdog.
		if e := u.state.CameraCurrent().Settings.Validate(); e != nil {
			return e
		}
	}
	u.state.mu.Lock()
	if on && u.state.CameraTrial.Poisoned {
		u.state.mu.Unlock()
		return camera.ErrOwnership
	}
	if on && u.state.CameraTrial.Active {
		u.state.mu.Unlock()
		return camera.ErrCaptureBusy
	}
	wasVisible := u.state.monitorVisibleLocked()
	u.state.Preview.Enabled = on
	if on && u.state.Preview.Options.Fullscreen && u.state.Camera.Enabled {
		u.state.setViewLocked(ViewCamera)
	}
	if !on && u.state.View == ViewCamera {
		u.state.setViewLocked(ViewMonitor)
	}
	if wasVisible != u.state.monitorVisibleLocked() {
		u.state.invalidateMonitorLocked()
		u.state.resyncLocked()
	}
	u.state.Preview.Retry++
	u.state.Preview.LastFrame = time.Time{}
	u.state.Preview.LastError = ""
	switch {
	case !on:
		u.state.Preview.Status = "DISABLED"
	default:
		u.state.Preview.Status = "STARTING CAMERA"
	}
	u.state.mu.Unlock()
	log.Printf("S7 preview request enabled=%t", on)
	return nil
}
func (u *UI) previewLines() []string {
	p := u.state.PreviewCurrent()
	if p.Options.Fullscreen {
		return []string{"PREVIEW", fmt.Sprintf("ENABLED: %t", p.Enabled), "WINDOW: FULLSCREEN", "BACK: CAMERA"}
	}
	return []string{"PREVIEW", fmt.Sprintf("ENABLED: %t", p.Enabled), "WINDOW: SMALL", fmt.Sprintf("WINDOW SIZE: %d%%", p.Options.WidthPercent), fmt.Sprintf("WINDOW POSITION: %s", []string{"TOP LEFT", "TOP RIGHT", "BOTTOM LEFT", "BOTTOM RIGHT"}[p.Options.Corner&3]), "BACK: CAMERA"}
}

func (u *UI) setPreviewFullscreen(on bool) {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	wasVisible := u.state.monitorVisibleLocked()
	u.state.Preview.Options.Fullscreen = on
	if on && u.state.Preview.Enabled && u.state.Camera.Enabled {
		u.state.setViewLocked(ViewCamera)
	}
	if !on && u.state.View == ViewCamera {
		u.state.setViewLocked(ViewMonitor)
	}
	if wasVisible != u.state.monitorVisibleLocked() {
		u.state.invalidateMonitorLocked()
		u.state.resyncLocked()
	}
}
func (u *UI) selectPreview(row int) {
	if u.state.PreviewCurrent().Options.Fullscreen && row > 2 {
		return
	}
	switch row {
	case 1:
		on := !u.state.PreviewCurrent().Enabled
		u.state.Error(u.enablePreview(on))
	case 2:
		u.setPreviewFullscreen(!u.state.PreviewCurrent().Options.Fullscreen)
	case 3:
		u.state.mu.Lock()
		switch u.state.Preview.Options.WidthPercent {
		case 25:
			u.state.Preview.Options.WidthPercent = 33
		case 33:
			u.state.Preview.Options.WidthPercent = 40
		default:
			u.state.Preview.Options.WidthPercent = 25
		}
		u.state.mu.Unlock()
	case 4:
		u.state.mu.Lock()
		u.state.Preview.Options.Corner = (u.state.Preview.Options.Corner + 1) % 4
		u.state.mu.Unlock()
	}
	u.draw()
}
func (u *UI) clearPreview() {
	u.presentationMu.Lock()
	defer u.presentationMu.Unlock()
	if u.screen != nil {
		p := u.state.PreviewForDisplay()
		if p.Enabled {
			u.state.Error(u.screen.PreviewPlaceholder(p.Options))
		} else {
			u.state.Error(u.screen.ClearPreview())
		}
	}
	u.state.mu.Lock()
	u.state.Preview.LastFrame = time.Time{}
	u.state.mu.Unlock()
}
func (u *UI) previewWorker(ctx context.Context) {
	var source camera.Source
	var sourceReady <-chan struct{}
	var active camera.Settings
	var serial uint64
	var retryAt, lastProgress time.Time
	var windowVisible bool
	var windowOptions fb.PreviewOptions
	latched := false
	budget := safety.RetryBudget{Maximum: 3, Window: time.Minute}
	closeSource := func() error {
		if source == nil {
			return nil
		}
		var gpuErr error
		if u.screen != nil {
			gpuErr = u.screen.ClosePreviewGPU()
		}
		e := errors.Join(gpuErr, source.Close())
		source = nil
		sourceReady = nil
		u.clearPreview()
		return e
	}
	defer func() { u.state.Error(closeSource()); u.clearPreview() }()
	fail := func(e error) {
		e = errors.Join(e, closeSource())
		u.state.Fault("preview", e)
		delay, ok := budget.Next(time.Now())
		retryAt = time.Now().Add(delay)
		if errors.Is(e, camera.ErrOwnership) || errors.Is(e, media.ErrQuarantined) {
			ok = false
		}
		latched = !ok
		status := "PREVIEW RETRY PENDING"
		if latched {
			status = "PREVIEW STOPPED / USE RETRY"
		}
		u.state.previewStatus(status, e)
	}
	// The timer checks controls while capture is off. Active native frames
	// wake through the subscriber mailbox instead of adding a 10 ms delay.
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-sourceReady:
		}
		now := time.Now()
		p := u.state.PreviewForDisplay()
		if p.Enabled && (!windowVisible || windowOptions != p.Options) || !p.Enabled && windowVisible {
			if windowVisible && p.Options.Fullscreen != windowOptions.Fullscreen && u.screen != nil {
				if e := u.screen.ClosePreviewGPU(); e != nil {
					fail(e)
					continue
				}
			}
			u.clearPreview()
			windowVisible = p.Enabled
			windowOptions = p.Options
		}
		cameraState := u.state.CameraCurrent()
		cam := cameraState.Settings
		u.state.mu.Lock()
		paused := u.state.ThermalPaused
		u.state.mu.Unlock()
		if serial != p.Retry {
			serial = p.Retry
			latched = false
			retryAt = time.Time{}
			budget = safety.RetryBudget{Maximum: 3, Window: time.Minute}
			if e := closeSource(); e != nil {
				fail(e)
			}
		}
		if source != nil && (!p.Enabled || !cameraState.Enabled || paused || camera.Key(active) != camera.Key(cam)) {
			if e := closeSource(); e != nil {
				fail(e)
			}
		}
		if !p.Enabled || !cameraState.Enabled || paused {
			status := "DISABLED"
			if p.Enabled && !cameraState.Enabled {
				status = "CAMERA OFF / BLACK PREVIEW"
			}
			if paused && p.Enabled {
				status = "THERMAL PAUSE / CAMERA RELEASED"
			}
			u.state.previewStatus(status, nil)
			continue
		}
		if latched || now.Before(retryAt) {
			continue
		}
		if source == nil {
			provider, ok := u.cameraProvider.(interface {
				OpenPreview(camera.Settings) (camera.Source, error)
			})
			if !ok {
				fail(fmt.Errorf("shared preview provider unavailable"))
				continue
			}
			var e error
			log.Printf("S7 preview source opening sensor=%s mode=%s", cam.Sensor, cam.Mode)
			source, e = provider.OpenPreview(cam)
			log.Printf("S7 preview source open returned error=%v", e)
			if e != nil {
				if errors.Is(e, camera.ErrCaptureBusy) {
					retryAt = now.Add(100 * time.Millisecond)
					u.state.previewStatus("WAITING FOR CAMERA MODE RELEASE", nil)
					continue
				}
				fail(e)
				continue
			}
			if source == nil {
				fail(fmt.Errorf("preview provider returned nil source"))
				continue
			}
			if notifier, ok := source.(interface{ Ready() <-chan struct{} }); ok {
				sourceReady = notifier.Ready()
			}
			e = cam.Image.Validate()
			if e != nil {
				fail(e)
				continue
			}
			active = cam
			lastProgress = now
			u.state.previewStatus("WAITING FOR FIRST SENSOR FRAME", nil)
		}
		n, e := source.Drain(func(im media.Image) error {
			if e := ctx.Err(); e != nil {
				return e
			}
			current := u.state.PreviewForDisplay()
			currentCamera := u.state.CameraCurrent()
			currentCam := currentCamera.Settings
			u.state.mu.Lock()
			paused := u.state.ThermalPaused
			panelRotation := int(u.state.ActiveRotation)
			u.state.mu.Unlock()
			// Never paint an old sensor/mode frame after a UI change or thermal pause.
			if !current.Enabled || !currentCamera.Enabled || paused || current.Retry != serial || camera.Key(currentCam) != camera.Key(active) {
				return nil
			}
			if u.screen == nil {
				return fmt.Errorf("preview framebuffer unavailable")
			}
			rotation := camera.PreviewRotation(currentCam.Sensor, panelRotation, int(currentCam.Image.Rotation))
			if e := u.screen.UpdateCameraPreview(im, current.Options, rotation, currentCam.Image.Mirror, currentCam.Image.Zoom()); e != nil {
				return e
			}
			if !u.state.CameraCurrent().Enabled {
				u.clearPreview()
				return nil
			}
			u.state.mu.Lock()
			u.state.Preview.Copied++
			if im.Lease != nil {
				u.state.Preview.Sensor = im.Lease.Sensor
				u.state.Preview.Stages = im.Lease.CameraStages
			}
			u.state.Preview.LastFrame = time.Now()
			u.state.mu.Unlock()
			return nil
		})
		if e != nil {
			if ctx.Err() != nil {
				return
			}
			fail(e)
			continue
		}
		if n > 0 {
			lastProgress = now
			u.state.previewStatus("LOCAL PREVIEW / NO USB ENCODER REQUIRED", nil)
		}
		if now.Sub(lastProgress) > 3*time.Second {
			fail(fmt.Errorf("local camera preview produced no new image for 3 seconds"))
		}
	}
}

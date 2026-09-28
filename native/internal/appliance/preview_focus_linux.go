//go:build linux && (amd64 || arm64)

package appliance

import (
	"perimode/native/internal/camera"
	"perimode/native/internal/camera/fimcshot"
	"time"
)

func (u *UI) focusPreview(x, y uint16) {
	if u.screen == nil {
		return
	}
	px, py, w, h, hit := u.screen.PreviewPoint(x, y)
	if !hit {
		return
	}
	selected := u.state.CameraCurrent()
	p := u.state.PreviewForDisplay()
	if !selected.Enabled || !p.Enabled || p.LastFrame.IsZero() || time.Since(p.LastFrame) > time.Second {
		return
	}
	u.cameraHUDFocusX, u.cameraHUDFocusY = x, y
	u.cameraHUDFocusAt = time.Now()
	u.draw()
	u.state.mu.Lock()
	panel := int(u.state.ActiveRotation)
	u.state.mu.Unlock()
	u.async(func() error {
		u.cameraControlsMu.Lock()
		defer u.cameraControlsMu.Unlock()
		current := u.state.CameraCurrent()
		if !current.Enabled || current.Epoch != selected.Epoch {
			return nil
		}
		service, ok := u.cameraProvider.(cameraControlService)
		if !ok {
			return camera.ErrControlsUnsupported
		}
		view, err := service.ControlState(selected.Settings)
		if err != nil {
			return err
		}
		if view.Descriptor.Limits.FocusModes&(1<<fimcshot.FocusSingle) == 0 {
			return camera.ErrControlsUnsupported
		}
		region, hit := camera.PreviewFocusRegion(selected.Settings, panel, px, py, w, h, view.Descriptor.Limits.Crop)
		if !hit {
			return nil
		}
		controls := view.Effective
		controls.Focus, controls.FocusDioptres, controls.Trigger = fimcshot.FocusSingle, -1, fimcshot.FocusIdle
		controls.AFRegion = region
		if err = service.SetControls(selected.Settings, camera.ControlSelection{Custom: true, Values: controls}); err != nil {
			return err
		}
		if err = service.TriggerFocus(selected.Settings, fimcshot.FocusStart); err != nil {
			return err
		}
		return u.refreshCamera3A()
	})
}

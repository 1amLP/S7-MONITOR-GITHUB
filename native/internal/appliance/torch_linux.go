//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"perimode/native/internal/camera"
	"perimode/native/internal/torch"
)

// TorchRuntime is transient: no saved ON bit, no auto-on after power-on,
// thermal recovery, USB reconnection, or a driver error. Accepted is a successful
// kernel command, not optical confirmation of a real illuminated LED.
type TorchRuntime struct {
	Wanted    bool   `json:"wanted"`
	Accepted  bool   `json:"driver_on_command_accepted"`
	Unknown   bool   `json:"physical_state_unknown"`
	Status    string `json:"status"`
	LastError string `json:"last_error"`
}

func (s *State) TorchCurrent() TorchRuntime { s.mu.Lock(); defer s.mu.Unlock(); return s.Torch }
func (s *State) RequestTorch(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on && (s.ThermalPaused || s.Camera.Settings.Sensor != camera.Rear) {
		return fmt.Errorf("rear torch requires rear source and a running thermal guard")
	}
	if on && s.Torch.Unknown {
		return fmt.Errorf("torch state unknown after driver failure; cannot enable")
	}
	s.Torch.Wanted = on
	if on {
		s.Torch.Status = "REQUESTED / WAITING FOR DRIVER"
	} else {
		s.Torch.Status = "OFF REQUESTED"
	}
	return nil
}
func (u *UI) torchLines() []string {
	t := u.state.TorchCurrent()
	return []string{"CAMERA / REAR TORCH", fmt.Sprintf("TORCH: %t / TAP TO CHANGE", t.Wanted), "REAR CAMERA ONLY / NORMAL TORCH MODE", "OFF AFTER THERMAL PAUSE OR USB DISCONNECT", "ON IS NEVER SAVED OR AUTOMATICALLY RETRIED", "STATUS: " + t.Status, "ERROR: " + t.LastError, "DRIVER COMMAND IS NOT OPTICAL CONFIRMATION", "BACK: CAMERA"}
}
func (u *UI) selectTorch(row int) {
	if row != 1 {
		return
	}
	u.state.Error(u.state.RequestTorch(!u.state.TorchCurrent().Wanted))
	u.draw()
}

type torchSession struct {
	u         *UI
	driver    torch.Driver
	requested bool
}

func (w *torchSession) publish(t TorchRuntime) {
	w.u.state.mu.Lock()
	defer w.u.state.mu.Unlock() // Never overwrite a newer user request.
	w.u.state.Torch.Accepted = t.Accepted
	w.u.state.Torch.Unknown = t.Unknown
	w.u.state.Torch.Status = t.Status
	w.u.state.Torch.LastError = t.LastError
}
func (w *torchSession) off() error {
	if w.driver == nil {
		return nil
	}
	e := w.driver.Close()
	w.driver = nil
	t := TorchRuntime{Status: "OFF COMMAND ACCEPTED"}
	if e != nil {
		t.Unknown = true
		t.Status = "OFF FAILED / REQUEST POWER OFF"
		t.LastError = e.Error()
	}
	w.publish(t)
	return e
}
func (w *torchSession) step() error {
	s := w.u.state
	s.mu.Lock()
	if s.ThermalPaused || s.Camera.Settings.Sensor != camera.Rear {
		s.Torch.Wanted = false
	}
	wanted := s.Torch.Wanted
	s.mu.Unlock()
	if !wanted {
		w.requested = false
		return w.off()
	}
	if w.requested {
		return nil
	} // One attempt for one explicit enable request.
	w.requested = true
	factory := w.u.torchFactory
	if factory == nil {
		factory = torch.OpenRear
	}
	d, e := factory()
	if d != nil {
		w.driver = d
	}
	if e == nil && d == nil {
		e = fmt.Errorf("torch driver factory returned nil")
	}
	if e == nil {
		w.driver = d
		e = d.Set(true)
	}
	if e != nil {
		ce := w.off()
		s.mu.Lock()
		s.Torch.Wanted = false
		s.Torch.LastError = e.Error()
		s.Torch.Status = "ENABLE FAILED / OFF"
		if ce != nil {
			s.Torch.Unknown = true
			s.Torch.Status = "OFF FAILED / REQUEST POWER OFF"
		}
		s.mu.Unlock()
		s.Fault("torch", e)
		return ce // Failed enable with known off is a local error.
	}
	w.publish(TorchRuntime{Accepted: true, Status: "NORMAL TORCH ON COMMAND ACCEPTED"})
	// A user, source, or thermal change during a blocking sysfs write must win.
	s.mu.Lock()
	safe := s.Torch.Wanted && !s.ThermalPaused && s.Camera.Settings.Sensor == camera.Rear
	s.mu.Unlock()
	if !safe {
		return w.off()
	}
	return nil
}
func (u *UI) torchWorker(ctx context.Context, stop func(error)) {
	w := torchSession{u: u}
	defer func() {
		if e := w.off(); e != nil {
			stop(fmt.Errorf("torch shutdown: %w", e))
		}
	}()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if e := w.step(); e != nil {
			stop(errors.Join(fmt.Errorf("cannot confirm normal torch OFF"), e))
			return
		}
	}
}

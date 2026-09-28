//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"

	"perimode/native/internal/camera"
)

type CameraTrialRuntime struct {
	Active          bool                      `json:"active"`
	CancelRequested bool                      `json:"cancel_requested"`
	Poisoned        bool                      `json:"ownership_unresolved"`
	FPS             uint32                    `json:"requested_fps"`
	Status          string                    `json:"status"`
	Result          camera.CaptureTrialReport `json:"result"`
}

type cameraTrialRunner func(context.Context, uint32, func() error) (camera.CaptureTrialReport, error)

func (s *State) CameraTrialCurrent() CameraTrialRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.CameraTrial
}
func (u *UI) trialLines() []string {
	v := u.state.CameraTrialCurrent()
	status := v.Status
	if status == "" {
		status = "NOT RUN / EXPLICIT CONFIRMATION REQUIRED"
	}
	return []string{"CAMERA / HIGH FPS / LOCAL TRIAL", "TEST REAR 720P / 120 FPS", "TEST REAR 720P / 240 FPS", "CANCEL CURRENT TRIAL", "STATUS: " + status,
		fmt.Sprintf("CAPTURED: %d / LOCAL %.2f FPS / PTS %.2f FPS", v.Result.Frames, v.Result.ArrivalFPS, v.Result.TimestampFPS),
		"ERROR: " + v.Result.Error, "2 S WARMUP + 10 S MEASUREMENT / NO VIDEO SAVED", "NV12 CAPTURE ONLY / NO MFC / NO USB", "USB CADENCE NOT VERIFIED BY THIS TEST", "BACK: CAMERA"}
}
func (u *UI) requestCameraTrial(fps uint32) error {
	if u.usbTrialFPS != 0 {
		return fmt.Errorf("local NV12 trial requires the normal BOOT; USB trial build is exclusive")
	}
	if (fps != 120 && fps != 240) || u.cameraTrialRequests == nil || u.background == nil {
		return fmt.Errorf("native camera trial worker unavailable or invalid FPS")
	}
	if e := u.background.Err(); e != nil {
		return e
	}
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	v := &u.state.CameraTrial
	if v.Active {
		return camera.ErrCaptureBusy
	}
	if v.Poisoned || u.state.cameraOwnershipFault {
		return camera.ErrOwnership
	}
	if u.state.Camera.Enabled || u.state.Preview.Enabled {
		return fmt.Errorf("disable webcam and PiP before a local camera trial")
	}
	if u.state.ThermalPaused {
		return fmt.Errorf("camera trial denied by thermal guard")
	}
	previous := *v
	*v = CameraTrialRuntime{Active: true, FPS: fps, Status: "QUEUED / EXCLUSIVE SENSOR TRIAL"}
	select {
	case u.cameraTrialRequests <- fps:
		return nil
	default:
		*v = previous
		return fmt.Errorf("camera trial already queued")
	}
}
func (u *UI) cancelCameraTrial() {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	if u.state.CameraTrial.Active {
		u.state.CameraTrial.CancelRequested = true
		u.state.CameraTrial.Status = "STOP REQUESTED / WAITING FOR OWNERSHIP RETURN"
	}
}
func (u *UI) trialGuard() error {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	v := u.state.CameraTrial
	if !v.Active || v.CancelRequested {
		return context.Canceled
	}
	if v.Poisoned || u.state.cameraOwnershipFault {
		return camera.ErrOwnership
	}
	if u.state.ThermalPaused {
		return fmt.Errorf("camera trial stopped by thermal guard")
	}
	if u.state.Camera.Enabled || u.state.Preview.Enabled {
		return camera.ErrCaptureBusy
	}
	return nil
}

// Fixed lifecycle worker, not a fire-and-forget task. A stuck device operation
// stays registered and its source stays owned. No retries, unbinds or reboots
// are performed here; ordinary PID1 shutdown retains its existing deadline.
func (u *UI) cameraTrialWorker(ctx context.Context) {
	defer func() {
		u.state.mu.Lock()
		defer u.state.mu.Unlock()
		if u.state.CameraTrial.Active {
			u.state.CameraTrial.Active = false
			u.state.CameraTrial.Status = "CANCELLED / WORKER STOPPED"
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case fps := <-u.cameraTrialRequests:
			u.state.mu.Lock()
			u.state.CameraTrial.Status = "RUNNING / WARMUP THEN MEASUREMENT"
			u.state.mu.Unlock()
			run := u.cameraTrialRun
			if run == nil {
				run = (camera.HerolteProvider{}).RunHighFPSTrial
			}
			result, err := run(ctx, fps, u.trialGuard)
			if err != nil {
				result.Error = err.Error()
				result.CadenceObserved = false
			}
			u.state.mu.Lock()
			v := &u.state.CameraTrial
			v.Result = result
			v.Active = false
			v.Poisoned = errors.Is(err, camera.ErrOwnership)
			switch {
			case v.Poisoned:
				v.Status = "OWNERSHIP UNRESOLVED / NO RETRY BEFORE REBOOT"
			case errors.Is(err, context.Canceled):
				v.Status = "CANCELLED / NOT A PASS"
			case err != nil:
				v.Status = "FAILED / CAPTURE NOT VERIFIED"
			case result.CadenceObserved:
				v.Status = "LOCAL CADENCE OBSERVED / USB NOT TESTED"
			default:
				v.Status = "COMPLETED / TARGET CADENCE NOT OBSERVED"
			}
			u.state.mu.Unlock()
			if err != nil && (!errors.Is(err, context.Canceled) || errors.Is(err, camera.ErrOwnership)) {
				u.state.Fault("camera-trial", err)
			}
		}
	}
}

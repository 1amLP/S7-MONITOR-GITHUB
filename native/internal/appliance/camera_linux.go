//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"perimode/native/internal/camera"
	"perimode/native/internal/camerausb"
	"perimode/native/internal/media"
	"perimode/native/internal/mediacodec"
	"perimode/native/internal/mediaruntime"
	"perimode/native/internal/uvcout"
	"perimode/native/pkg/cameractl"
	"perimode/native/pkg/usb/configfs"
	"perimode/native/pkg/usb/uvcmode"
	"log"
	"os"
	"time"
)

type CameraRuntime struct {
	LastActivity time.Time       `json:"last_activity"`
	Settings     camera.Settings `json:"settings"`
	Enabled      bool            `json:"enabled"`
	Epoch        uint64          `json:"epoch"`
	Status       string          `json:"status"`
	LastError    string          `json:"last_error"`
	FirstError   string          `json:"first_error,omitempty"`
	Counters     camera.Counters `json:"counters"`
	USB          uvcout.Stats    `json:"usb_queue"`
}

func (s *State) CameraCurrent() CameraRuntime { s.mu.Lock(); defer s.mu.Unlock(); return s.Camera }
func (s *State) CameraPreference() [16]byte   { return s.CameraCurrent().Settings.Preference() }
func (s *State) ConfigureCamera(v camera.Settings) error {
	if e := v.Validate(); e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCameraChangeLocked(v); err != nil {
		return err
	}
	s.configureCameraLocked(v)
	return nil
}

func (s *State) validateCameraChangeLocked(v camera.Settings) error {
	if s.CameraTrial.Active {
		return camera.ErrCaptureBusy
	}
	if s.cameraSelection.Owner(s.cameraEnvironmentLocked(), time.Now()) != 0 {
		if s.Camera.Settings.Mode != v.Mode && !s.cameraDynamicFormat {
			return fmt.Errorf("Windows owns the camera format; keep its resolution and FPS while streaming")
		}
		if !cameractl.Allowed(byte(v.Sensor), cameractl.Mode(v.Mode), s.cameraEnvironmentLocked().Masks) {
			return fmt.Errorf("selected sensor does not support the open Windows format")
		}
	}
	return nil
}
func (s *State) configureCameraLocked(v camera.Settings) {
	if v.Sensor != s.Camera.Settings.Sensor {
		s.CameraImages[s.Camera.Settings.Sensor] = s.Camera.Settings.Image
		v.Image = s.CameraImages[v.Sensor]
	}
	s.CameraImages[v.Sensor] = v.Image
	if s.Camera.Settings != v {
		wasVisible := s.monitorVisibleLocked()
		previous := s.Camera.Settings
		previous.Image.ZoomPercent = v.Image.ZoomPercent
		s.Camera.Settings = v
		if wasVisible != s.monitorVisibleLocked() {
			s.invalidateMonitorLocked()
			s.resyncLocked()
		}
		if previous == v {
			return // Live scaler crop; preserve sensor, MFC, PTS and USB ownership.
		}
		s.Camera.Epoch++
		s.Camera.LastActivity = time.Time{}
		s.Camera.Counters = camera.Counters{}
		s.Camera.USB = uvcout.Stats{}
		s.Camera.Status = "MODE CHANGED / REOPEN HOST PREVIEW"
	}
}
func (s *State) cameraStatus(status string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Camera.Status = status
	if err != nil {
		s.Camera.LastError = err.Error()
		if s.Camera.FirstError == "" {
			s.Camera.FirstError = status + ": " + err.Error()
			log.Printf("S7 first camera failure: %s", s.Camera.FirstError)
		}
	} else {
		s.Camera.LastError = ""
	}
}
func (s *State) cameraEnabled(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if on && s.CameraTrial.Poisoned {
		return camera.ErrOwnership
	}
	if on && s.CameraTrial.Active {
		return camera.ErrCaptureBusy
	}
	if s.Camera.Enabled != on {
		s.Preview.Retry++
	}
	s.Camera.Enabled = on
	if !on && s.View == ViewCamera {
		s.setViewLocked(ViewMonitor)
	}
	s.Camera.Epoch++
	s.Camera.LastActivity = time.Time{}
	if !on {
		s.cameraSelection.Reset()
		s.wakeCameraLEDLocked()
		s.Camera.Status = "DISABLED"
	} else {
		s.Camera.Status = "STARTING CAMERA"
	}
	return nil
}
func (s *State) cameraCounters(c camera.Counters, u uvcout.Stats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.USBQueued > s.Camera.Counters.USBQueued {
		s.Camera.LastActivity = time.Now()
	}
	s.Camera.Counters = c
	s.Camera.USB = u
}

type cameraTask struct {
	cancel       context.CancelFunc
	done         chan struct{}
	err          error
	retry        chan chan error
	owner        *cameraBindingOwner // Retained after an unsafe stop; read after done.
	openErr      error               // A partial producer open remains owned until unbind.
	retainOutput bool                // Transport owns the shared FunctionFS device across pauses.
}

// Start USB control handling with the composite, not after Enabled. This does
// not open the sensor/encoder; it prevents losing the host's initial UVC setup.
func (t *Transport) prepareCameraControl(provider camera.Provider) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.bound && !t.closed {
		t.cameraProvider = provider
	}
}

// Called only with Transport.mu held. The worker never acquires Transport.mu.
func (t *Transport) startCameraLocked() {
	if (t.cameraFunction == nil && t.cameraPrivate == nil) || t.cameraProvider == nil || t.cameraTask != nil {
		return
	}
	ctx, cancel := context.WithCancel(t.ctx)
	task := &cameraTask{cancel: cancel, done: make(chan struct{}), retry: make(chan chan error), retainOutput: t.cameraPrivate != nil}
	t.cameraTask = task
	provider := t.cameraProvider
	vc := t.cameraVC
	table := t.cameraTable
	private := t.cameraPrivate
	go func() {
		defer close(task.done)
		if private != nil {
			selected, e := mediacodec.LoadSelection()
			if e != nil {
				task.err = e
				return
			}
			task.err = runCameraBinding(ctx, t.state, provider, private,
				func(c context.Context, v media.EncodeSettings) (camera.Encoder, error) {
					return openNativeCameraEncoder(c, selected, v)
				},
				func() (bool, error) { return selected.Camera == "mfc", nil }, task)
		} else {
			task.err = runCameraOutput(ctx, t.state, provider, vc, vc+1, table, task)
		}
		if task.err != nil && (!errors.Is(task.err, context.Canceled) || errors.Is(task.err, camera.ErrOwnership) || errors.Is(task.err, media.ErrQuarantined)) {
			t.state.Fault("camera", task.err)
			t.state.cameraStatus("ERROR / REOPEN OR RETRY", task.err)
		}
	}()
}

func (t *Transport) provisionCameraLocked(table uvcmode.Table) error {
	if t.cameraFunction != nil || t.cameraPrivate != nil {
		return nil
	}
	if t.bound {
		if t.cameraProvisionErr != nil {
			return fmt.Errorf("camera endpoints unavailable at USB bind: %w", t.cameraProvisionErr)
		}
		return fmt.Errorf("camera endpoints were not provisioned before USB bind")
	}
	if t.state.USBTrialFPS == 0 {
		d, err := camerausb.Open(t.ctx, t.g, t.c, t.state.cameraWireAuthorize, t.state.cameraWireMode)
		if err != nil {
			return err
		}
		t.cameraPrivate = d
		t.state.mu.Lock()
		t.state.cameraDynamicFormat = true
		t.state.mu.Unlock()
		t.cameraVC = t.nextInterface
		t.nextInterface++
		t.cameraTable = table
		t.cameraProvisionErr = nil
		return nil
	}
	u, err := configfs.CreateAvailableH264UVC(gadget, table)
	if err != nil {
		return err
	}
	if err = t.c.AddUVCFunction(u); err != nil {
		return errors.Join(err, u.Close())
	}
	t.cameraVC = t.nextInterface
	t.nextInterface += 2
	t.cameraFunction = u
	t.cameraTable = table
	t.cameraProvisionErr = nil
	return nil
}
func (t *Transport) stopCameraLocked() error {
	task := t.cameraTask
	if task == nil {
		return nil
	}
	task.cancel()
	select {
	case <-task.done:
		if errors.Is(task.err, camera.ErrOwnership) || errors.Is(task.err, media.ErrQuarantined) {
			return task.err
		}
		t.cameraTask = nil
		return nil
	case <-time.After(3 * time.Second):
		return fmt.Errorf("camera worker has not returned DMA ownership; USB unbind refused")
	}
}

// Configuration/enable is admitted only when a real native sensor provider is
// available. Rejected sensor/profile requests return BEFORE any USB descriptor
// or device change. Successful admission is not a frame/hardware acceptance test.
// This path never enables synthetic video.
func (t *Transport) EnsureCamera(provider camera.Provider, settings camera.Settings) error {
	if provider == nil {
		return camera.ErrSensorGraph
	}
	if e := provider.Available(settings); e != nil {
		return e
	}
	catalog := camera.InspectModes(provider, settings)
	var table uvcmode.Table
	var e error
	if fps := camera.USBTrialFPS(provider); fps != 0 {
		// Explicit engineering BOOT, not a persisted preference or host command.
		if settings.Sensor != camera.Rear || settings.Mode != (camera.Mode{Width: 1280, Height: 720, FPS: fps}) {
			return fmt.Errorf("USB trial descriptor/source mismatch")
		}
		table, e = uvcmode.NewUSBTrial(fps)
	} else {
		if e = catalog.Check(settings); e != nil {
			return e
		}
		table, e = catalog.Table()
	}
	if e != nil {
		return e
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return os.ErrClosed
	}
	if t.cameraFunction != nil || t.cameraPrivate != nil {
		if !t.cameraTable.Contains(uvcmode.Mode(settings.Mode)) {
			return fmt.Errorf("selected mode is absent from this USB session's immutable descriptors")
		}
		t.cameraProvider = provider
		t.state.cameraPublishedTable(catalog, t.cameraTable)
		if t.bound {
			t.startCameraLocked()
		}
		return nil
	}
	if e = t.provisionCameraLocked(table); e != nil {
		t.cameraProvisionErr = e
		return e
	}
	t.cameraProvider = provider
	t.state.cameraPublishedTable(catalog, table)
	if t.bound {
		t.startCameraLocked()
	}
	return nil
}
func (t *Transport) RetryCamera() error {
	t.mu.Lock()
	if t.closed || !t.bound || (t.cameraFunction == nil && t.cameraPrivate == nil) {
		t.mu.Unlock()
		return camera.ErrSensorGraph
	}
	task := t.cameraTask
	if task == nil {
		t.mu.Unlock()
		return fmt.Errorf("camera binding worker is unavailable")
	}
	t.mu.Unlock()
	// No cancel/reopen here: closing the producer disconnects all USB on e418.
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	reply := make(chan error, 1)
	select {
	case <-task.done:
		return errors.Join(fmt.Errorf("camera binding worker stopped"), task.err)
	case task.retry <- reply:
	case <-timer.C:
		return fmt.Errorf("camera retry not accepted; binding owner retained")
	}
	select {
	case err := <-reply:
		return err
	case <-task.done:
		return errors.Join(fmt.Errorf("camera binding worker stopped"), task.err)
	case <-timer.C:
		return fmt.Errorf("camera retry cleanup still pending; binding owner retained")
	}
}

// cameraOutputDevice is the UVC device boundary. The same session loop runs on
// S7 and in the combined fault-injection harness; only device I/O is substituted.
type cameraOutputDevice interface {
	camera.Sink
	Events(func(uint32) error) error
	Mode() (camera.Mode, error)
	Start(camera.Mode) error
	Stop() error
	Streaming(camera.Mode) bool
	Stats() uvcout.Stats
	Close() error
}
type nativeCameraOutput struct{ *uvcout.Device }

func (d nativeCameraOutput) Mode() (camera.Mode, error) { return d.Controller.Mode() }

type cameraEncoderOpen func(context.Context, media.EncodeSettings) (camera.Encoder, error)
type cameraStartGate func() (bool, error)

func openNativeCameraEncoder(ctx context.Context, selected mediacodec.Selection, settings media.EncodeSettings) (camera.Encoder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if selected.Camera == "mediacodec" {
		encoder, err := mediacodec.OpenEncoder(ctx, settings)
		if encoder == nil {
			return nil, err
		}
		return encoder, err
	}
	if selected.Camera != "mfc" {
		return nil, fmt.Errorf("unselected camera codec; no implicit backend")
	}
	path, err := media.EncoderPath(media.Discover())
	if err != nil {
		return nil, err
	}
	encoder, err := media.OpenEncoder(path, settings)
	if encoder == nil {
		return nil, err
	}
	return encoder, err
}

// Slow source/codec opens cannot reactivate a disabled, superseded or paused
// camera. Their resources still need an explicit Close before retrying.
func cameraStartupCurrent(ctx context.Context, s *State, epoch uint64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Camera.Enabled && s.Camera.Epoch == epoch && !s.ThermalPaused, nil
}

func runCameraOutput(ctx context.Context, s *State, provider camera.Provider, vc, vs uint8, table uvcmode.Table, task *cameraTask) (err error) {
	// Configuration errors must precede the e418 producer open, not close it.
	selected, e := mediacodec.LoadSelection()
	if e != nil {
		return e
	}
	// V4L2 gadget node exists only once configfs has bound the UDC.
	var path string
	end := time.Now().Add(2 * time.Second)
	for ctx.Err() == nil {
		path, err = uvcout.FindDevice()
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) || time.Now().After(end) {
			return fmt.Errorf("find bound UVC producer in /sys/class/video4linux: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	d, e := uvcout.OpenTable(path, vc, vs, table)
	if d == nil {
		return fmt.Errorf("open UVC producer %s: %w", path, e)
	}
	if e != nil {
		task.openErr = fmt.Errorf("initialize UVC producer %s: %w", path, e)
	}
	log.Printf("S7 UVC control owner path=%s vc=%d vs=%d error=%v", path, vc, vs, e)
	return runCameraBinding(ctx, s, provider, nativeCameraOutput{d},
		func(c context.Context, v media.EncodeSettings) (camera.Encoder, error) {
			if fps := camera.USBTrialFPS(provider); fps != 0 {
				if selected.Camera != "mediacodec" || v.FPS != fps {
					return nil, fmt.Errorf("USB high-FPS trial requires the selected MediaCodec backend")
				}
				encoder, err := mediacodec.OpenUSBTrialEncoder(c, v)
				if encoder == nil {
					return nil, err
				}
				return encoder, err
			}
			return openNativeCameraEncoder(c, selected, v)
		}, func() (bool, error) {
			if selected.Camera != "mediacodec" {
				return true, nil
			}
			return mediaruntime.Snapshot().CodecReady()
		}, task)
}

var errCameraRetry = errors.New("explicit camera capture retry")

// One owner per USB binding. Capture errors may release only the source/codec;
// the e418 producer fd must remain open, including while the host is unplugged.
type cameraBindingOwner struct {
	cameraOutputDevice
	capture        cameraCaptureOwner
	retry          <-chan chan error
	reply          chan error
	demand, replay bool
	initialized    bool
	deviceErr      error
	trialCtx       context.Context
	trialCancel    context.CancelFunc
}

type cameraCaptureOwner struct {
	source   camera.Source
	encoder  camera.Encoder
	pipeline *camera.Pipeline
	unsafe   error
}

func (o *cameraCaptureOwner) close() error {
	if o.unsafe != nil {
		return o.unsafe
	}
	var err error
	if o.pipeline != nil {
		err = o.pipeline.Close()
		if err == nil {
			o.pipeline, o.source, o.encoder = nil, nil, nil
		}
	} else {
		if o.encoder != nil {
			e := o.encoder.Close()
			err = errors.Join(err, e)
			if e == nil {
				o.encoder = nil
			}
		}
		if o.source != nil {
			e := o.source.Close()
			err = errors.Join(err, e)
			if e == nil {
				o.source = nil
			}
		}
	}
	if err != nil {
		o.unsafe = errors.Join(camera.ErrOwnership, err)
	}
	return o.unsafe
}

func (o *cameraBindingOwner) Events(emit func(uint32) error) error {
	select {
	case o.reply = <-o.retry:
		return errCameraRetry
	default:
	}
	if !o.initialized {
		// Do not send ioctls to a producer whose identity/subscriptions failed.
		return nil
	}
	if o.replay {
		o.replay = false
		if o.demand {
			if err := emit(uvcout.EventStreamOn); err != nil {
				return err
			}
		}
	}
	var callbackErr error
	err := o.cameraOutputDevice.Events(func(event uint32) error {
		switch event {
		case uvcout.EventStreamOn:
			o.demand = true
		case uvcout.EventStreamOff, uvcout.EventDisconnect:
			o.demand = false
			if o.trialCancel != nil {
				o.trialCancel()
			}
			if err := o.cameraOutputDevice.Stop(); err != nil {
				o.deviceErr = errors.Join(camera.ErrOwnership, err)
				return o.deviceErr
			}
		}
		callbackErr = emit(event)
		return callbackErr
	})
	if err != nil && callbackErr == nil {
		o.deviceErr = fmt.Errorf("UVC producer event dequeue: %w", err)
		return o.deviceErr
	}
	return err
}

func (o *cameraBindingOwner) Start(m camera.Mode) error {
	if o.deviceErr != nil {
		return o.deviceErr
	}
	if o.Streaming(m) {
		return nil
	}
	if err := o.cameraOutputDevice.Start(m); err != nil {
		o.deviceErr = fmt.Errorf("UVC producer start %s: %w", m, err)
		return o.deviceErr
	}
	return nil
}

// A capture attempt borrows output. Only real STREAMOFF/disconnect or binding
// teardown stops it. Retaining an empty output queue avoids another e418
// STREAMON/setup_continue when the sensor is retried without SET_INTERFACE.
func (o *cameraBindingOwner) Stop() error { return o.deviceErr }
func (o *cameraBindingOwner) BeginCapture(m camera.Mode) error {
	if v, ok := o.cameraOutputDevice.(interface{ BeginCapture(camera.Mode) error }); ok {
		return v.BeginCapture(m)
	}
	return nil
}
func (o *cameraBindingOwner) Close() error { return camera.ErrOwnership }

func (o *cameraBindingOwner) retryError() error {
	if err := errors.Join(o.capture.unsafe, o.deviceErr); err != nil {
		return err
	}
	if o.trialCtx != nil {
		return o.trialCtx.Err()
	}
	return nil
}

func (o *cameraBindingOwner) answerRetry(err error) {
	if o.reply != nil {
		o.reply <- err
		o.reply = nil
	}
}

func runCameraBinding(ctx context.Context, s *State, provider camera.Provider, d cameraOutputDevice, openEncoder cameraEncoderOpen, backendReady cameraStartGate, task *cameraTask) (err error) {
	if task == nil || s == nil || provider == nil || d == nil || openEncoder == nil || backendReady == nil {
		return fmt.Errorf("camera binding owner missing")
	}
	o := &cameraBindingOwner{cameraOutputDevice: d, retry: task.retry, initialized: task.openErr == nil, deviceErr: task.openErr}
	task.owner = o
	defer func() {
		if o.trialCancel != nil {
			o.trialCancel()
		}
		cleanup := o.capture.close()
		if e := d.Stop(); e != nil {
			cleanup = errors.Join(cleanup, camera.ErrOwnership, e)
		}
		if cleanup == nil && !task.retainOutput {
			cleanup = d.Close()
		}
		if cleanup != nil {
			cleanup = errors.Join(camera.ErrOwnership, cleanup)
		} else {
			task.owner = nil
		}
		o.answerRetry(errors.Join(ctx.Err(), cleanup))
		s.cameraStreaming(false, cleanup)
		err = errors.Join(err, cleanup)
	}()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for ctx.Err() == nil {
		o.replay = o.demand
		failure := task.openErr
		if failure == nil {
			failure = runCameraCaptureSession(ctx, s, provider, o, openEncoder, backendReady, &o.capture)
		}
		if errors.Is(failure, camera.ErrOwnership) || errors.Is(failure, media.ErrQuarantined) {
			o.capture.unsafe = errors.Join(camera.ErrOwnership, failure)
		}
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), failure)
		}
		if errors.Is(failure, errCameraRetry) && o.retryError() == nil {
			o.answerRetry(nil)
			continue
		}
		o.answerRetry(errors.Join(failure, o.retryError()))
		s.cameraStatus("ERROR / EXPLICIT RETRY REQUIRED", failure)
		failedEpoch := s.CameraCurrent().Epoch
		for ctx.Err() == nil {
			// A new explicit menu choice retries a cleanly stopped capture once.
			// The UVC fd, UDC, monitor and audio remain untouched.
			current := s.CameraCurrent()
			if current.Enabled && current.Epoch != failedEpoch && o.retryError() == nil {
				break
			}
			// Host events and elapsed time alone do not reopen failed capture.
			e := o.Events(func(event uint32) error {
				if event == uvcout.EventStreamOn && o.deviceErr == nil {
					m, err := o.Mode()
					if err != nil {
						return err
					}
					return o.Start(m)
				}
				s.cameraStreaming(o.demand, o.capture.unsafe)
				return nil
			})
			if errors.Is(e, errCameraRetry) {
				if blocked := o.retryError(); blocked != nil {
					o.answerRetry(blocked)
				} else {
					o.answerRetry(nil)
					break
				}
			} else if e != nil {
				controlError := fmt.Errorf("UVC event/control: %w", e)
				if failure != nil {
					controlError = errors.Join(fmt.Errorf("camera capture: %w", failure), controlError)
				}
				s.cameraStatus("ERROR / UVC CONTROL", controlError)
			}
			select {
			case <-ctx.Done():
				return errors.Join(ctx.Err(), failure)
			case <-tick.C:
			}
		}
	}
	return ctx.Err()
}

// Device enumeration/configfs remains outside this loop. Teardown here has no
// UDC, HID, monitor, audio or system-power operations.
func runCameraSession(ctx context.Context, s *State, provider camera.Provider, d cameraOutputDevice, openEncoder cameraEncoderOpen, backendReady cameraStartGate) (err error) {
	// Single-attempt harness; production always uses the USB-binding owner.
	return runCameraCaptureSession(ctx, s, provider, d, openEncoder, backendReady, &cameraCaptureOwner{})
}

func runCameraCaptureSession(ctx context.Context, s *State, provider camera.Provider, d cameraOutputDevice, openEncoder cameraEncoderOpen, backendReady cameraStartGate, resources *cameraCaptureOwner) (err error) {
	if s == nil || provider == nil || d == nil || openEncoder == nil || backendReady == nil {
		return fmt.Errorf("camera session owner missing")
	}
	trialFPS := camera.USBTrialFPS(provider)
	var trialCancel context.CancelFunc
	binding, _ := d.(*cameraBindingOwner)
	if binding != nil && binding.trialCtx != nil {
		ctx, trialCancel = binding.trialCtx, binding.trialCancel
	}
	var p *camera.Pipeline
	var sourceReady <-chan struct{}
	var epoch uint64
	var activeMode camera.Mode
	streamOnPending := false
	hostDemand := false
	var captureBusy cameraCaptureWait
	waitHandoff := func(e error, epoch uint64) bool {
		if !errors.Is(e, camera.ErrCaptureBusy) || !captureBusy.pending(epoch, time.Now()) {
			return false
		}
		s.cameraStatus("WAITING FOR CAMERA MODE RELEASE", nil)
		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Millisecond):
		}
		return true
	}
	stop := func() error {
		sourceReady = nil
		a := resources.close()
		if a == nil {
			p = nil
		}
		stopped := errors.Join(a, d.Stop())
		s.cameraStreaming(hostDemand, stopped)
		return stopped
	}
	handleEvent := func(event uint32) error {
		if event == uvcout.EventStreamOn || event == uvcout.EventStreamOff || event == uvcout.EventDisconnect {
			log.Printf("S7 UVC event=%#x", event)
		}
		if event == uvcout.EventStreamOn {
			hostDemand = true
			s.cameraStreaming(true, nil)
			streamOnPending = true
		}
		if event == uvcout.EventStreamOff || event == uvcout.EventDisconnect {
			hostDemand = false
			streamOnPending = false
			e := stop()
			if trialCancel != nil {
				trialCancel()
			}
			return e
		}
		return nil
	}
	defer func() {
		hostDemand = false
		cleanup := stop()
		s.cameraStreaming(false, cleanup)
		err = errors.Join(err, cleanup)
	}()
	interval := 20 * time.Millisecond
	tick := time.NewTicker(interval)
	defer tick.Stop()
	s.cameraStatus("WAITING FOR WINDOWS CAMERA", nil)
	for ctx.Err() == nil {
		if err = d.Events(handleEvent); err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		current := s.CameraCurrent()
		if !hostDemand || !current.Enabled {
			captureBusy = cameraCaptureWait{}
		}
		s.mu.Lock()
		paused := s.ThermalPaused
		s.mu.Unlock()
		// A SET_INTERFACE must not wait forever for unavailable capture. Complete
		// it with an empty compressed queue before stopping/rejecting the camera.
		// This is camera-local; no UDC or monitor reset is performed.
		negotiated, modeErr := d.Mode()
		if streamOnPending {
			if p != nil {
				if e := stop(); e != nil {
					return e
				}
			}
			streamOnPending = false
			if modeErr != nil {
				return modeErr
			}
			if e := d.Start(negotiated); e != nil {
				return e
			}
		}
		if p != nil && (!hostDemand || !current.Enabled || paused || epoch != current.Epoch || modeErr != nil || activeMode != negotiated) {
			if err = stop(); err != nil {
				return err
			}
		}
		// Do not open the sensor repeatedly while native media services unpack or
		// register. Keep servicing UVC events; no fake frames and no USB rebind.
		if hostDemand && current.Enabled && !paused {
			ready, e := backendReady()
			if e != nil {
				return e
			}
			if !ready {
				if p != nil {
					return fmt.Errorf("camera codec runtime lost readiness during capture")
				}
				s.cameraStatus("WAITING FOR NATIVE MEDIACODEC SERVICES", nil)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-tick.C:
				}
				continue
			}
		}
		if hostDemand && current.Enabled && !paused && p == nil {
			m, e := d.Mode()
			if e != nil {
				return e
			}
			if m != current.Settings.Mode {
				return fmt.Errorf("camera mode mismatch: Windows committed %s, selected %s; reopen preview", m, current.Settings.Mode)
			} else {
				if e = provider.Available(current.Settings); e != nil {
					if waitHandoff(e, current.Epoch) {
						continue
					}
					return e
				}
				// Complete pending alternate-setting status before potentially slow sensor
				// initialization. Empty compressed queues do not transmit fake pictures.
				if !d.Streaming(m) {
					if e = d.Start(m); e != nil {
						return e
					}
				}
				if trialFPS != 0 && trialCancel == nil {
					// Starts on the FIRST real host capture request, not at boot or
					// enumeration. One deadline across source, codec and all retries.
					trialCtx, cancel := context.WithTimeout(ctx, mediacodec.USBTrialLimit)
					if binding != nil {
						binding.trialCtx, binding.trialCancel = trialCtx, cancel
					} else {
						defer cancel()
					}
					ctx, trialCancel = trialCtx, cancel
				}
				source, e := provider.Open(current.Settings)
				resources.source = source
				if e != nil {
					if source == nil && waitHandoff(e, current.Epoch) {
						// Preview retires its previous sensor asynchronously. Keep
						// servicing UVC without reopening MFC or rebinding USB.
						continue
					}
					return fmt.Errorf("open native %s camera %s: %w", current.Settings.Sensor, current.Settings.Mode, e)
				}
				captureBusy = cameraCaptureWait{}
				if source == nil {
					return fmt.Errorf("camera provider returned no source")
				}
				raw := source
				if ready, ok := raw.(interface{ Ready() <-chan struct{} }); ok {
					sourceReady = ready.Ready()
				}
				options := current.Settings.Image
				s.mu.Lock()
				panelRotation := int(s.ActiveRotation)
				s.mu.Unlock()
				options.Rotation = uint16(camera.PreviewRotation(current.Settings.Sensor, panelRotation, int(options.Rotation)))
				source, e = camera.WrapLiveHardwareTransform(raw, current.Settings.Mode, options, func() int {
					return s.CameraCurrent().Settings.Image.Zoom()
				})
				if e != nil {
					return e
				}
				resources.source = source
				valid, e := cameraStartupCurrent(ctx, s, current.Epoch)
				if e != nil || !valid {
					if cleanup := errors.Join(e, resources.close()); cleanup != nil {
						return cleanup
					}
					continue
				}
				enc, e := openEncoder(ctx, current.Settings.Encoder())
				resources.encoder = enc
				if e != nil {
					return e
				}
				if enc == nil {
					return fmt.Errorf("camera encoder opener returned no encoder")
				}
				// StreamOff and mode changes may have arrived during a vendor open.
				// Consume them before the first NV12 submission, not one frame later.
				pendingErr := d.Events(handleEvent)
				valid, e = cameraStartupCurrent(ctx, s, current.Epoch)
				latest, modeErr := d.Mode()
				ready, readyErr := backendReady()
				if pendingErr != nil || e != nil || !valid || !hostDemand || streamOnPending || modeErr != nil || latest != m || !ready || readyErr != nil {
					cleanup := errors.Join(pendingErr, e, readyErr, resources.close())
					if cleanup != nil {
						return cleanup
					}
					continue
				}
				if boundary, ok := d.(interface{ BeginCapture(camera.Mode) error }); ok {
					if e = boundary.BeginCapture(m); e != nil {
						return e
					}
				}
				p, e = camera.NewModePipeline(current.Settings, source, enc, d, time.Now())
				if e != nil {
					return e
				}
				resources.pipeline = p
				epoch = current.Epoch
				activeMode = m
				s.cameraStatus("CAPTURING / AWAITING FIRST IDR", nil)
			}
		}
		if p != nil {
			if err = p.Step(ctx, time.Now()); err != nil {
				return err
			}
			stats := p.Stats()
			s.cameraCounters(stats, d.Stats())
			if stats.USBQueued > 0 {
				s.cameraStatus("H264 QUEUED / HOST RECEIPT NOT MEASURED", nil)
			}
		}
		if hostDemand && p == nil {
			if e := d.Stop(); e != nil {
				return e
			}
		}
		if paused {
			s.cameraStatus("THERMAL PAUSE", nil)
		} else if !current.Enabled {
			s.cameraStatus("DISABLED", nil)
		} else if !hostDemand {
			s.cameraStatus("WAITING FOR WINDOWS CAMERA", nil)
		}
		nextInterval := 20 * time.Millisecond
		if p != nil {
			nextInterval = 2 * time.Millisecond
		}
		if nextInterval != interval {
			interval = nextInterval
			tick.Reset(interval)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sourceReady:
		case <-tick.C:
		}
	}
	return ctx.Err()
}

// RefreshCameraCatalog does not stream, but native identity reads may initialize
// calibration hardware. Call only after an explicit camera request.
func (s *State) RefreshCameraCatalog(provider camera.Provider) {
	c := camera.InspectModes(provider, s.CameraCurrent().Settings)
	s.mu.Lock()
	s.CameraModes = c
	s.mu.Unlock()
}
func (s *State) CameraCatalog() camera.Catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append(camera.Catalog(nil), s.CameraModes...)
}

// CameraModeAdmitted never modifies descriptors or rebinds the shared gadget.
func (t *Transport) CameraModeAdmitted(mode camera.Mode) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || (t.cameraFunction == nil && t.cameraPrivate == nil) || !t.cameraTable.Contains(uvcmode.Mode(mode)) {
		return fmt.Errorf("camera mode is absent from the current USB session; active mode unchanged")
	}
	return nil
}

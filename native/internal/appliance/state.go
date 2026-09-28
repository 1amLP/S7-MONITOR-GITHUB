package appliance

import (
	"encoding/binary"
	"fmt"
	"perimode/native/internal/ambient"
	"perimode/native/internal/camera"
	"perimode/native/internal/fb"
	"perimode/native/internal/journal"
	"perimode/native/internal/orientation"
	"perimode/native/pkg/cameractl"
	"perimode/native/pkg/cameraprop"
	"perimode/native/pkg/monitor"
	"sync"
	"time"
)

// State is the single owner of USB stream generation and settings. Queues carry
// compressed access units, never unbounded decoded images.
type faultEnvelope struct {
	Generation uint64
	Event      journal.Event
}

type HostDriverFailure struct {
	At         time.Time `json:"at"`
	Generation uint32    `json:"generation"`
	Code       uint32    `json:"hresult"`
	Count      uint64    `json:"count"`
}

func (s *State) HostFailureSnapshot() HostDriverFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.LastDriverFailure
}

type State struct {
	View                                                     ScreenView
	sniperInput                                              sniperInputQueue
	InstallerActive                                          bool
	LastDriverFailure                                        HostDriverFailure
	InstallerStatus                                          string
	CPUEco                                                   bool
	HapticPercent                                            int
	Haptics                                                  HapticRuntime
	Power                                                    PowerRuntime
	PackageRelease                                           uint32
	Endpoints                                                EndpointStatus
	TouchResumeKind                                          byte
	frameRate                                                frameRateWindow
	USBTrialFPS                                              uint32
	monitorChange                                            chan struct{}
	link                                                     monitorLinkRuntime
	CameraTrial                                              CameraTrialRuntime
	Camera3A                                                 Camera3ARuntime
	Camera3APreferences                                      camera.ControlPreferences
	CameraImages                                             [2]camera.ImageOptions
	cameraSelection                                          cameractl.Session
	cameraPropertyJobs                                       chan cameraprop.Message
	cameraPropertyAck                                        cameraprop.Message
	cameraPropertyRequest                                    cameraprop.Message
	cameraPropertyResults                                    [8]cameraPropertyResult
	cameraPropertyResultNext                                 uint8
	cameraLEDWake                                            chan struct{}
	CameraLED                                                CameraLEDRuntime
	cameraPublished                                          [2]byte
	cameraUSBReady, cameraUSBStreaming, cameraOwnershipFault bool
	cameraDynamicFormat                                      bool
	metrics                                                  MonitorMetrics
	Torch                                                    TorchRuntime
	Ambient                                                  AmbientRuntime
	Preview                                                  PreviewRuntime
	faultGeneration                                          uint64
	LastBlit                                                 time.Time
	AudioSpeakerLast, AudioMicrophoneLast                    time.Time
	BatteryPercent                                           int
	BatteryStatus                                            string
	faults                                                   chan faultEnvelope
	diagnosticEnabled                                        bool
	DiagnosticStatus                                         string
	FirstFault                                               *journal.Record
	Rotation                                                 orientation.Settings
	ActiveRotation                                           orientation.Degrees
	RotationSerial                                           uint64
	RotationStatus, RotationSource                           string
	RotationSamples, RotationChanges                         uint64
	RotationLastSample                                       time.Time
	Brightness                                               int
	BrightnessActual                                         int
	BrightnessStatus                                         string
	Camera                                                   CameraRuntime
	CameraModes                                              camera.Catalog
	EncoderTest                                              any
	mu                                                       sync.Mutex
	Settings                                                 monitor.Settings
	Generation, KeyRequest, Ack, HostState, HostError        uint32
	Consumer                                                 bool
	previous, pts                                            uint64
	needKey                                                  bool
	frames                                                   chan monitor.Frame
	Bytes, Received, Dropped, Decoded, Blitted               uint64
	LastFrame, LastPoll, LastDecoded                         time.Time
	LastError                                                string
	Menu                                                     string
	TouchKind                                                byte
	Corner                                                   int
	CPULoad                                                  int
	CPUMilliC, BatteryMilliC                                 int64
	GPUMilliC, ISPMilliC, SoCMilliC                          int64
	Indicators                                               bool
	IndicatorScale                                           int
	PadPollingHz                                             int
	TouchSensorRateStatus                                    string
	GPUProbeStatus                                           string
	GPUProbeError                                            string
	PadSensitivity                                           int
	PadAcceleration                                          bool
	SpeakerVolume, MicrophoneVolume                          int
	SpeakerEnabled, MicrophoneEnabled                        bool
	AudioError                                               string
	AudioOutput                                              string
	AudioSpeakerFrames, AudioMicrophoneFrames                uint64
	AudioMicrophoneInputPeak, AudioMicrophoneOutputPeak      uint16
	AudioMicrophonePeakAt                                    time.Time
	ThermalPaused                                            bool
	ThermalStatus                                            string
	DecoderRetry                                             uint64
	DecoderErrors                                            []string
	RetrySerial                                              uint64
	AudioRetrySerial                                         uint64
	Persistence                                              string
}

func NewState() *State {
	return &State{HapticPercent: defaultHapticPercent, Torch: TorchRuntime{Status: "OFF / NOT REQUESTED"}, Ambient: AmbientRuntime{Settings: ambient.DefaultSettings(), Status: "MANUAL / SENSOR OFF"}, Preview: PreviewRuntime{Options: fb.DefaultPreviewOptions(), Status: "DISABLED"}, BatteryPercent: -1, BatteryStatus: "UNKNOWN", CPUMilliC: -1, GPUMilliC: -1, ISPMilliC: -1, SoCMilliC: -1, BatteryMilliC: -1, faults: make(chan faultEnvelope, 1), DiagnosticStatus: "DISABLED / NO CONSENT", TouchSensorRateStatus: "NOT REQUESTED", Rotation: orientation.DefaultSettings(), ActiveRotation: 270, RotationStatus: "MANUAL", BrightnessStatus: "PANEL NOT OPENED", CameraModes: camera.InspectModes(nil, camera.DefaultSettings()), Camera: CameraRuntime{Settings: camera.DefaultSettings(), Epoch: 1, Status: "OFF"}, Settings: monitor.DefaultSettings(), Generation: 1, KeyRequest: 1, needKey: true, frames: make(chan monitor.Frame, 3), Menu: "", TouchKind: 2, Indicators: true, IndicatorScale: 100, PadPollingHz: 90, Corner: 2, CPULoad: -1, PadSensitivity: 100, SpeakerVolume: 50, MicrophoneVolume: 20, AudioOutput: "OFF"}
}

func (s *State) AudioPeakSnapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"input_peak": s.AudioMicrophoneInputPeak,
		"usb_peak":   s.AudioMicrophoneOutputPeak,
		"sample_at":  s.AudioMicrophonePeakAt,
	}
}
func (s *State) Error(e error) {
	if e != nil {
		s.mu.Lock()
		s.LastError = e.Error()
		s.mu.Unlock()
	}
}
func (s *State) decoderError(e error) {
	if e == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	const limit = 4
	if len(s.DecoderErrors) == limit {
		copy(s.DecoderErrors, s.DecoderErrors[1:])
		s.DecoderErrors = s.DecoderErrors[:limit-1]
	}
	s.DecoderErrors = append(s.DecoderErrors, e.Error())
	s.LastError = e.Error()
}
func (s *State) drainLocked() {
	for {
		select {
		case <-s.frames:
			s.Dropped++
		default:
			return
		}
	}
}
func (s *State) resyncLocked() {
	s.needKey = true
	s.KeyRequest++
	if s.KeyRequest == 0 {
		s.KeyRequest = 1
	}
	s.drainLocked()
}
func (s *State) Configure(v monitor.Settings) error {
	if e := v.Validate(); e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v != s.Settings {
		s.invalidateMonitorLocked()
		s.Settings = v
		if !s.viewAllowedLocked(s.View) {
			s.View = ViewMonitor
		}
		s.Generation++
		if s.Generation == 0 {
			s.Generation = 1
		}
		s.Ack = 0
		s.previous = 0
		s.pts = 0
		s.resyncLocked()
	}
	return nil
}
func (s *State) Disconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateMonitorLocked()
	s.sniperInput = sniperInputQueue{}
	s.Torch.Wanted = false
	s.Generation++
	if s.Generation == 0 {
		s.Generation = 1
	}
	s.LastDecoded = time.Time{}
	s.LastBlit = time.Time{}
	s.Camera.LastActivity = time.Time{}
	s.AudioSpeakerLast = time.Time{}
	s.AudioMicrophoneLast = time.Time{}
	s.HostState = 0
	s.HostError = 0
	s.Ack = 0
	s.previous = 0
	s.pts = 0
	s.LastPoll = time.Time{}
	s.resyncLocked()
}
func (s *State) Keyframe() { s.mu.Lock(); defer s.mu.Unlock(); s.resyncLocked() }
func (s *State) RejectTransportFrame(e error) {
	if e == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Dropped++
	s.LastError = "discarded monitor frame: " + e.Error()
	s.resyncLocked()
}
func (s *State) Configuration() [48]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	// A resumed poll alone cannot resurrect a stale driver's acknowledgement.
	if !s.LastPoll.IsZero() && !recentLink(now, s.LastPoll, linkHeartbeatLimit) {
		s.Ack, s.HostState, s.HostError = 0, 0, 0
	}
	s.LastPoll = now
	var b [48]byte
	copy(b[:], "S7C1")
	le := binary.LittleEndian
	le.PutUint16(b[4:], 2)
	flags := uint16(0)
	if s.Settings.Enabled {
		flags |= 1
	}
	if s.Consumer && !s.ThermalPaused && s.monitorVisibleLocked() {
		flags |= 2
	}
	le.PutUint16(b[6:], flags)
	if s.activeViewLocked() == ViewSniper {
		le.PutUint16(b[4:], 3)
		flags |= 4 | uint16(s.Settings.Zoom())<<4
		if s.Settings.SniperStretch {
			flags |= 8
		}
		le.PutUint16(b[6:], flags)
	}
	w, h := s.Settings.Dimensions()
	for i, v := range []uint32{s.Generation, w, h, s.Settings.FPS, s.Settings.Bitrate, s.KeyRequest, s.Ack, s.HostState, s.HostError, s.Settings.GOPSeconds} {
		le.PutUint32(b[8+i*4:], v)
	}
	return b
}
func (s *State) HostStatus(b []byte) error {
	if len(b) != 12 {
		return fmt.Errorf("bad host-status size")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	le := binary.LittleEndian
	if le.Uint32(b) != s.Generation {
		return nil
	}
	v := le.Uint32(b[4:])
	if v > 3 {
		return fmt.Errorf("bad host state")
	}
	code := le.Uint32(b[8:])
	if (v == 3 || code != 0) && (s.LastDriverFailure.Generation != s.Generation || s.LastDriverFailure.Code != code || s.HostState != v || s.HostError != code) {
		s.LastDriverFailure = HostDriverFailure{At: time.Now(), Generation: s.Generation, Code: code, Count: s.LastDriverFailure.Count + 1}
	}
	s.Ack = s.Generation
	s.HostState = v
	s.HostError = le.Uint32(b[8:])
	if v <= 2 && s.HostError == 0 {
		s.link.HadReply = true
	}
	return nil
}
func (s *State) Publish(f monitor.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Bytes += uint64(len(f.Payload) + 48)
	w, h := s.Settings.Dimensions()
	fw, fh := f.Dimensions()
	if !s.Consumer || s.ThermalPaused || !s.Settings.Enabled || !s.monitorVisibleLocked() || f.Generation != s.Generation || f.FPS != s.Settings.FPS || fw != w || fh != h {
		s.Dropped++
		return nil
	}
	if s.previous != 0 && (f.Sequence <= s.previous || f.PTS <= s.pts) {
		if f.Sequence == s.previous && f.PTS == s.pts {
			s.Dropped++ // A duplicate is not a new desktop frame.
			return nil
		}
		if !f.Key {
			s.Dropped++
			if !s.needKey {
				s.resyncLocked()
			}
			return nil
		}
		// A restarted Windows host can reset its monotonic sequence while the
		// USB cable stays connected. Admit only a complete IDR as the new origin.
		s.previous, s.pts = 0, 0
		s.resyncLocked()
	}
	if f.Sequence != s.previous+1 {
		s.resyncLocked()
	}
	s.previous, s.pts = f.Sequence, f.PTS
	if s.needKey && !f.Key {
		s.Dropped++
		return nil
	}
	if f.Key {
		s.needKey = false
	}
	select {
	case s.frames <- f:
		s.Received++
		s.LastFrame = time.Now()
		s.metrics.received(s.Generation, f.PTS, s.LastFrame, len(s.frames))
	default:
		s.Dropped++
		s.resyncLocked()
	}
	return nil
}
func (s *State) CountDecoded(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Decoded += uint64(n)
	if n > 0 {
		s.LastDecoded = time.Now()
	}
}
func (s *State) CountBlit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Blitted++
	s.LastBlit = time.Now()
	s.link.HadReply = true
}
func (s *State) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{"usb_trial_fps": s.USBTrialFPS, "monitor_link": s.monitorLinkLocked(time.Now()), "monitor_timing": s.metrics.snapshot(), "rear_torch": s.Torch, "first_fault": s.FirstFault, "diagnostic_status": s.DiagnosticStatus, "device_rotation": map[string]any{"settings": s.Rotation, "active_degrees": s.ActiveRotation, "status": s.RotationStatus, "source": s.RotationSource, "samples": s.RotationSamples, "changes": s.RotationChanges, "last_sample": s.RotationLastSample, "hardware_tested": false}, "auto_brightness": s.Ambient, "brightness": map[string]any{"saved_percent": s.Brightness, "actual_percent": s.BrightnessActual, "status": s.BrightnessStatus}, "settings": s.Settings, "generation": s.Generation, "consumer": s.Consumer, "host_recent": !s.LastPoll.IsZero() && time.Since(s.LastPoll) < 3*time.Second, "bytes": s.Bytes, "received": s.Received, "dropped": s.Dropped, "decoded": s.Decoded, "cpu_blits": s.Blitted, "physical_presentations": "not measured", "last_error": s.LastError, "decoder_errors": append([]string(nil), s.DecoderErrors...), "android_framework": false, "android_vendor_components": true, "runtime_scope": "native PID1 with isolated, selected Android/vendor dependencies", "camera_backend": "native FIMC-IS/ISP with source-derived profiles; hardware unverified", "camera_3a": s.Camera3A, "camera": s.Camera, "local_preview": s.Preview, "camera_modes": append(camera.Catalog(nil), s.CameraModes...), "audio_backend": "native ALSA (disabled until explicit selection)", "audio_error": s.AudioError, "audio_output": s.AudioOutput, "speaker_frames": s.AudioSpeakerFrames, "microphone_frames": s.AudioMicrophoneFrames, "thermal_status": s.ThermalStatus, "thermal_paused": s.ThermalPaused, "decoder_retries": s.DecoderRetry, "hardware_tested": false, "native_encoder_diagnostic": s.EncoderTest, "native_high_fps_trial": s.CameraTrial, "cpu_percent": s.CPULoad, "cpu_source": "/proc/stat delta", "cpu_millic": s.CPUMilliC, "gpu_millic": s.GPUMilliC, "isp_millic": s.ISPMilliC, "soc_millic": s.SoCMilliC, "battery_millic": s.BatteryMilliC, "battery_percent": s.BatteryPercent, "battery_status": s.BatteryStatus, "battery_source": "/sys/class/power_supply/battery/capacity"}
}
func (s *State) Current() (monitor.Settings, uint32, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Settings, s.Generation, s.Menu
}

func (s *State) SetThermalPaused(paused bool, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ThermalStatus = reason
	if paused {
		s.Torch.Wanted = false
	}
	if s.ThermalPaused != paused {
		s.invalidateMonitorLocked()
		s.ThermalPaused = paused
		s.Ack = 0
		s.Generation++
		if s.Generation == 0 {
			s.Generation = 1
		}
		s.previous = 0
		s.pts = 0
		s.LastDecoded = time.Time{}
		s.resyncLocked()
	}
}
func (s *State) SetConsumer(ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Consumer != ready {
		s.invalidateMonitorLocked()
	}
	s.Consumer = ready
}
func (s *State) MonitorRunnable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Settings.Enabled && !s.ThermalPaused && s.monitorVisibleLocked()
}

// Rear Preview replaces only the video consumer. Keep the Windows monitor
// attached and its Enabled preference unchanged while the camera owns VPP.
func (s *State) monitorVisibleLocked() bool {
	return s.activeViewLocked() != ViewCamera
}

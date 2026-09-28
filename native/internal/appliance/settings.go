package appliance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"perimode/native/internal/ambient"
	"perimode/native/internal/camera"
	"perimode/native/internal/fb"
	"perimode/native/internal/orientation"
	"perimode/native/pkg/monitor"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type Preferences struct {
	HapticPercent     int                       `json:"haptic_feedback_percent"`
	CPUEco            bool                      `json:"cpu_eco,omitempty"`
	CameraEnabled     bool                      `json:"camera_enabled"`
	PreviewEnabled    bool                      `json:"preview_enabled"`
	SpeakerEnabled    bool                      `json:"speaker_enabled"`
	MicrophoneEnabled bool                      `json:"microphone_enabled"`
	IndicatorScale    int                       `json:"indicator_scale_percent"`
	PadPollingHz      int                       `json:"pad_polling_hz"`
	Camera3A          camera.ControlPreferences `json:"camera_3a"`
	CameraImages      [2]camera.ImageOptions    `json:"camera_images"`
	AutoBrightness    ambient.Settings          `json:"auto_brightness"`
	Preview           fb.PreviewOptions         `json:"preview_placement"`
	Rotation          orientation.Settings      `json:"device_rotation"`
	Brightness        int                       `json:"panel_brightness_percent"`
	Camera            camera.Settings           `json:"camera"`
	Version           int                       `json:"version"`
	Monitor           monitor.Settings          `json:"monitor"`
	TouchKind         byte                      `json:"touch_kind"`
	TouchResumeKind   byte                      `json:"touch_resume_kind,omitempty"`
	PadSensitivity    int                       `json:"pad_sensitivity_percent"`
	PadAcceleration   bool                      `json:"pad_acceleration"`
	Indicators        bool                      `json:"indicators"`
	Corner            int                       `json:"indicator_corner"`
	SpeakerVolume     int                       `json:"speaker_volume"`
	MicrophoneVolume  int                       `json:"microphone_volume"`
}

const settingsVersion = 15

func DefaultPreferences() Preferences {
	return Preferences{HapticPercent: defaultHapticPercent, AutoBrightness: ambient.DefaultSettings(), Preview: fb.DefaultPreviewOptions(), Rotation: orientation.DefaultSettings(), Version: settingsVersion, Monitor: monitor.DefaultSettings(), TouchKind: 2, PadSensitivity: 100, Indicators: true, IndicatorScale: 100, PadPollingHz: 90, Corner: 2, SpeakerVolume: 50, MicrophoneVolume: 20, Camera: camera.DefaultSettings()}
}
func (p Preferences) Validate() error {
	if p.HapticPercent < 0 || p.HapticPercent > 100 {
		return fmt.Errorf("invalid haptic feedback strength")
	}
	if p.TouchResumeKind > 2 {
		return fmt.Errorf("invalid saved touch resume mode")
	}
	if !p.Camera.Mode.WebcamEligible() {
		return fmt.Errorf("hidden or removed current camera mode")
	}
	if !fb.ValidIndicatorScale(p.IndicatorScale) || !validPadPolling(p.PadPollingHz) {
		return fmt.Errorf("invalid indicator size or touchpad polling rate")
	}
	if p.Version != settingsVersion || p.TouchKind > 2 || p.PadSensitivity < 25 || p.PadSensitivity > 300 || p.Corner < 0 || p.Corner > 3 || p.SpeakerVolume < 0 || p.SpeakerVolume > 100 || p.MicrophoneVolume < 0 || p.MicrophoneVolume > 100 {
		return fmt.Errorf("invalid native preferences")
	}
	if p.Brightness != 0 && (p.Brightness < 5 || p.Brightness > 100) {
		return fmt.Errorf("brightness outside normal 5..100 percent range")
	}
	return errors.Join(p.Monitor.Validate(), p.Camera.Validate(), p.Rotation.Validate(), p.Preview.Validate(), p.AutoBrightness.Validate(), p.Camera3A.Validate(), p.CameraImages[0].Validate(), p.CameraImages[1].Validate())
}

type settingsRecord struct {
	Sequence uint64          `json:"sequence"`
	Payload  json.RawMessage `json:"payload"`
	SHA256   string          `json:"sha256"`
}
type SettingsStore struct {
	mu        sync.Mutex
	Directory string
	sequence  uint64
}

func decodeSettings(b []byte) (Preferences, uint64, error) {
	var rec settingsRecord
	var p Preferences
	if len(b) > 16384 {
		return p, 0, fmt.Errorf("oversized settings")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&rec); e != nil {
		return p, 0, e
	}
	if e := dec.Decode(new(any)); e != io.EOF {
		return p, 0, fmt.Errorf("trailing settings data")
	}
	sum := sha256.Sum256(rec.Payload)
	if rec.Sequence == 0 || rec.Sequence == ^uint64(0) || rec.SHA256 != hex.EncodeToString(sum[:]) {
		return p, 0, fmt.Errorf("settings checksum/sequence invalid")
	}
	dec = json.NewDecoder(bytes.NewReader(rec.Payload))
	dec.DisallowUnknownFields()
	if e := dec.Decode(&p); e != nil {
		return p, 0, e
	}
	if p.Version < 15 && (p.Monitor.SniperRotation != 0 || p.Monitor.SniperMirror) {
		return p, 0, fmt.Errorf("unexpected sniper transform in old settings")
	}
	// Go silently ignores excess JSON array elements when decoding fixed arrays.
	// Inspect the saved array length before accepting either historical or new data.
	var shape struct {
		Camera3A      []camera.ControlSelection `json:"camera_3a"`
		CameraImages  []camera.ImageOptions     `json:"camera_images"`
		HapticPercent *int                      `json:"haptic_feedback_percent"`
	}
	if e := json.Unmarshal(rec.Payload, &shape); e != nil {
		return p, 0, e
	}
	if shape.CameraImages == nil {
		p.CameraImages = [2]camera.ImageOptions{p.Camera.Image, p.Camera.Image}
	} else if len(shape.CameraImages) != 2 {
		return p, 0, fmt.Errorf("invalid camera image bank length")
	}
	if p.Version >= 1 && p.Version <= 13 && shape.HapticPercent == nil {
		p.HapticPercent = defaultHapticPercent
	}
	if (p.Version >= 8 && p.Version <= 9 && len(shape.Camera3A) != 5) ||
		(p.Version >= 10 && p.Version <= settingsVersion && len(shape.Camera3A) != 10) ||
		(p.Version < 8 && len(shape.Camera3A) > 5) {
		return p, 0, fmt.Errorf("invalid camera controls slot count for settings schema")
	}
	if p.Version <= 10 && (p.CameraEnabled || p.PreviewEnabled || p.SpeakerEnabled || p.MicrophoneEnabled) {
		return p, 0, fmt.Errorf("legacy settings contain new device enable states")
	}
	if p.Version <= 11 && p.Preview.Fullscreen {
		return p, 0, fmt.Errorf("legacy schema has unexpected fullscreen preview")
	}
	if p.Version <= 11 && p.TouchResumeKind != 0 {
		return p, 0, fmt.Errorf("legacy schema has unexpected input resume mode")
	}
	// Migrate the previous verified schema only after its original checksum
	// passed. Do not reset monitor/input/audio preferences or enable any device.
	if p.Version == 1 {
		if p.Camera != (camera.Settings{}) {
			return p, 0, fmt.Errorf("version 1 record contains unexpected camera data")
		}
		p.Version = 2
		p.Camera = camera.DefaultSettings()
	}
	if p.Version == 2 {
		if p.Camera.Image != (camera.ImageOptions{}) {
			return p, 0, fmt.Errorf("version 2 record has unexpected image options")
		}
		p.Version = 3
	}
	if p.Version == 3 {
		if p.Rotation != (orientation.Settings{}) || p.Brightness != 0 {
			return p, 0, fmt.Errorf("version 3 record contains unexpected device preferences")
		}
		p.Rotation = orientation.DefaultSettings()
		p.Version = 4
	}
	if p.Version == 4 {
		if p.Preview != (fb.PreviewOptions{}) {
			return p, 0, fmt.Errorf("legacy settings contain preview placement")
		}
		p.Preview = fb.DefaultPreviewOptions()
		p.Version = 5
	}
	if p.Version == 5 {
		if p.AutoBrightness != (ambient.Settings{}) {
			return p, 0, fmt.Errorf("legacy settings contain unexpected auto brightness")
		}
		p.AutoBrightness = ambient.DefaultSettings()
		p.Version = 6
	}

	if p.Version == 6 {
		if p.Camera.Image.ZoomPercent != 0 {
			return p, 0, fmt.Errorf("schema6 contains unexpected zoom")
		}
		p.Version = 7
	}
	if p.Version == 7 {
		if p.Camera3A != (camera.ControlPreferences{}) {
			return p, 0, fmt.Errorf("legacy schema has unexpected 3A controls")
		}
		p.Version = 8
	}
	if p.Version == 8 {
		if p.IndicatorScale != 0 || p.PadPollingHz != 0 {
			return p, 0, fmt.Errorf("legacy schema contains new UI/HID settings")
		}
		p.IndicatorScale = 100
		p.PadPollingHz = 125
		p.Version = 9
	}
	if p.Version == 9 {
		legacyValid := false
		legacyModes := []camera.Mode{{Width: 3840, Height: 2160, FPS: 30}, {Width: 1920, Height: 1080, FPS: 30}, {Width: 1280, Height: 720, FPS: 120}}
		if p.Camera.Sensor == camera.Front {
			legacyModes = []camera.Mode{{Width: 1920, Height: 1080, FPS: 30}, {Width: 1280, Height: 720, FPS: 30}}
		}
		if p.Camera.Sensor <= camera.Front {
			for _, m := range legacyModes {
				legacyValid = legacyValid || p.Camera.Mode == m
			}
		}
		if !legacyValid {
			return p, 0, fmt.Errorf("invalid legacy camera identity")
		}
		old := p.Camera3A
		// Validate every original slot (including the removed 4K slot) before remap.
		if e := old.Validate(); e != nil {
			return p, 0, e
		}
		p.Camera3A = camera.ControlPreferences{}
		// Old: rear4K, rear1080p30, rear720p120, front1080p30, front720p30.
		for from, to := range map[int]int{1: 2, 2: 4, 3: 8, 4: 9} {
			p.Camera3A[to] = old[from]
		}
		if p.Camera.Sensor == camera.Rear && p.Camera.Mode == (camera.Mode{Width: 3840, Height: 2160, FPS: 30}) {
			p.Camera.Mode = camera.Mode{Width: 1920, Height: 1080, FPS: 30}
		} else if !p.Camera.Mode.Valid(p.Camera.Sensor) {
			return p, 0, fmt.Errorf("invalid legacy camera mode")
		}
		// Hidden FPS selections may remain stored as per-mode controls but cannot be
		// restored as the public current mode. Preserve unrelated image/audio/UI fields.
		if !p.Camera.Mode.WebcamEligible() {
			p.Camera.Mode = camera.Mode{Width: 1920, Height: 1080, FPS: 30}
		}
		p.Version = 10
	}
	// Monitor transport is now fixed at 60 Hz. Preserve every other validated
	// schema-10 preference while migrating the removed 30 Hz selection.
	if p.Version >= 10 && p.Version <= settingsVersion && p.Monitor.FPS == 30 {
		p.Monitor.FPS = 60
	}
	// Older HID service limits exceeded this controller's 90 Hz maximum.
	if p.Version >= 10 && p.Version <= settingsVersion && (p.PadPollingHz == 125 || p.PadPollingHz == 500 || p.PadPollingHz == 1000) {
		p.PadPollingHz = 90
	}
	if p.Version == 12 {
		if p.Monitor.SniperEnabled || p.Monitor.SniperStretch || p.Monitor.SniperZoom != 0 || p.Monitor.SniperX != 0 || p.Monitor.SniperY != 0 {
			return p, 0, fmt.Errorf("unexpected sniper fields in old settings")
		}
		p.Version = settingsVersion
	}
	if p.Version == 10 {
		p.Version = settingsVersion
	}
	if p.Version == 11 {
		p.Version = settingsVersion
	}
	if p.Version == 13 {
		p.Version = settingsVersion
	}
	if p.Version == 14 {
		p.Version = settingsVersion
	}
	if e := p.Validate(); e != nil {
		return p, 0, e
	}
	return p, rec.Sequence, nil
}

// SettingsStore never chooses or mounts a partition. Its caller must explicitly
// supply an owned directory. Symlinks and nonregular slot files are rejected.
func (s *SettingsStore) checkDir() error {
	if !filepath.IsAbs(s.Directory) {
		return fmt.Errorf("absolute settings directory required")
	}
	path := filepath.Clean(s.Directory)
	for path != "/" {
		st, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe settings directory")
		}
		path = filepath.Dir(path)
	}
	return nil
}
func (s *SettingsStore) Load() (Preferences, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.checkDir(); e != nil {
		return Preferences{}, e
	}
	var best Preferences
	seq := uint64(0)
	var errs []error
	for _, name := range []string{"settings-a.json", "settings-b.json"} {
		path := filepath.Join(s.Directory, name)
		st, e := os.Lstat(path)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			errs = append(errs, e)
			continue
		}
		if !st.Mode().IsRegular() || st.Size() > 16384 {
			errs = append(errs, fmt.Errorf("unsafe settings slot %s", name))
			continue
		}
		b, e := os.ReadFile(path)
		if e != nil {
			errs = append(errs, e)
			continue
		}
		p, n, e := decodeSettings(b)
		if e != nil {
			errs = append(errs, e)
			continue
		}
		if n > seq {
			best, seq = p, n
		}
	}
	if seq == 0 {
		if len(errs) > 0 {
			return best, errors.Join(errs...)
		}
		return best, os.ErrNotExist
	}
	s.sequence = seq
	return best, nil
}
func (s *SettingsStore) Save(p Preferences) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := p.Validate(); e != nil {
		return e
	}
	if e := s.checkDir(); e != nil {
		return e
	}
	payload, e := json.Marshal(p)
	if e != nil {
		return e
	}
	sum := sha256.Sum256(payload)
	next := s.sequence + 1
	if next == 0 || next == ^uint64(0) {
		return fmt.Errorf("settings sequence exhausted")
	}
	b, e := json.Marshal(settingsRecord{next, payload, hex.EncodeToString(sum[:])})
	if e != nil {
		return e
	}
	name := "settings-a.json"
	if next%2 == 0 {
		name = "settings-b.json"
	}
	dst := filepath.Join(s.Directory, name)
	if st, e := os.Lstat(dst); e == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("unsafe settings destination")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	f, e := os.CreateTemp(s.Directory, ".settings-*")
	if e != nil {
		return e
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, e = f.Write(append(b, '\n')); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(temp, dst); e != nil {
		return e
	}
	d, e := os.Open(s.Directory)
	if e != nil {
		return e
	}
	e = d.Sync()
	ce := d.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	s.sequence = next
	return nil
}
func (s *State) Preferences() Preferences {
	s.mu.Lock()
	defer s.mu.Unlock()
	images := s.CameraImages
	images[s.Camera.Settings.Sensor] = s.Camera.Settings.Image
	return Preferences{CameraImages: images, HapticPercent: s.HapticPercent, CPUEco: s.CPUEco, TouchResumeKind: s.TouchResumeKind, CameraEnabled: s.Camera.Enabled, PreviewEnabled: s.Preview.Enabled, SpeakerEnabled: s.SpeakerEnabled, MicrophoneEnabled: s.MicrophoneEnabled, AutoBrightness: s.Ambient.Settings, Preview: s.Preview.Options, Rotation: s.Rotation, Brightness: s.Brightness, Version: settingsVersion, Monitor: s.Settings, TouchKind: s.TouchKind, PadSensitivity: s.PadSensitivity, PadAcceleration: s.PadAcceleration, Indicators: s.Indicators, IndicatorScale: s.IndicatorScale, PadPollingHz: s.PadPollingHz, Corner: s.Corner, SpeakerVolume: s.SpeakerVolume, MicrophoneVolume: s.MicrophoneVolume, Camera: s.Camera.Settings, Camera3A: s.Camera3APreferences}
}
func (s *State) ApplyPreferences(p Preferences) error {
	if e := p.Validate(); e != nil {
		return e
	}
	if e := s.Configure(p.Monitor); e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCameraChangeLocked(p.Camera); err != nil {
		return err
	}
	s.configureCameraLocked(p.Camera)
	s.Camera.Settings.Image = p.Camera.Image
	s.CameraImages = p.CameraImages
	s.CameraImages[p.Camera.Sensor] = p.Camera.Image
	s.Camera.Enabled = p.CameraEnabled
	s.CPUEco = p.CPUEco
	s.HapticPercent = p.HapticPercent
	s.Camera.Epoch++
	s.Preview.Enabled = p.PreviewEnabled
	s.Preview.Retry++
	s.SpeakerEnabled = p.SpeakerEnabled && p.SpeakerVolume > 0
	s.MicrophoneEnabled = p.MicrophoneEnabled && p.MicrophoneVolume > 0
	s.Camera3APreferences = p.Camera3A
	s.Ambient.Settings = p.AutoBrightness
	s.Ambient.Revision++
	s.Ambient.Fallback = p.Brightness
	s.Preview.Options = p.Preview
	s.Rotation = p.Rotation
	s.Brightness = p.Brightness
	s.TouchKind = p.TouchKind
	s.TouchResumeKind = p.TouchResumeKind
	s.PadSensitivity = p.PadSensitivity
	s.PadAcceleration = p.PadAcceleration
	s.Indicators = p.Indicators
	s.IndicatorScale = p.IndicatorScale
	s.PadPollingHz = p.PadPollingHz
	s.Corner = p.Corner
	s.SpeakerVolume = p.SpeakerVolume
	s.MicrophoneVolume = p.MicrophoneVolume
	return nil
}

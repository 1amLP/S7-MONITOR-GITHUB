// Package camera owns the native camera contract, independently of USB and UI.
// A target mode is NOT evidence that the S7 sensor/ISP can provide it natively.
package camera

import (
	"encoding/binary"
	"errors"
	"fmt"
	"perimode/native/internal/media"
	"perimode/native/pkg/cameramode"
)

type Sensor = cameramode.Sensor

const (
	Rear  = cameramode.Rear
	Front = cameramode.Front
)

type Mode = cameramode.Mode

func Modes(s Sensor) []Mode        { return cameramode.Modes(s) }
func VisibleModes(s Sensor) []Mode { return cameramode.VisibleModes(s) }

type Settings struct {
	Sensor     Sensor       `json:"sensor"`
	Mode       Mode         `json:"mode"`
	Bitrate    uint32       `json:"bitrate"`
	GOPSeconds uint32       `json:"gop_seconds"`
	Image      ImageOptions `json:"image"`
}

func DefaultSettings() Settings {
	return Settings{Sensor: Front, Mode: Mode{Width: 1920, Height: 1080, FPS: 30}, Bitrate: 8_000_000, GOPSeconds: 1}
}
func (s Settings) Validate() error {
	if !s.Mode.Valid(s.Sensor) || s.GOPSeconds < 1 || s.GOPSeconds > 5 {
		return fmt.Errorf("invalid camera sensor/mode/GOP: %+v", s)
	}
	return errors.Join(s.Encoder().Validate(), s.Image.Validate())
}
func (s Settings) Encoder() media.EncodeSettings {
	return media.EncodeSettings{Width: s.Mode.Width, Height: s.Mode.Height, FPS: s.Mode.FPS, Bitrate: s.Bitrate, GOP: s.Mode.FPS * s.GOPSeconds}
}
func (s Settings) Preference() [16]byte {
	var b [16]byte
	copy(b[:], "S7W1")
	binary.LittleEndian.PutUint32(b[4:], s.Mode.Width)
	binary.LittleEndian.PutUint32(b[8:], s.Mode.Height)
	binary.LittleEndian.PutUint32(b[12:], s.Mode.FPS)
	return b
}

var ErrSensorGraph = errors.New("S7 FIMC-IS sensor startup, calibrated profiles and streaming graph are not integrated; native webcam unavailable")
var ErrOwnership = errors.New("camera DMA ownership unresolved; retain resources until reboot")

// Provider is intentionally separate from the generic V4L2 raw-buffer adapter.
// The latter cannot magically configure a Samsung FIMC-IS processing pipeline.
type Provider interface {
	Available(Settings) error
	Open(Settings) (Source, error)
}

// Fixed UVC descriptors advertise the union of all target sensor modes. A partial
// provider must not expose that union as if every entry could produce real frames.
// Available is required to be read-only: never power a sensor during this gate.
func RequireMatrix(p Provider, selected Settings) error {
	if p == nil {
		return ErrSensorGraph
	}
	if e := p.Available(selected); e != nil {
		return e
	}
	for _, sensor := range []Sensor{Rear, Front} {
		for _, mode := range VisibleModes(sensor) {
			v := selected
			v.Sensor = sensor
			v.Mode = mode
			if e := p.Available(v); e != nil {
				return fmt.Errorf("fixed UVC matrix missing %s %s: %w", sensor, mode, e)
			}
		}
	}
	return nil
}

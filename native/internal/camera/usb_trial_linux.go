//go:build linux && (amd64 || arm64)

package camera

import (
	"fmt"
	"perimode/native/pkg/cameramode"
)

// This constructor is selected only by an explicit ramdisk engineering option.
// It retains the SAME Herolte owner, source profiles, firmware and DMA teardown.
// Ordinary HerolteProvider and InspectModes still reject high-FPS publication.
type usbTrialProvider struct {
	HerolteProvider
	mode Mode
}

func NewUSBTrialProvider(fps uint32) (Provider, error) {
	m, err := cameramode.USBTrialMode(fps)
	if err != nil {
		return nil, err
	}
	return usbTrialProvider{mode: m}, nil
}
func (p usbTrialProvider) USBTrialFPS() uint32 { return p.mode.FPS }
func (p usbTrialProvider) Available(s Settings) error {
	if s.Mode != p.mode || !isHighFPSTrialSettings(s) {
		return fmt.Errorf("USB trial accepts only its fixed rear mode without transforms")
	}
	return p.HerolteProvider.available(s, true)
}
func (p usbTrialProvider) Open(s Settings) (Source, error) {
	if e := p.Available(s); e != nil {
		return nil, e
	}
	return p.HerolteProvider.openSession(s, true)
}
func USBTrialFPS(p Provider) uint32 {
	if v, ok := p.(interface{ USBTrialFPS() uint32 }); ok {
		fps := v.USBTrialFPS()
		if _, err := cameramode.USBTrialMode(fps); err == nil {
			return fps
		}
	}
	return 0
}
func (p *SharedProvider) USBTrialFPS() uint32 { return USBTrialFPS(p.backend) }

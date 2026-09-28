//go:build linux && (amd64 || arm64)

package fimcdma

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
)

// SetSensorParameters is VIDIOC_S_PARM. The supplied m_initFlitePipe sends
// CAPTURE_MPLANE, numerator=1, denominator=FPS before S_FMT. S_INPUT here uses
// the bare module selector, NOT PackInput's processing-graph bit fields.
const SetSensorParameters uintptr = 0xc0cc5616

// StreamParameters has the exact Linux v4l2_streamparm size and union alignment.
// Its stream-rate fields are at byte 12/16, not at 8/12 (capturemode is before it).
type StreamParameters struct {
	Type uint32
	Data [200]byte
}

func sensorParameters(fps uint32) StreamParameters {
	p := StreamParameters{Type: media.Capture}
	binary.LittleEndian.PutUint32(p.Data[8:12], 1)
	binary.LittleEndian.PutUint32(p.Data[12:16], fps)
	return p
}
func (p StreamParameters) validateRate(fps uint32) error {
	n := binary.LittleEndian.Uint32(p.Data[8:12])
	d := binary.LittleEndian.Uint32(p.Data[12:16])
	if p.Type != media.Capture || n == 0 || d == 0 || uint64(n)*uint64(fps) != uint64(d) {
		return fmt.Errorf("sensor returned rate %d/%d instead of 1/%d", n, d, fps)
	}
	return nil
}

// SensorConfig contains the physical-sensor format chosen by an established
// module profile. The geometry is NOT the USB output size. No rear/front sensor
// dimensions, Bayer order, module ID, calibration or setfile is guessed here.
type SensorConfig struct {
	KernelABI    string
	Driver, Card string
	ModuleID     uint32
	FPS          uint32
	Format       media.Format
	Role         MetadataRole
}

func (c SensorConfig) Validate() error {
	if e := ValidateKernelABI(c.KernelABI); e != nil {
		return e
	}
	if c.Driver == "" || c.Card == "" || len(c.Driver) >= 16 || len(c.Card) >= 32 {
		return fmt.Errorf("exact sensor identity required")
	}
	if c.ModuleID == 0 || c.ModuleID > 255 || c.FPS == 0 || c.FPS > 240 {
		return fmt.Errorf("physical module 1..255 and sensor FPS 1..240 required")
	}
	if c.Format.Type != media.Capture || c.Format.Planes() != 2 || c.Role != ShotMetadata {
		return fmt.Errorf("FLITE CAPTURE_MPLANE Bayer and separate shot plane required")
	}
	var bits uint64
	switch c.Format.PixelFormat() {
	case 0x30314742:
		bits = 10 // BG10, observed in m_initFlitePipe
	case 0x32314742:
		bits = 12 // BG12
	case 0x32525942:
		bits = 16 // BYR2
	default:
		return fmt.Errorf("unsupported physical sensor Bayer format")
	}
	if e := ValidateTypedLayout(c.Format, c.Role); e != nil {
		return e
	}
	minimum := ((uint64(c.Format.Width())*bits+7)/8 + 15) &^ uint64(15)
	if c.Format.Stride(0)%16 != 0 || uint64(c.Format.Stride(0)) < minimum || uint64(c.Format.Stride(0))*uint64(c.Format.Height()) > uint64(c.Format.PlaneSize(0)) {
		return fmt.Errorf("sensor Bayer plane cannot contain its geometry")
	}
	return nil
}

// ConfigurePhysicalSensor performs QUERYCAP -> bare S_INPUT -> S_PARM -> S_FMT
// -> G_FMT on a borrowed, exclusive fd. It does not allocate buffers, power on
// the stream, select calibration files, or reset another function of the phone.
// The caller must configure the companion first when the module profile needs it.
// On failure discard this fd instance; do not assume kernel input state rolled back.
func ConfigurePhysicalSensor(fd int, c SensorConfig) (*PreparedNode, media.Format, error) {
	if e := c.Validate(); e != nil {
		return nil, media.Format{}, e
	}
	if e := verifyVideoFD(fd); e != nil {
		return nil, media.Format{}, e
	}
	n, format, err := configurePhysicalSensor(fd, c, func(r uintptr, p unsafe.Pointer) error { return linuxio.Ioctl(fd, r, p) })
	if err == nil {
		n.ready = n.completionReady
	}
	return n, format, err
}
func configurePhysicalSensor(fd int, c SensorConfig, call func(uintptr, unsafe.Pointer) error) (*PreparedNode, media.Format, error) {
	fail := func(e error) (*PreparedNode, media.Format, error) { return nil, media.Format{}, e }
	if e := c.Validate(); e != nil {
		return fail(e)
	}
	if e := queryNodeIdentityABI(c.Driver, c.Card, media.Capture, c.KernelABI, call); e != nil {
		return fail(e)
	}
	return configureSelectedSensor(fd, c, call)
}

// configureSelectedSensor requires a successful, same-fd QUERYCAP handshake.
func configureSelectedSensor(fd int, c SensorConfig, call func(uintptr, unsafe.Pointer) error) (*PreparedNode, media.Format, error) {
	fail := func(e error) (*PreparedNode, media.Format, error) { return nil, media.Format{}, e }
	if e := selectPhysicalModule(c.ModuleID, call); e != nil {
		return fail(e)
	}
	p := sensorParameters(c.FPS)
	if e := call(SetSensorParameters, unsafe.Pointer(&p)); e != nil {
		return fail(fmt.Errorf("sensor S_PARM: %w", e))
	}
	if e := p.validateRate(c.FPS); e != nil {
		return fail(e)
	}
	return negotiateNodeABI(fd, c.Format, c.Role, c.KernelABI, call)
}

// ConfigureCompanion issues only QUERYCAP and bare module S_INPUT. The supplied
// HAL opens video109 for this operation. The caller owns the exact descriptor:
// this routine never probes arbitrary video nodes and never invents a reset CID.
func ConfigureCompanion(fd int, driver, card string, moduleID uint32) error {
	if moduleID == 0 || moduleID > 255 {
		return fmt.Errorf("companion module 1..255 required")
	}
	if e := verifyVideoFD(fd); e != nil {
		return e
	}
	return configureCompanion(driver, card, moduleID, func(r uintptr, p unsafe.Pointer) error { return linuxio.Ioctl(fd, r, p) })
}
func configureCompanion(driver, card string, moduleID uint32, call func(uintptr, unsafe.Pointer) error) error {
	if moduleID == 0 || moduleID > 255 {
		return fmt.Errorf("companion module 1..255 required")
	}
	if e := queryNodeIdentity(driver, card, 0, call); e != nil {
		return e
	}
	return selectPhysicalModule(moduleID, call)
}
func selectPhysicalModule(moduleID uint32, call func(uintptr, unsafe.Pointer) error) error {
	selector := moduleID
	if e := call(SetInput, unsafe.Pointer(&selector)); e != nil {
		return fmt.Errorf("physical module S_INPUT: %w", e)
	}
	if selector != moduleID {
		return fmt.Errorf("driver altered physical module selector")
	}
	return nil
}

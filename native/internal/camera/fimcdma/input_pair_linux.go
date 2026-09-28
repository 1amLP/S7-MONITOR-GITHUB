//go:build linux && (amd64 || arm64)

package fimcdma

import (
	"fmt"
	"unsafe"

	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
)

// CompanionConfig identifies the control node required by a verified module
// profile. The module ID is taken from SensorConfig, so the two inputs cannot
// silently select different modules. A nil companion explicitly means that the
// profile does not require one; it is not an automatic fallback on failure.
type CompanionConfig struct{ Driver, Card, KernelABI string }

// ConfigureInputPair completes companion then physical-sensor selection. All
// validation and both QUERYCAP identity checks happen BEFORE any S_INPUT.
// No device paths are guessed or scanned. Borrowed descriptors must be distinct,
// exclusive open file descriptions with no queues allocated. Duplicate-fd aliases
// are prohibited by the caller contract (equal numeric fds are also rejected).
// A failed ioctl may have side effects: discard BOTH input instances on failure;
// this function does not claim an atomic hardware rollback or reset shared USB.
func ConfigureInputPair(companionFD, sensorFD int, companion *CompanionConfig, sensor SensorConfig) (*PreparedNode, media.Format, error) {
	if e := sensor.Validate(); e != nil {
		return nil, media.Format{}, e
	}
	if e := verifyVideoFD(sensorFD); e != nil {
		return nil, media.Format{}, e
	}
	var companionCall func(uintptr, unsafe.Pointer) error
	if companion != nil {
		if companionFD == sensorFD {
			return nil, media.Format{}, fmt.Errorf("companion and sensor descriptors alias")
		}
		if e := verifyVideoFD(companionFD); e != nil {
			return nil, media.Format{}, e
		}
		companionCall = func(r uintptr, p unsafe.Pointer) error { return linuxio.Ioctl(companionFD, r, p) }
	}
	n, format, err := configureInputPair(sensorFD, companion, sensor, companionCall, func(r uintptr, p unsafe.Pointer) error { return linuxio.Ioctl(sensorFD, r, p) })
	if err == nil {
		n.ready = n.completionReady
	}
	return n, format, err
}
func configureInputPair(sensorFD int, companion *CompanionConfig, sensor SensorConfig, companionCall, sensorCall func(uintptr, unsafe.Pointer) error) (*PreparedNode, media.Format, error) {
	fail := func(e error) (*PreparedNode, media.Format, error) { return nil, media.Format{}, e }
	if e := sensor.Validate(); e != nil {
		return fail(e)
	}
	// Take values, not a mutable caller-owned profile, across blocking ioctls.
	var cc CompanionConfig
	if companion != nil {
		cc = *companion
		if cc.KernelABI != sensor.KernelABI {
			return fail(fmt.Errorf("companion/sensor ABI mismatch"))
		}
	}
	if e := queryNodeIdentityABI(sensor.Driver, sensor.Card, media.Capture, sensor.KernelABI, sensorCall); e != nil {
		return fail(e)
	}
	if companion != nil {
		if e := queryNodeIdentityABI(cc.Driver, cc.Card, 0, cc.KernelABI, companionCall); e != nil {
			return fail(e)
		}
		if e := selectPhysicalModule(sensor.ModuleID, companionCall); e != nil {
			return fail(fmt.Errorf("companion input: %w", e))
		}
	}
	// The identities have already been checked. Do not call ConfigureNode: that
	// would send the packed processing-graph route instead of this bare selector.
	return configureSelectedSensor(sensorFD, sensor, sensorCall)
}

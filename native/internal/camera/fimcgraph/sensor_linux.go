//go:build linux && (amd64 || arm64)

package fimcgraph

import (
	"fmt"
	"perimode/native/internal/camera/fimcdma"
	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
	"sync"
	"syscall"
	"unsafe"
)

// Exact CID in the supplied PipeFlite::sensorStream: MOVZ/MOVK at
// 0x88800/0x88804. This is a private ABI of that build, not a generic V4L2 CID.
const SensorStreamCID uint32 = 0x009a100e

// SensorControl takes its own fd reference. Closing/reusing the caller's fd
// cannot redirect a later sensor-stop ioctl to another device. It does not
// select/open a guessed /dev/videoNN or claim that firmware has been loaded.
type SensorControl struct {
	mu                    sync.Mutex
	fd                    int
	ioctl                 func(uintptr, unsafe.Pointer) error
	close                 func() error
	uncertain, on, closed bool
}

func NewSensorControl(configuredFD int, driver, card string) (*SensorControl, error) {
	return NewSensorControlABI(configuredFD, driver, card, "")
}

// The 481 kernel's sensor QUERYCAP is empty. The caller must have verified the
// pinned kernel, exact sysfs identity and dev_t before using this ABI.
func NewSensorControlABI(configuredFD int, driver, card, abi string) (*SensorControl, error) {
	if e := fimcdma.ValidateKernelABI(abi); e != nil {
		return nil, e
	}
	if configuredFD < 0 || driver == "" || card == "" {
		return nil, fmt.Errorf("configured sensor identity required")
	}
	if e := media.VerifyABI(); e != nil {
		return nil, e
	}
	// F_DUPFD_CLOEXEC is atomic; no exec-visible descriptor leak during creation.
	fd, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(configuredFD), syscall.F_DUPFD_CLOEXEC, 0)
	if errno != 0 {
		return nil, errno
	}
	fail := func(e error) (*SensorControl, error) { _ = syscall.Close(int(fd)); return nil, e }
	var st syscall.Stat_t
	if e := syscall.Fstat(int(fd), &st); e != nil {
		return fail(e)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFCHR {
		return fail(fmt.Errorf("sensor fd is not a character device"))
	}
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
	if errno != 0 {
		return fail(errno)
	}
	if flags&syscall.O_ACCMODE != syscall.O_RDWR || flags&syscall.O_NONBLOCK == 0 {
		return fail(fmt.Errorf("nonblocking read/write sensor descriptor required"))
	}
	var caps media.Capability
	if e := linuxio.Ioctl(int(fd), media.QueryCap, unsafe.Pointer(&caps)); e != nil {
		return fail(e)
	}
	if abi == "" && (linuxio.CString(caps.Driver[:]) != driver || linuxio.CString(caps.Card[:]) != card) {
		return fail(fmt.Errorf("sensor identity mismatch"))
	}
	s := &SensorControl{fd: int(fd)}
	s.ioctl = func(req uintptr, p unsafe.Pointer) error { return linuxio.Ioctl(s.fd, req, p) }
	s.close = func() error { return syscall.Close(s.fd) }
	return s, nil
}
func (s *SensorControl) SetStreaming(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("sensor control closed")
	}
	value := int32(0)
	if on {
		value = 1
	}
	c := media.Control{ID: SensorStreamCID, Value: value}
	s.uncertain = true
	if e := s.ioctl(media.SetControl, unsafe.Pointer(&c)); e != nil {
		return e
	}
	if c.ID != SensorStreamCID || c.Value != value {
		return fmt.Errorf("driver changed sensor stream control")
	}
	s.on = on
	s.uncertain = false
	return nil
}
func (s *SensorControl) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if s.on || s.uncertain {
		return fmt.Errorf("sensor stop not confirmed; descriptor retained")
	}
	// On Linux, retrying close after EINTR/EIO can hit an unrelated reused fd.
	e := s.close()
	s.closed = true
	s.fd = -1
	return e
}
func Contract() map[string]any {
	return map[string]any{
		"native_capture_ready": false, "android_library_executed": false,
		"prepared_graph":                             "3AA leader/result -> ISP M2M leader -> MCSC result",
		"metadata_roles":                             "camera2_shot_ext request/result; camera2_stream capture prefix",
		"association":                                "sensor frame count and session epoch, never queue slot index",
		"raw_transfer":                               "fixed shared ION RAW planes; ISP DMA pins delay capture reuse; metadata only copied",
		"source_sha256":                              "9604961bf7e4e8c657b70185f359cb87c368769eb71d24ea6d4b58f5ac8045f3",
		"node_configuration_implemented":             true,
		"prepared_graph_factory_implemented":         true,
		"live_request_controls_implemented":          true,
		"physical_sensor_configuration_implemented":  true,
		"companion_module_selection_implemented":     true,
		"paired_input_configuration_implemented":     true,
		"ordered_flite_stream_switch_implemented":    true,
		"configured_otf_flite_pool_pump_implemented": true,
		"physical_queue_type":                        "CAPTURE_MPLANE (9), not processing OUTPUT_MPLANE (10)",
		"missing":                                    "verified mode/setfile/profile, input descriptor creation, complete calibrated descriptor creation and HerolteProvider integration; no completed cold capture",
	}
}

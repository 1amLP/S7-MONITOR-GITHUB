//go:build linux && (amd64 || arm64)

package media

// This is the encoder ABI of the supplied e41817ea9198/481bdb278a10 kernels, not the newer
// mainline MFC driver. Its encoder has no S_PARM handler. Initial parameters use
// S_EXT_CTRLS; G_CTRL does not read back these initial encoder parameters.
import (
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

const (
	setExtControls       = 0xc0205648
	mpegControlClass     = 0x00990000
	cidFrameRC           = 0x009909d7
	cidMBRC              = 0x009909da
	cidH264IQP           = 0x00990a5e
	cidH264PQP           = 0x00990a5f
	cidH264BQP           = 0x00990a60
	cidH264MinIQP        = 0x00990a61
	cidH264MaxIQP        = 0x00990a62
	cidH264Transform8x8  = 0x00990a63
	cidH264Entropy       = 0x00990a65
	cidH264ReferenceP    = 0x00991136
	cidH264MaxPQP        = 0x009920c9
	cidH264MinPQP        = 0x009920cf
	cidH264MaxBQP        = 0x009920d5
	cidH264MinBQP        = 0x009920d8
	cidClosedGOP         = 0x009909cc
	cidHeaderMode        = 0x009909d8
	cidMFCForceFrame     = 0x00991103
	cidMFCFrameRate      = 0x00992015
	cidMFCPrependHeaders = 0x0099202e
	cidMFCVUI            = 0x0099206a
	cidMFCFrameTag       = 0x00992006
)

type encoderIOCTL func(uintptr, unsafe.Pointer) error

type extControls struct {
	Class, Count, ErrorIndex uint32
	Reserved                 [2]uint32
	Padding                  uint32
	Controls                 unsafe.Pointer
}

// v4l2_ext_control is packed: its int64 union begins at byte 12, not 16.
// A normal Go struct containing that union would silently produce a 24-byte
// stride instead of the required 20 bytes on arm64.
type packedControl [20]byte

func matchEncoderKernel(system, release, machine string) error {
	// Both revisions have MFC tree ee4d34a7c94c8b7cbcb3d30570152b91df8b60e6
	// and identical V4L2 UAPI, videobuf2, ION and arm64 headers. Other kernels
	// remain rejected; runtime format/control/queue checks are still mandatory.
	if system == "Linux" && machine == "aarch64" {
		for _, prefix := range []string{"3.18.140-ge41817ea9198", "3.18.140-g481bdb278a10"} {
			if release == prefix || strings.HasPrefix(release, prefix+"-") {
				return nil
			}
		}
	}
	return fmt.Errorf("MFC encoder requires verified e41817ea9198/481bdb278a10 3.18.140/aarch64 ABI; found %s/%s/%s", system, release, machine)
}
func requireEncoderKernel() error {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return err
	}
	text := func(a []int8) string {
		out := make([]byte, 0, len(a))
		for _, c := range a {
			if c == 0 {
				break
			}
			out = append(out, byte(c))
		}
		return string(out)
	}
	return matchEncoderKernel(text(u.Sysname[:]), text(u.Release[:]), text(u.Machine[:]))
}

func encoderParameters(c EncodeSettings) ([]Control, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return []Control{
		{ID: cidBFrames, Value: 0},
		{ID: cidFrameRC, Value: 1},
		// e418 allocates zeroed encoder parameters; advertised controls are not
		// initialized automatically. In particular QP bounds must not stay 0/0.
		{ID: cidMBRC, Value: 1},
		{ID: cidH264Entropy, Value: 1}, // CABAC, encoded by MFC
		{ID: cidH264Transform8x8, Value: 1},
		{ID: cidH264ReferenceP, Value: 1},
		{ID: cidH264IQP, Value: 26},
		{ID: cidH264PQP, Value: 26},
		{ID: cidH264BQP, Value: 26},
		{ID: cidH264MinIQP, Value: 10}, {ID: cidH264MaxIQP, Value: 51},
		{ID: cidH264MinPQP, Value: 10}, {ID: cidH264MaxPQP, Value: 51},
		{ID: cidH264MinBQP, Value: 10}, {ID: cidH264MaxBQP, Value: 51},
		{ID: cidBitrate, Value: int32(c.Bitrate)},
		{ID: cidGOP, Value: int32(c.GOP)},
		{ID: cidClosedGOP, Value: 1},
		{ID: cidH264Profile, Value: 4}, // V4L2 H264 High
		{ID: cidH264Level, Value: c.Level()},
		{ID: cidMFCFrameRate, Value: int32(c.FPS)},
		{ID: cidHeaderMode, Value: 0}, // separate sequence header accepted by packetizer
		{ID: cidMFCPrependHeaders, Value: 1},
		{ID: cidMFCVUI, Value: 1},
	}, nil
}

func queryEncoderValue(call encoderIOCTL, c Control) error {
	q := QueryCtrl{ID: c.ID}
	if err := call(QueryControl, unsafe.Pointer(&q)); err != nil {
		return fmt.Errorf("query MFC control %#x: %w", c.ID, err)
	}
	delta := int64(c.Value) - int64(q.Minimum)
	if q.ID != c.ID || (q.Type != 1 && q.Type != 2) || q.Flags&1 != 0 ||
		q.Minimum > q.Maximum || c.Value < q.Minimum || c.Value > q.Maximum ||
		q.Step < 0 || (q.Step > 1 && delta%int64(q.Step) != 0) {
		return fmt.Errorf("MFC control %#x rejects %d (range %d..%d step %d flags %#x)", c.ID, c.Value, q.Minimum, q.Maximum, q.Step, q.Flags)
	}
	return nil
}

func configureEncoder(call encoderIOCTL, c EncodeSettings) error {
	controls, err := encoderParameters(c)
	if err != nil {
		return err
	}
	if call == nil || unsafe.Sizeof(extControls{}) != 32 || unsafe.Offsetof(extControls{}.Controls) != 24 {
		return fmt.Errorf("invalid MFC extended-controls ABI")
	}
	// Reject missing capabilities before the first write. A failed batch is not
	// retried on this instance: its owner closes the unstarted encoder instead.
	for _, c := range controls {
		if err := queryEncoderValue(call, c); err != nil {
			return err
		}
	}
	packed := make([]packedControl, len(controls))
	for i, c := range controls {
		le.PutUint32(packed[i][0:4], c.ID)
		le.PutUint32(packed[i][12:16], uint32(c.Value))
	}
	ext := extControls{Class: mpegControlClass, Count: uint32(len(controls)), Controls: unsafe.Pointer(&packed[0])}
	err = call(setExtControls, unsafe.Pointer(&ext))
	runtime.KeepAlive(packed)
	if err != nil {
		if int(ext.ErrorIndex) < len(controls) {
			return fmt.Errorf("MFC parameter %#x (batch index %d): %w", controls[ext.ErrorIndex].ID, ext.ErrorIndex, err)
		}
		return fmt.Errorf("MFC parameter batch: %w", err)
	}
	for i, c := range controls {
		if le.Uint32(packed[i][0:4]) != c.ID || int32(le.Uint32(packed[i][12:16])) != c.Value {
			return fmt.Errorf("MFC changed parameter %#x during submission", c.ID)
		}
	}
	// This means the kernel accepted the configuration, NOT that the sensor or
	// encoder delivered the requested rate. Actual SPS and frame PTS are checked
	// downstream; there is deliberately no fictitious G_CTRL/S_PARM readback.
	return nil
}

func forceEncoderKey(call encoderIOCTL) error {
	c := Control{ID: cidMFCForceFrame, Value: 1} // next I picture; require actual NAL 5 before resuming USB
	if err := queryEncoderValue(call, c); err != nil {
		return err
	}
	if err := call(SetControl, unsafe.Pointer(&c)); err != nil {
		return fmt.Errorf("MFC force I frame: %w", err)
	}
	return nil
}

//go:build linux && (amd64 || arm64)

package appliance

import (
	"errors"
	"perimode/native/internal/linuxio"
	"time"
	"unsafe"
)

const localPowerHold uint16 = 0xf116
const powerHoldTime = 2 * time.Second

// VolumeInput is an observation of physical gpio-keys, never a remote command.
// A zero event is not all-up: only EVIOCGKEY can establish initial/recovered state.
type VolumeInput struct {
	Code        uint16
	Value       int32
	Lost, AllUp bool
}

type physicalKeys struct {
	gpio, ready, boundary bool
	down                  map[uint16]bool
	powerDownAt           time.Time
	powerHoldSent         bool
}

func newPhysicalKeys(gpio bool) *physicalKeys {
	return &physicalKeys{gpio: gpio, boundary: true, down: map[uint16]bool{}}
}
func (k *physicalKeys) allowed(code uint16) bool {
	if k.gpio {
		return code == 114 || code == 115 || code == 116 || code == 172
	}
	return code == 158 || code == 254
}
func (k *physicalKeys) resync(bits []byte) bool {
	if !k.boundary || len(bits) < 32 {
		return false
	}
	for _, code := range []uint16{114, 115, 116, 172, 158, 254} {
		if k.allowed(code) && bits[code/8]&(1<<(code%8)) != 0 {
			return false
		}
	}
	clear(k.down)
	k.powerDownAt = time.Time{}
	k.powerHoldSent = false
	k.ready = true
	return true
}

// feed returns release-triggered local controls and edge-triggered media keys.
func (k *physicalKeys) feed(e Event) (uint16, *VolumeInput) {
	return k.feedAt(e, time.Now())
}

func (k *physicalKeys) feedAt(e Event, now time.Time) (uint16, *VolumeInput) {
	if e.Type == 0 && e.Code == 3 {
		k.ready, k.boundary = false, false
		clear(k.down)
		k.powerDownAt = time.Time{}
		k.powerHoldSent = false
		if k.gpio {
			return 0, &VolumeInput{Lost: true}
		}
		return 0, nil
	}
	if !k.ready {
		if e.Type == 0 && e.Code == 0 {
			k.boundary = true
		}
		return 0, nil
	}
	if e.Type != 1 || !k.allowed(e.Code) || (e.Value != 0 && e.Value != 1) {
		return 0, nil
	}
	if e.Value == 1 {
		if k.down[e.Code] {
			return 0, nil
		}
		k.down[e.Code] = true
		if k.gpio && e.Code == 116 {
			k.powerDownAt, k.powerHoldSent = now, false
		}
		if e.Code == 114 || e.Code == 115 {
			return 0, &VolumeInput{Code: e.Code, Value: 1}
		}
		if !k.gpio {
			return e.Code, nil
		}
		return 0, nil
	}
	if !k.down[e.Code] {
		return 0, nil
	}
	delete(k.down, e.Code)
	if k.gpio && e.Code == 116 {
		k.powerDownAt = time.Time{}
		if k.powerHoldSent {
			return 0, nil
		}
	}
	if !k.gpio {
		return 0, nil
	}
	if e.Code == 114 || e.Code == 115 {
		return 0, &VolumeInput{Code: e.Code, Value: 0}
	}
	return e.Code, nil
}

func (k *physicalKeys) powerHoldDue(now time.Time) bool {
	if !k.gpio || !k.ready || !k.down[116] || k.powerHoldSent || k.powerDownAt.IsZero() || now.Sub(k.powerDownAt) < powerHoldTime {
		return false
	}
	k.powerHoldSent = true
	return !k.down[114]
}
func readPhysicalKeys(fd int) ([]byte, error) {
	// Linux KEY_MAX=0x2ff: 96 bytes; EVIOCGKEY(sizeof(bitmap)).
	var bits [96]byte
	if e := linuxio.Ioctl(fd, 0x80604518, unsafe.Pointer(&bits[0])); e != nil {
		return nil, errors.Join(errors.New("EVIOCGKEY failed; physical key actions disabled"), e)
	}
	return bits[:], nil
}

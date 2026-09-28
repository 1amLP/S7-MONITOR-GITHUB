package appliance

import (
	"encoding/binary"
	"fmt"
	"time"
)

const endpointMask = uint32(0x7f)
const packageVerifiedFlag = uint32(1 << 31)

func (s *State) EndpointSnapshot() EndpointStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.Endpoints
	v.Requested = s.endpointFlagsLocked()
	v.PackageRelease = s.PackageRelease
	if v.Updated.IsZero() || time.Since(v.Updated) > 10*time.Second {
		v.Known, v.Applied = 0, 0
		v.PackageVerified = false
	}
	// IDD owns monitor arrival/departure and acknowledges it on its own channel.
	if s.Ack == s.Generation && s.HostError == 0 && s.HostState < 3 && recentLink(time.Now(), s.LastPoll, linkHeartbeatLimit) {
		v.Known |= 1
		v.Applied = (v.Applied &^ 1) | (v.Requested & 1)
	}
	return v
}

type EndpointStatus struct {
	PackageRelease  uint32    `json:"package_release"`
	PackageVerified bool      `json:"package_verified"`
	Requested       uint32    `json:"requested"`
	Known           uint32    `json:"known"`
	Applied         uint32    `json:"applied"`
	Error           uint32    `json:"windows_error"`
	Updated         time.Time `json:"updated"`
}

func (s *State) endpointFlagsLocked() uint32 {
	var flags uint32
	for i, on := range []bool{s.Settings.Enabled, s.Camera.Enabled, s.SpeakerEnabled, s.MicrophoneEnabled, s.TouchKind == 1, s.TouchKind == 2} {
		if on {
			flags |= 1 << i
		}
	}
	// Older PC services must still read the package release and auto-update
	// before the new independently mapped HID endpoint flag is exposed.
	if s.Settings.Enabled && s.Settings.SniperEnabled && s.TouchKind != 0 && (s.PackageRelease == 0 || s.Endpoints.PackageVerified) {
		flags |= 64
	}
	return flags
}

// Separate from monitor Configuration: this read does not renew its heartbeat.
func (s *State) EndpointConfiguration() [16]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b [16]byte
	copy(b[:], "S7E1")
	binary.LittleEndian.PutUint16(b[4:], 1)
	if s.PackageRelease != 0 {
		binary.LittleEndian.PutUint16(b[4:], 2)
	}
	binary.LittleEndian.PutUint16(b[6:], 16)
	binary.LittleEndian.PutUint32(b[8:], s.endpointFlagsLocked())
	binary.LittleEndian.PutUint32(b[12:], s.PackageRelease)
	return b
}

func (s *State) EndpointAcknowledgement(b []byte) error {
	if len(b) != 16 {
		return fmt.Errorf("endpoint acknowledgement size")
	}
	le := binary.LittleEndian
	wanted, known, applied, code := le.Uint32(b), le.Uint32(b[4:]), le.Uint32(b[8:]), le.Uint32(b[12:])
	verified := known&packageVerifiedFlag != 0
	known &^= packageVerifiedFlag
	if (wanted|known|applied)&^endpointMask != 0 || applied&^known != 0 {
		return fmt.Errorf("endpoint acknowledgement flags")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if wanted != s.endpointFlagsLocked() {
		return nil
	}
	if verified && s.PackageRelease == 0 {
		return fmt.Errorf("package acknowledgement without a delivery")
	}
	s.Endpoints = EndpointStatus{PackageRelease: s.PackageRelease, PackageVerified: verified, Requested: wanted, Known: known, Applied: applied, Error: code, Updated: time.Now()}
	return nil
}

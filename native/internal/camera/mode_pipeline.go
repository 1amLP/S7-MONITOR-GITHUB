package camera

import (
	"fmt"
	"time"

	"perimode/native/internal/media"
)

// modeContract binds a single native capture/encode/USB session. It holds PTS
// values only, never sensor DMA pointers or an unbounded queue of full frames.
type modeContract struct {
	settings                   media.EncodeSettings
	pending                    [16]int64
	pendingAt                  [16]time.Time
	count                      int
	firstCapture, firstEncoded int64
}

func NewModePipeline(settings Settings, source Source, encoder Encoder, sink Sink, now time.Time) (*Pipeline, error) {
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	p, err := NewPipeline(source, encoder, sink, now)
	if err != nil {
		return nil, err
	}
	p.contract = &modeContract{settings: settings.Encoder()}
	return p, nil
}
func (m *modeContract) validateImage(im media.Image) error {
	w, h := int(m.settings.Width), int(m.settings.Height)
	if im.Lease != nil {
		if im.Lease.Live() && im.Width == w && im.Height == h && im.Lease.Width == w && im.Lease.Height == h && im.Lease.PTS == im.PTS {
			return nil
		}
		return fmt.Errorf("camera DMA lease does not match committed mode")
	}
	if im.Width != w || im.Height != h || im.StrideY < w || im.StrideUV < w || im.StrideY > 16384 || im.StrideUV > 16384 ||
		len(im.Y) < (h-1)*im.StrideY+w || len(im.UV) < (h/2-1)*im.StrideUV+w {
		return fmt.Errorf("camera native frame does not match committed %dx%d", w, h)
	}
	return nil
}

// The queue already has a 3s progress guard. Track every submitted timestamp
// separately so a stuck frame has a stage-specific error instead of a fake USB
// diagnosis. This never compares sensor PTS to local monotonic time.
func (m *modeContract) checkResidence(now time.Time) error {
	if m.count == 0 {
		return nil
	}
	age := now.Sub(m.pendingAt[0])
	if age < 0 {
		return fmt.Errorf("camera encoder residence clock regressed")
	}
	if age > 3*time.Second {
		return fmt.Errorf("camera MFC retained submitted PTS %d for more than 3 seconds", m.pending[0])
	}
	return nil
}
func (m *modeContract) completeAt(pts int64, now time.Time) (uint64, uint64, error) {
	for i := 0; i < m.count; i++ {
		if m.pending[i] == pts {
			age := now.Sub(m.pendingAt[i])
			if age < 0 {
				return 0, 0, fmt.Errorf("camera encoder completion clock regressed")
			}
			copy(m.pending[:], m.pending[i+1:m.count])
			copy(m.pendingAt[:], m.pendingAt[i+1:m.count])
			old := m.count
			m.count -= i + 1
			clear(m.pending[m.count:old])
			clear(m.pendingAt[m.count:old])
			return uint64(i), uint64(age / time.Microsecond), nil
		}
	}
	return 0, 0, fmt.Errorf("encoded camera PTS %d was never successfully submitted in this session", pts)
}

// No payload copy: the real encoder's assembler already owns the access unit.
// This boundary check still rejects a mislabeled encoder or incompatible sink.
func (m *modeContract) validatePacket(p media.Encoded) error {
	nals, err := media.SplitAnnexB(p.Data)
	if err != nil {
		return err
	}
	hasSPS, hasPPS, hasVCL, key := false, false, false, false
	for _, n := range nals {
		switch n[0] & 31 {
		case 7:
			info, err := media.InspectSPS(n)
			if err != nil {
				return err
			}
			if err = info.CheckMode(m.settings); err != nil {
				return err
			}
			hasSPS = true
		case 8:
			hasPPS = true
		case 1, 5:
			hasVCL = true
			key = key || n[0]&31 == 5
		}
	}
	if !hasVCL || key != p.Key || (key && (!hasSPS || !hasPPS)) {
		return fmt.Errorf("invalid camera frame/key/header contract")
	}
	return nil
}
func measuredRateMilli(count uint64, first, last int64) uint64 {
	// The camera/encoder PTS are microseconds. Never manufacture FPS by repeating
	// pictures. A short sample remains unknown; this is NOT Windows presentation.
	if count < 2 || last-first < 1_000_000 || first < 0 || last <= first {
		return 0
	}
	return (count - 1) * 1_000_000_000 / uint64(last-first)
}

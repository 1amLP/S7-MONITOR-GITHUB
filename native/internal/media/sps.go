package media

import (
	"fmt"
	"perimode/native/pkg/monitor"
)

// Minimal bounded H.264 SPS parser. Rejects unsupported syntax instead of
// passing arbitrary geometry/bit depth to the unaccepted vendor driver.
type bits struct {
	p   []byte
	n   int
	err error
}

func (b *bits) read(n int) uint32 {
	if n < 0 || n > 32 || b.n+n > len(b.p)*8 {
		b.err = fmt.Errorf("truncated H264 bits")
		return 0
	}
	v := uint32(0)
	for i := 0; i < n; i++ {
		v = v<<1 | uint32((b.p[b.n/8]>>(7-b.n%8))&1)
		b.n++
	}
	return v
}
func (b *bits) ue() uint32 {
	n := 0
	for b.err == nil && b.read(1) == 0 {
		n++
		if n > 24 {
			b.err = fmt.Errorf("excessive H264 Exp-Golomb")
			return 0
		}
	}
	if b.err != nil {
		return 0
	}
	return (1 << n) - 1 + b.read(n)
}
func (b *bits) se() int32 {
	v := b.ue()
	if v&1 != 0 {
		return int32((v + 1) / 2)
	}
	return -int32(v / 2)
}
func (rb *bits) skipScaling(n int) {
	last, next := int32(8), int32(8)
	for j := 0; j < n && rb.err == nil; j++ {
		if next != 0 {
			next = (last + rb.se() + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}
func NALs(p []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(p); {
		n := 0
		if i+3 < len(p) && p[i] == 0 && p[i+1] == 0 && p[i+2] == 0 && p[i+3] == 1 {
			n = 4
		} else if p[i] == 0 && p[i+1] == 0 && p[i+2] == 1 {
			n = 3
		}
		if n > 0 {
			if start >= 0 && i > start {
				out = append(out, p[start:i])
			}
			start = i + n
			i += n
		} else {
			i++
		}
	}
	if start >= 0 && start < len(p) {
		out = append(out, p[start:])
	}
	return out
}
func RBSP(n []byte) ([]byte, error) {
	if len(n) > 1<<20 {
		return nil, fmt.Errorf("NAL too large")
	}
	return rbspPrefix(n, len(n))
}

// Slice identity needs only its first three Exp-Golomb fields, not a decoded
// copy of a multi-megabyte picture. Lookahead still uses the complete NAL.
func rbspPrefix(n []byte, limit int) ([]byte, error) {
	out := make([]byte, 0, min(len(n), limit))
	zeros := 0
	for i, v := range n {
		if len(out) == limit {
			break
		}
		if zeros == 2 && v == 3 {
			if i+1 >= len(n) || n[i+1] > 3 {
				return nil, fmt.Errorf("invalid emulation prevention")
			}
			zeros = 0
			continue
		}
		out = append(out, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out, nil
}

// SPSInfo describes visible progressive 8-bit 4:2:0 geometry and the VUI
// timing prefix. Timing is signaling, not proof of actual capture frame rate.
// HRD/bitstream-restriction fields following timing are not interpreted here.
type SPSInfo struct {
	Width, Height                 int
	ID                            uint32
	TimingPresent, FixedFrameRate bool
	NumUnitsInTick, TimeScale     uint32
}

func (s SPSInfo) CheckMode(c EncodeSettings) error {
	if s.Width != int(c.Width) || s.Height != int(c.Height) {
		return fmt.Errorf("SPS visible %dx%d != requested %dx%d", s.Width, s.Height, c.Width, c.Height)
	}
	if s.TimingPresent && uint64(s.TimeScale) != 2*uint64(s.NumUnitsInTick)*uint64(c.FPS) {
		return fmt.Errorf("SPS nominal rate %d/(2*%d) != requested %d FPS", s.TimeScale, s.NumUnitsInTick, c.FPS)
	}
	return nil
}
func SPSDimensions(n []byte) (w, h int, err error) {
	info, err := InspectSPS(n)
	return info.Width, info.Height, err
}
func InspectSPS(n []byte) (SPSInfo, error) {
	if len(n) < 5 || n[0]&0x80 != 0 || n[0]&31 != 7 {
		return SPSInfo{}, fmt.Errorf("not SPS")
	}
	p, e := RBSP(n[1:])
	if e != nil {
		return SPSInfo{}, e
	}
	b := bits{p: p}
	profile := b.read(8)
	b.read(8)
	b.read(8)
	id := b.ue()
	if id > 31 {
		return SPSInfo{}, fmt.Errorf("SPS id")
	}
	chroma := uint32(1)
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		chroma = b.ue()
		if chroma != 1 {
			return SPSInfo{}, fmt.Errorf("native decoder accepts only 4:2:0")
		}
		if b.ue() != 0 || b.ue() != 0 {
			return SPSInfo{}, fmt.Errorf("native decoder accepts only 8-bit")
		}
		b.read(1)
		if b.read(1) != 0 {
			for i := 0; i < 8; i++ {
				if b.read(1) != 0 {
					n := 16
					if i >= 6 {
						n = 64
					}
					b.skipScaling(n)
				}
			}
		}
	case 66, 77, 88:
	default:
		return SPSInfo{}, fmt.Errorf("unsupported AVC profile %d", profile)
	}
	if b.ue() > 12 {
		return SPSInfo{}, fmt.Errorf("frame_num range")
	}
	poc := b.ue()
	if poc == 0 {
		if b.ue() > 12 {
			return SPSInfo{}, fmt.Errorf("POC range")
		}
	} else if poc == 1 {
		b.read(1)
		b.se()
		b.se()
		n := b.ue()
		if n > 255 {
			return SPSInfo{}, fmt.Errorf("POC cycle")
		}
		for i := uint32(0); i < n; i++ {
			b.se()
		}
	} else if poc != 2 {
		return SPSInfo{}, fmt.Errorf("POC type")
	}
	if b.ue() > 16 {
		return SPSInfo{}, fmt.Errorf("reference frame limit")
	}
	b.read(1)
	mw, mh := uint64(b.ue())+1, uint64(b.ue())+1
	if mw > 512 || mh > 512 {
		return SPSInfo{}, fmt.Errorf("oversized SPS")
	}
	progressive := b.read(1)
	if progressive == 0 {
		return SPSInfo{}, fmt.Errorf("interlaced video not supported")
	}
	b.read(1)
	left, right, top, bottom := uint32(0), uint32(0), uint32(0), uint32(0)
	if b.read(1) != 0 {
		left, right, top, bottom = b.ue(), b.ue(), b.ue(), b.ue()
	}
	if b.err != nil {
		return SPSInfo{}, b.err
	}
	cx, cy := uint64(left)+uint64(right), uint64(top)+uint64(bottom)
	if cx >= mw*8 || cy >= mh*8 {
		return SPSInfo{}, fmt.Errorf("invalid cropping")
	}
	info := SPSInfo{Width: int(mw*16 - cx*2), Height: int(mh*16 - cy*2), ID: id}
	if b.read(1) != 0 { // vui_parameters_present_flag
		if b.read(1) != 0 && b.read(8) == 255 {
			b.read(16)
			b.read(16)
		}
		if b.read(1) != 0 {
			b.read(1)
		} // overscan
		if b.read(1) != 0 { // video signal type
			b.read(3)
			b.read(1)
			if b.read(1) != 0 {
				b.read(8)
				b.read(8)
				b.read(8)
			}
		}
		if b.read(1) != 0 {
			b.ue()
			b.ue()
		} // chroma location
		if b.read(1) != 0 {
			info.NumUnitsInTick, info.TimeScale = b.read(32), b.read(32)
			info.FixedFrameRate = b.read(1) != 0
			info.TimingPresent = true
			if info.NumUnitsInTick == 0 || info.TimeScale == 0 {
				return SPSInfo{}, fmt.Errorf("invalid zero SPS timing")
			}
		}
	}
	if b.err != nil {
		return SPSInfo{}, b.err
	}
	return info, nil
}
func Validate720pKey(p []byte) error {
	w, h, err := MonitorKeyDimensions(p)
	if err == nil && (w != 1280 || h != 720) {
		return fmt.Errorf("expected 720p SPS")
	}
	return err
}
func MonitorKeyDimensions(p []byte) (width, height uint32, err error) {
	if err := monitor.ValidateAnnexB(p, true); err != nil {
		return 0, 0, err
	}
	found := false
	for _, n := range NALs(p) {
		if len(n) > 0 && n[0]&31 == 7 {
			w, h, e := SPSDimensions(n)
			if e != nil {
				return 0, 0, e
			}
			if !monitor.ValidMode(uint32(w), uint32(h)) || found && (uint32(w) != width || uint32(h) != height) {
				return 0, 0, fmt.Errorf("unsupported/conflicting monitor SPS %dx%d", w, h)
			}
			width, height = uint32(w), uint32(h)
			found = true
		}
	}
	if !found {
		return 0, 0, fmt.Errorf("missing SPS")
	}
	return width, height, nil
}

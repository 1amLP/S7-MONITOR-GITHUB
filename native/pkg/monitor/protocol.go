// Package monitor carries framed H.264, not DisplayLink pixels.
package monitor

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	HeaderSize    = 48
	ConfigSize    = 48
	MaxFrame      = 1024 * 1024
	Address       = "127.0.0.1:39570"
	GetConfig     = 0x51
	SetHostStatus = 0x52
)

var le = binary.LittleEndian

type Settings struct {
	SniperEnabled  bool   `json:"sniper_enabled,omitempty"`
	SniperStretch  bool   `json:"sniper_stretch,omitempty"`
	SniperZoom     uint16 `json:"sniper_zoom,omitempty"`
	SniperX        int16  `json:"sniper_x,omitempty"`
	SniperY        int16  `json:"sniper_y,omitempty"`
	SniperScaleX   uint16 `json:"sniper_scale_x,omitempty"`
	SniperScaleY   uint16 `json:"sniper_scale_y,omitempty"`
	SniperRotation uint16 `json:"sniper_rotation_tenths,omitempty"`
	SniperMirror   bool   `json:"sniper_mirror,omitempty"`
	Width          uint32 `json:"width,omitempty"`
	Height         uint32 `json:"height,omitempty"`
	Enabled        bool   `json:"enabled"`
	FPS            uint32 `json:"fps"`
	Bitrate        uint32 `json:"bitrate"`
	GOPSeconds     uint32 `json:"gop_seconds"`
}

func DefaultSettings() Settings {
	return Settings{Enabled: true, FPS: 60, Bitrate: 20_000_000, GOPSeconds: 1}
}
func ValidMode(w, h uint32) bool { return w == 1280 && h == 720 || w == 2560 && h == 1440 }
func dimensions(w, h uint32) (uint32, uint32) {
	if w == 0 && h == 0 {
		return 1280, 720
	}
	return w, h
}
func (s Settings) Dimensions() (uint32, uint32) { return dimensions(s.Width, s.Height) }
func (s Settings) Zoom() int {
	if s.SniperZoom == 0 {
		return 100
	}
	return int(s.SniperZoom)
}
func (s Settings) ScaleX() int { return sniperScale(s.SniperScaleX) }
func (s Settings) ScaleY() int { return sniperScale(s.SniperScaleY) }
func sniperScale(value uint16) int {
	if value == 0 {
		return 100
	}
	return int(value)
}
func (s Settings) Validate() error {
	if s.SniperRotation >= 3600 {
		return errors.New("sniper rotation must be 0..359.9 degrees")
	}
	if s.ScaleX() < 25 || s.ScaleX() > 400 || s.ScaleY() < 25 || s.ScaleY() > 400 {
		return errors.New("sniper axis scale must be 25..400 percent")
	}
	if s.SniperX < -5000 || s.SniperX > 5000 || s.SniperY < -5000 || s.SniperY > 5000 {
		return errors.New("sniper position outside display")
	}
	if s.Zoom() < 100 || s.Zoom() > 1600 {
		return errors.New("sniper zoom must be 100..1600 percent")
	}
	if w, h := s.Dimensions(); !ValidMode(w, h) {
		return errors.New("monitor mode must be 720p60 or 1440p60")
	}
	if s.GOPSeconds != 1 && s.GOPSeconds != 2 && s.GOPSeconds != 5 {
		return errors.New("monitor GOP must be 1, 2 or 5 seconds")
	}
	if s.FPS != 60 {
		return errors.New("monitor rate is fixed at 60 Hz")
	}
	if s.Bitrate < 2_000_000 || s.Bitrate > 30_000_000 {
		return errors.New("monitor bitrate must be 2..30 Mbit/s")
	}
	return nil
}

type Frame struct {
	Width, Height   uint32
	FPS, Generation uint32
	Sequence, PTS   uint64
	Key             bool
	Payload         []byte
}

func (f Frame) Dimensions() (uint32, uint32) { return dimensions(f.Width, f.Height) }

func (f Frame) Header() [HeaderSize]byte {
	var b [HeaderSize]byte
	copy(b[:], "S7M1")
	le.PutUint16(b[4:], 2)
	if f.Key {
		le.PutUint16(b[6:], 1)
	}
	w, h := f.Dimensions()
	le.PutUint32(b[8:], w)
	le.PutUint32(b[12:], h)
	le.PutUint32(b[16:], uint32(len(f.Payload)))
	le.PutUint32(b[20:], f.FPS)
	le.PutUint64(b[24:], f.Sequence)
	le.PutUint64(b[32:], f.PTS)
	le.PutUint32(b[40:], f.Generation)
	return b
}
func ParseHeader(b []byte) (Frame, int, error) {
	var f Frame
	if len(b) != HeaderSize || string(b[:4]) != "S7M1" || le.Uint16(b[4:]) != 2 {
		return f, 0, errors.New("invalid S7 monitor v2 header")
	}
	flags, n := le.Uint16(b[6:]), le.Uint32(b[16:])
	f = Frame{FPS: le.Uint32(b[20:]), Sequence: le.Uint64(b[24:]), PTS: le.Uint64(b[32:]), Generation: le.Uint32(b[40:]), Key: flags == 1}
	f.Width, f.Height = le.Uint32(b[8:]), le.Uint32(b[12:])
	if flags > 1 || !ValidMode(f.Width, f.Height) || n < 4 || n > MaxFrame || f.FPS != 60 || f.Sequence == 0 || f.Sequence > 1<<63-1 || f.PTS > 1<<63-1 || f.Generation == 0 || le.Uint32(b[44:]) != 0 {
		return Frame{}, 0, errors.New("invalid monitor flags, mode, length or generation")
	}
	return f, int(n), nil
}

// Validate each complete NAL header. Syntax decoding remains MediaCodec's job.
func ValidateAnnexB(p []byte, key bool) error {
	if len(p) < 4 || len(p) > MaxFrame {
		return errors.New("invalid AVC access-unit size")
	}
	found, idr, vcl, sps, pps := false, false, false, false, false
	for i := 0; i+2 < len(p); i++ {
		if p[i] != 0 || p[i+1] != 0 || p[i+2] != 1 {
			continue
		}
		start := i
		if i > 0 && p[i-1] == 0 {
			start--
		}
		if !found {
			for _, v := range p[:start] {
				if v != 0 {
					return errors.New("AVC data before start code")
				}
			}
		}
		if i+3 >= len(p) {
			return errors.New("empty final AVC NAL")
		}
		v := p[i+3]
		typ := v & 31
		if v&128 != 0 || typ == 0 || typ >= 24 {
			return fmt.Errorf("invalid AVC NAL header %02x", v)
		}
		found = true
		idr = idr || typ == 5
		vcl = vcl || typ == 1 || typ == 5
		sps = sps || typ == 7
		pps = pps || typ == 8
		i += 2
	}
	if !found || key != idr || !vcl || (key && (!sps || !pps)) {
		return errors.New("AVC IDR/VCL/SPS/PPS contract")
	}
	return nil
}

type Framer struct {
	header  [HeaderSize]byte
	used    int
	seeking bool
	frame   Frame
	size    int
	consume func(Frame) error
	invalid func(error)
}

func NewFramer(consume func(Frame) error, invalid ...func(error)) *Framer {
	p := &Framer{consume: consume}
	if len(invalid) > 1 {
		panic("multiple invalid-frame handlers")
	}
	if len(invalid) == 1 {
		p.invalid = invalid[0]
	}
	return p
}
func (p *Framer) reset() {
	p.used = 0
	p.seeking = false
	p.frame = Frame{}
	p.size = 0
}
func (p *Framer) Feed(b []byte) error {
	for len(b) > 0 {
		if p.used < HeaderSize {
			n := copy(p.header[p.used:], b)
			p.used += n
			b = b[n:]
			if p.used < HeaderSize {
				continue
			}
			var err error
			p.frame, p.size, err = ParseHeader(p.header[:])
			if err != nil {
				if p.invalid == nil {
					return err
				}
				if !p.seeking {
					p.invalid(err)
				}
				// A cancelled/truncated USB frame can leave its tail before the next
				// header. Keep the receiver alive and scan one byte at a time until a
				// fully validated v2 header is found, including across Read calls.
				copy(p.header[:HeaderSize-1], p.header[1:])
				p.used = HeaderSize - 1
				p.seeking = true
				continue
			}
			p.seeking = false
			p.frame.Payload = make([]byte, 0, p.size)
		}
		n := min(len(b), p.size-len(p.frame.Payload))
		p.frame.Payload = append(p.frame.Payload, b[:n]...)
		b = b[n:]
		if len(p.frame.Payload) == p.size {
			if err := ValidateAnnexB(p.frame.Payload, p.frame.Key); err != nil {
				p.reset()
				if p.invalid == nil {
					return err
				}
				p.invalid(err)
				continue
			}
			if err := p.consume(p.frame); err != nil {
				if p.invalid == nil {
					return err
				}
				p.invalid(err)
			}
			p.reset()
		}
	}
	return nil
}
func ClientConfiguration(b []byte) (Settings, uint32, error) {
	if len(b) != ConfigSize || string(b[:4]) != "S7C1" || le.Uint16(b[4:]) != 2 || le.Uint16(b[6:]) > 1 || le.Uint32(b[8:]) != 0 || !ValidMode(le.Uint32(b[12:]), le.Uint32(b[16:])) {
		return Settings{}, 0, errors.New("invalid monitor client configuration")
	}
	for _, v := range b[32:44] {
		if v != 0 {
			return Settings{}, 0, errors.New("reserved client configuration fields")
		}
	}
	s := Settings{Enabled: le.Uint16(b[6:]) == 1, FPS: le.Uint32(b[20:]), Bitrate: le.Uint32(b[24:]), GOPSeconds: le.Uint32(b[44:])}
	if le.Uint32(b[12:]) != 1280 {
		s.Width, s.Height = le.Uint32(b[12:]), le.Uint32(b[16:])
	}
	return s, le.Uint32(b[28:]), s.Validate()
}

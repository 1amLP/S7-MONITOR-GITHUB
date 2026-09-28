package audio

import (
	"encoding/binary"
	"fmt"
	"math"
)

// ClockMatcher bridges the two hardware clock domains with bounded +/-1000 ppm
// linear interpolation. It never changes the advertised USB rate or skips a
// whole 10ms block. The FIFO is bounded; reset after every underrun/disconnect.
type ClockMatcher struct {
	samples  []int16
	position float64
	Channels int
}

func (c *ClockMatcher) Reset() { c.samples = nil; c.position = 0 }
func RateCorrection(delay, target int64) (float64, error) {
	if target < 240 || target > 8192 || delay < -8192 || delay > 65536 {
		return 0, fmt.Errorf("implausible playback delay")
	}
	errorFrames := float64(delay - target)
	// High queue: produce slightly fewer frames. Low queue: slightly more frames.
	return math.Max(-1000, math.Min(1000, -errorFrames*2)), nil
}
func (c *ClockMatcher) Process(data []byte, ppm float64) ([]byte, error) {
	return c.ProcessInto(nil, data, ppm)
}

// ProcessInto reuses the output block while retaining only the one interpolation
// frame required for continuity between calls.
func (c *ClockMatcher) ProcessInto(dst, data []byte, ppm float64) ([]byte, error) {
	if c.Channels < 1 || c.Channels > 2 || len(data) == 0 || len(data)%(c.Channels*2) != 0 || len(data) > 8192*c.Channels*2 || math.IsNaN(ppm) || math.IsInf(ppm, 0) || ppm < -1000 || ppm > 1000 {
		return nil, fmt.Errorf("invalid audio clock-matching input")
	}
	for i := 0; i < len(data); i += 2 {
		c.samples = append(c.samples, int16(binary.LittleEndian.Uint16(data[i:])))
	}
	frames := len(c.samples) / c.Channels
	if frames > 8194 {
		c.Reset()
		return nil, fmt.Errorf("audio clock-matcher FIFO limit")
	}
	step := 1 / (1 + ppm/1_000_000)
	needed := (frames + 2) * c.Channels * 2
	if cap(dst) < needed {
		dst = make([]byte, 0, needed)
	} else {
		dst = dst[:0]
	}
	for c.position+1 < float64(frames) {
		a := int(c.position)
		fraction := c.position - float64(a)
		for ch := 0; ch < c.Channels; ch++ {
			x, y := float64(c.samples[a*c.Channels+ch]), float64(c.samples[(a+1)*c.Channels+ch])
			v := int16(math.Round(x + (y-x)*fraction))
			dst = append(dst, byte(v), byte(uint16(v)>>8))
		}
		c.position += step
	}
	drop := int(c.position)
	if drop > frames-1 {
		drop = frames - 1
	}
	copy(c.samples, c.samples[drop*c.Channels:])
	c.samples = c.samples[:len(c.samples)-drop*c.Channels]
	c.position -= float64(drop)
	return dst, nil
}

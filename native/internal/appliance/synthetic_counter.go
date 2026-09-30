package appliance

import (
	"fmt"
	"perimode/native/internal/media"
	"time"
)

const syntheticCounterMagic uint32 = 0x504d5337

type syntheticFrameCounts struct {
	samples, unique, repeats, missing, invalid uint64
	first, last                                time.Time
	previous                                   uint16
	have                                       bool
}

func (c *syntheticFrameCounts) add(value uint16, valid bool, now time.Time) {
	if !valid {
		c.invalid++
		return
	}
	if !c.have {
		c.have = true
		c.first = now
		c.unique++
	} else {
		step := uint16(value - c.previous)
		if step >= 32768 {
			c.invalid++
			return
		}
		if step == 0 {
			c.repeats++
		} else {
			c.unique++
			c.missing += uint64(step - 1)
		}
	}
	c.samples++
	c.previous = value
	c.last = now
}
func (c syntheticFrameCounts) snapshot() map[string]any {
	seconds := c.last.Sub(c.first).Seconds()
	var fps any
	if c.unique > 1 && seconds > 0 {
		fps = float64(c.unique-1) / seconds
	}
	return map[string]any{"samples": c.samples, "unique": c.unique, "repeats": c.repeats, "missing": c.missing, "invalid": c.invalid, "seconds": seconds, "unique_fps": fps}
}

type syntheticCounterProbe struct {
	deadline           time.Time
	generation         uint32
	decoded, submitted syntheticFrameCounts
}

func syntheticFrameTag(im media.Image) (uint16, bool) {
	if im.Width < 1280 || im.Height < 23 || im.StrideY < im.Width || im.StrideY > 16384 || len(im.Y) < 22*im.StrideY+1267 {
		return 0, false
	}
	var counter, magic uint32
	for bit := 0; bit < 64; bit++ {
		y := im.Y[22*im.StrideY+bit*20+6]
		if y > 70 && y < 110 {
			return 0, false
		}
		if y >= 110 {
			if bit < 32 {
				counter |= 1 << bit
			} else {
				magic |= 1 << (bit - 32)
			}
		}
	}
	if magic != syntheticCounterMagic || uint16(counter)^uint16(counter>>16) != 0xffff {
		return 0, false
	}
	return uint16(counter), true
}
func (p *syntheticCounterProbe) observe(im media.Image, gen uint32, now time.Time, submitted bool) bool {
	if p.deadline.IsZero() || !now.Before(p.deadline) || p.generation != gen {
		return false
	}
	tag, valid := syntheticFrameTag(im)
	if submitted {
		p.submitted.add(tag, valid, now)
	} else {
		p.decoded.add(tag, valid, now)
	}
	return true
}
func (s *State) startSyntheticCounter(seconds int, now time.Time) error {
	if seconds < 0 || seconds > 30 {
		return fmt.Errorf("synthetic counter duration outside 0..30 seconds")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if seconds == 0 {
		s.syntheticCounterOn.Store(false)
		return nil
	}
	if !s.Settings.Enabled || !s.Consumer || s.ThermalPaused {
		return fmt.Errorf("monitor stream is not active")
	}
	s.syntheticCounter = syntheticCounterProbe{deadline: now.Add(time.Duration(seconds) * time.Second), generation: s.Generation}
	s.syntheticCounterOn.Store(true)
	return nil
}
func (s *State) observeSyntheticDecoded(im media.Image, gen uint32) {
	if !s.syntheticCounterOn.Load() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.syntheticCounter.observe(im, gen, time.Now(), false) {
		s.syntheticCounterOn.Store(false)
	}
}
func (s *State) SyntheticCounterSnapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !time.Now().Before(s.syntheticCounter.deadline) || s.syntheticCounter.generation != s.Generation {
		s.syntheticCounterOn.Store(false)
	}
	return map[string]any{"active": s.syntheticCounterOn.Load(), "deadline": s.syntheticCounter.deadline,
		"scope":   "test-pattern counters only; DECON submissions, not physical panel scanout",
		"decoded": s.syntheticCounter.decoded.snapshot(), "submitted": s.syntheticCounter.submitted.snapshot(), "images_saved": false}
}

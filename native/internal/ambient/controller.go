package ambient

import (
	"fmt"
	"math"
	"time"
)

type Settings struct {
	Automatic bool `json:"automatic"`
	Minimum   int  `json:"minimum_percent"`
	Maximum   int  `json:"maximum_percent"`
	Bias      int  `json:"bias_percent"`
}

func DefaultSettings() Settings { return Settings{Minimum: 5, Maximum: 100} }
func (s Settings) Validate() error {
	if s.Minimum < 5 || s.Maximum > 100 || s.Minimum > s.Maximum || s.Bias < -30 || s.Bias > 30 {
		return fmt.Errorf("auto brightness requires 5..100 range, min <= max, bias -30..30")
	}
	return nil
}

// Normal 8-bit brightness percentages, not nits/HBM indices. This initial curve
// is a software policy and must be evaluated against the real panel separately.
var curve = [...][2]float64{{0, 8}, {5, 12}, {25, 20}, {100, 32}, {300, 45}, {1000, 62}, {5000, 82}, {20000, 100}}

func Target(lux float64, s Settings) (int, error) {
	if e := s.Validate(); e != nil {
		return 0, e
	}
	if math.IsNaN(lux) || math.IsInf(lux, 0) || lux < 0 || lux > MaxLux {
		return 0, fmt.Errorf("invalid ambient lux")
	}
	v := 100.0
	for i := 1; i < len(curve); i++ {
		a, b := curve[i-1], curve[i]
		if lux <= b[0] {
			t := (math.Log1p(lux) - math.Log1p(a[0])) / (math.Log1p(b[0]) - math.Log1p(a[0]))
			v = a[1] + t*(b[1]-a[1])
			break
		}
	}
	return max(s.Minimum, min(s.Maximum, int(math.Round(v))+s.Bias)), nil
}

type Controller struct {
	settings       Settings
	last           Sample
	first          time.Time
	count          int
	filtered       float64
	committed      int
	lastStep       time.Time
	candidate      int
	candidateSince time.Time
}

func NewController(s Settings, actual int, now time.Time) (*Controller, error) {
	if e := s.Validate(); e != nil {
		return nil, e
	}
	if actual < 5 || actual > 100 || now.IsZero() {
		return nil, fmt.Errorf("invalid current panel level/time")
	}
	return &Controller{settings: s, committed: max(5, actual), lastStep: now}, nil
}
func (c *Controller) ResetSamples() {
	c.last = Sample{}
	c.first = time.Time{}
	c.count = 0
	c.candidateSince = time.Time{}
}
func (c *Controller) Accept(s Sample, now time.Time) error {
	if s.Lux < 0 || s.Lux > MaxLux || s.Timestamp == 0 || s.Timestamp > 1<<63-1 || s.Received.IsZero() || now.Before(s.Received) || now.Sub(s.Received) > StaleAfter {
		return fmt.Errorf("invalid/stale ALS sample")
	}
	if c.last.Timestamp != 0 && (s.Timestamp <= c.last.Timestamp || !s.Received.After(c.last.Received)) {
		return fmt.Errorf("replayed/reordered ALS sample")
	}
	if !c.last.Received.IsZero() && s.Received.Sub(c.last.Received) > StaleAfter {
		c.ResetSamples()
	}
	if c.count == 0 {
		c.filtered = float64(s.Lux)
		c.first = s.Received
	} else {
		tau := 2.0
		if float64(s.Lux) > c.filtered {
			tau = 0.5
		}
		a := 1 - math.Exp(-s.Received.Sub(c.last.Received).Seconds()/tau)
		c.filtered += a * (float64(s.Lux) - c.filtered)
	}
	c.last = s
	c.count++
	return nil
}
func (c *Controller) Fresh(now time.Time) bool {
	return !c.last.Received.IsZero() && !now.Before(c.last.Received) && now.Sub(c.last.Received) <= StaleAfter
}
func (c *Controller) Last() Sample         { return c.last }
func (c *Controller) FilteredLux() float64 { return c.filtered }

// Next proposes a write. Commit is separate: a failed sysfs write never becomes
// the new current level, and no automatic values are persisted as manual levels.
func (c *Controller) Next(now time.Time, fallback int) (target int, write bool, status string) {
	if !c.settings.Automatic {
		return c.committed, false, "MANUAL / SENSOR OFF"
	}
	desired := c.committed
	status = "WAITING FOR FRESH ALS / HOLD MANUAL"
	if c.Fresh(now) {
		if c.count < 3 || c.last.Received.Sub(c.first) < 400*time.Millisecond {
			return c.committed, false, "ALS WARMUP / HOLD MANUAL"
		}
		desired, _ = Target(c.filtered, c.settings)
		status = "AUTOMATIC / FRESH LUX"
		diff := desired - c.committed
		if abs(diff) < 3 {
			c.candidateSince = time.Time{}
			return c.committed, false, status
		}
		direction := 1
		if diff < 0 {
			direction = -1
		}
		if c.candidate != direction || c.candidateSince.IsZero() {
			c.candidate = direction
			c.candidateSince = now
			return c.committed, false, "ALS SETTLING"
		}
		settle := 400 * time.Millisecond
		if direction < 0 {
			settle = 1200 * time.Millisecond
		}
		if now.Sub(c.candidateSince) < settle {
			return c.committed, false, "ALS SETTLING"
		}
	} else {
		c.candidateSince = time.Time{}
		desired = max(5, min(100, fallback))
		status = "ALS UNAVAILABLE / RESTORE MANUAL"
	}
	if now.Before(c.lastStep) || now.Sub(c.lastStep) < 250*time.Millisecond {
		return c.committed, false, status
	}
	delta := desired - c.committed
	if delta == 0 {
		return c.committed, false, status
	}
	elapsed := math.Min(now.Sub(c.lastStep).Seconds(), 0.5)
	rate := 20.0
	if delta < 0 {
		rate = 8
	}
	step := max(1, int(elapsed*rate))
	target = c.committed + max(-step, min(step, delta))
	return max(5, min(100, target)), true, status
}
func (c *Controller) Commit(actual int, now time.Time) error {
	if actual < 5 || actual > 100 {
		return fmt.Errorf("auto brightness readback outside normal range")
	}
	c.committed = actual
	c.lastStep = now
	return nil
}
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

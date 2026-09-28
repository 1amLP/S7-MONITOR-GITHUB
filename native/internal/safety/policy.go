// Package safety contains conservative workload guards, not replacements for
// the kernel/PMIC thermal and charging protections.
package safety

import (
	"fmt"
	"time"
)

type Sample struct {
	CPU, Battery int64
	At           time.Time
	Valid        bool
}
type Action uint8

const (
	Stop Action = iota
	Run
	Pause
)

func (a Action) String() string {
	switch a {
	case Run:
		return "RUN"
	case Pause:
		return "COOLING"
	default:
		return "STOP"
	}
}

type Decision struct {
	Action Action
	Reason string
}
type Guard struct {
	paused    bool
	coolSince time.Time
}

// Hard cutoffs do not exceed the preceding image's conservative 71/45 C limits.
// Soft pause: 65/42 C. Resume below 60/39 C for five continuous seconds.
func (g *Guard) Evaluate(now time.Time, s Sample) Decision {
	if !s.Valid || s.At.IsZero() || s.At.After(now) || now.Sub(s.At) > 2*time.Second || s.CPU < 5000 || s.CPU > 200000 || s.Battery < 0 || s.Battery > 80000 {
		return Decision{Stop, "missing, stale or implausible thermal sensors"}
	}
	if s.CPU >= 71000 || s.Battery >= 45000 {
		return Decision{Stop, fmt.Sprintf("thermal shutdown: CPU %d mC, battery %d mC", s.CPU, s.Battery)}
	}
	if s.CPU >= 65000 || s.Battery >= 42000 {
		g.paused = true
		g.coolSince = time.Time{}
		return Decision{Pause, "thermal workload pause"}
	}
	if g.paused {
		if s.CPU < 60000 && s.Battery < 39000 {
			if g.coolSince.IsZero() {
				g.coolSince = now
			}
			if now.Sub(g.coolSince) >= 5*time.Second {
				g.paused = false
				return Decision{Run, "cooled"}
			}
		} else {
			g.coolSince = time.Time{}
		}
		return Decision{Pause, "waiting for thermal hysteresis"}
	}
	return Decision{Run, "thermal sample within workload limits"}
}

// RetryBudget allows a bounded number of attempts in a sliding interval.
// Use a new instance only for an explicit user retry, not on every bad frame.
type RetryBudget struct {
	times   []time.Time
	Maximum int
	Window  time.Duration
}

func (b *RetryBudget) Next(now time.Time) (time.Duration, bool) {
	if b.Maximum < 1 || b.Maximum > 8 || b.Window <= 0 {
		return 0, false
	}
	n := 0
	for _, t := range b.times {
		if now.Sub(t) < b.Window && !t.After(now) {
			b.times[n] = t
			n++
		}
	}
	b.times = b.times[:n]
	if n >= b.Maximum {
		return 0, false
	}
	b.times = append(b.times, now)
	return time.Duration(1<<uint(n)) * 250 * time.Millisecond, true
}

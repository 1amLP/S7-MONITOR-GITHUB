package appliance

import (
	"sort"
	"time"
)

const metricWindow = 128

// Metrics use the phone's monotonic clock only. A PC timestamp is a frame key,
// not a comparable clock. These numbers do NOT claim cable end-to-end latency
// or panel scan-out/presentation.
type frameTiming struct {
	valid               bool
	generation          uint32
	pts                 uint64
	received, submitted time.Time
}
type durationWindow struct {
	values      [metricWindow]int64
	count, next int
	total       uint64
}
type DurationSummary struct {
	Samples uint64 `json:"total_samples"`
	Window  int    `json:"rolling_samples"`
	MeanUS  int64  `json:"mean_us"`
	P95US   int64  `json:"p95_us"`
	MaxUS   int64  `json:"max_us"`
}

func (w *durationWindow) add(d time.Duration) {
	if d < 0 || d > time.Minute {
		return
	}
	w.values[w.next] = d.Microseconds()
	w.next = (w.next + 1) % metricWindow
	if w.count < metricWindow {
		w.count++
	}
	w.total++
}
func (w *durationWindow) summary() DurationSummary {
	r := DurationSummary{Samples: w.total, Window: w.count}
	if w.count == 0 {
		return r
	}
	v := make([]int64, w.count)
	copy(v, w.values[:w.count])
	var sum int64
	for _, x := range v {
		sum += x
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	r.MeanUS = sum / int64(w.count)
	r.P95US = v[(w.count*95+99)/100-1]
	r.MaxUS = v[len(v)-1]
	return r
}

type MonitorMetrics struct {
	frames                                          [metricWindow]frameTiming
	next                                            int
	generation                                      uint32
	queueHighWater                                  int
	overwritten, unmatched, coalesced               uint64
	receiveSubmit, receiveBlit, submitBlit, cpuBlit durationWindow
}

func (m *MonitorMetrics) received(gen uint32, pts uint64, now time.Time, depth int) {
	if gen != m.generation {
		m.frames = [metricWindow]frameTiming{}
		m.next = 0
		m.generation = gen
	}
	if m.frames[m.next].valid {
		m.overwritten++
	}
	m.frames[m.next] = frameTiming{valid: true, generation: gen, pts: pts, received: now}
	m.next = (m.next + 1) % metricWindow
	if depth > m.queueHighWater {
		m.queueHighWater = depth
	}
}
func (m *MonitorMetrics) submitted(gen uint32, pts uint64, at time.Time) {
	for i := range m.frames {
		f := &m.frames[i]
		if f.valid && f.generation == gen && f.pts == pts {
			if at.Before(f.received) {
				return
			}
			f.submitted = at
			m.receiveSubmit.add(at.Sub(f.received))
			return
		}
	}
}
func (m *MonitorMetrics) blitted(gen uint32, pts uint64, before, after time.Time) {
	if after.Before(before) {
		return
	}
	m.cpuBlit.add(after.Sub(before))
	for i := range m.frames {
		f := &m.frames[i]
		if f.valid && f.generation == gen && f.pts == pts {
			f.valid = false
			if !after.Before(f.received) {
				m.receiveBlit.add(after.Sub(f.received))
			}
			if !f.submitted.IsZero() && !after.Before(f.submitted) {
				m.submitBlit.add(after.Sub(f.submitted))
			}
			return
		}
	}
	m.unmatched++
}
func (m *MonitorMetrics) snapshot() map[string]any {
	return map[string]any{
		"clock": "phone monotonic only; NOT host-to-panel latency", "window_capacity": metricWindow,
		"received_to_submit": m.receiveSubmit.summary(), "received_to_cpu_blit_done": m.receiveBlit.summary(), "submit_to_cpu_blit_done": m.submitBlit.summary(), "cpu_blit_duration": m.cpuBlit.summary(),
		"compressed_queue_high_water": m.queueHighWater, "decoded_not_presented_as_obsolete": m.coalesced, "timing_entries_overwritten": m.overwritten, "presented_timing_not_found": m.unmatched,
	}
}
func (s *State) markSubmitted(gen uint32, pts uint64, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics.submitted(gen, pts, at)
}
func (s *State) markBlitted(gen uint32, pts int64, before, after time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pts >= 0 {
		s.metrics.blitted(gen, uint64(pts), before, after)
	}
}
func (s *State) markCoalesced(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics.coalesced += uint64(n)
}

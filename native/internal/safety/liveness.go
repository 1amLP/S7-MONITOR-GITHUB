package safety

import (
	"sync/atomic"
	"time"
)

type Liveness struct {
	born                   time.Time
	ui, thermal, operation atomic.Int64
	failure                atomic.Pointer[HeartbeatStatus]
}

type HeartbeatStatus struct {
	Healthy        bool   `json:"healthy"`
	Reason         string `json:"reason,omitempty"`
	ElapsedMS      int64  `json:"elapsed_ms"`
	UIAgeMS        int64  `json:"ui_age_ms"`
	ThermalAgeMS   int64  `json:"thermal_age_ms"`
	OperationAgeMS int64  `json:"operation_age_ms"`
}

func NewLiveness() *Liveness { return &Liveness{born: time.Now()} }
func (l *Liveness) UI()      { l.ui.Store(time.Since(l.born).Nanoseconds() + 1) }
func (l *Liveness) Thermal() { l.thermal.Store(time.Since(l.born).Nanoseconds() + 1) }
func (l *Liveness) Healthy() bool {
	return l.Check().Healthy
}
func (l *Liveness) Check() HeartbeatStatus {
	// Load each heartbeat before sampling time. A producer may refresh it
	// concurrently; loading after "now" could falsely classify a fresh beat as
	// a future timestamp and stop watchdog feeding on a healthy system.
	op := l.operation.Load()
	ui := l.ui.Load()
	thermal := l.thermal.Load()
	now := time.Since(l.born).Nanoseconds() + 1
	age := func(last int64) int64 {
		if last == 0 {
			return -1
		}
		return (now - last) / int64(time.Millisecond)
	}
	s := HeartbeatStatus{Healthy: true, ElapsedMS: now / int64(time.Millisecond), UIAgeMS: age(ui), ThermalAgeMS: age(thermal), OperationAgeMS: age(op)}
	if op > 0 && now-op > int64(10*time.Second) {
		s.Healthy = false
		s.Reason = "control operation stale"
	} else if !FreshHeartbeat(now, ui, int64(3*time.Second), int64(15*time.Second)) {
		s.Healthy = false
		s.Reason = "UI heartbeat stale"
	} else if !FreshHeartbeat(now, thermal, int64(2*time.Second), int64(15*time.Second)) {
		s.Healthy = false
		s.Reason = "thermal heartbeat stale"
	}
	return s
}
func (l *Liveness) RecordFailure(s HeartbeatStatus) {
	if !s.Healthy {
		l.failure.CompareAndSwap(nil, &s)
	}
}
func (l *Liveness) Failure() *HeartbeatStatus {
	if p := l.failure.Load(); p != nil {
		copy := *p
		return &copy
	}
	return nil
}
func FreshHeartbeat(now, last, limit, startup int64) bool {
	if now < 0 || last > now || limit <= 0 {
		return false
	}
	if last == 0 {
		return now <= startup
	}
	return now-last <= limit
}

func (l *Liveness) BeginOperation() { l.operation.Store(time.Since(l.born).Nanoseconds() + 1) }
func (l *Liveness) EndOperation()   { l.operation.Store(0) }

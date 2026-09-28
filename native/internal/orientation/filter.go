package orientation

import (
	"encoding/binary"
	"fmt"
	"time"
)

// SSP calibrated accelerometer ABI from the supplied sensors.sensorhub.so:
// three LE int16 values (4096 counts/g), then an unaligned LE uint64 timestamp.
// It is a packed 14-byte IIO record, NOT struct input_event or generic IIO scans.
const SSPRecordSize = 14
const StaleAfter = 750 * time.Millisecond

type Sample struct {
	X, Y, Z   int32
	Timestamp uint64
	Received  time.Time
}

func ParseSSP(b []byte, now time.Time) (Sample, error) {
	if len(b) != SSPRecordSize {
		return Sample{}, fmt.Errorf("SSP accelerometer: expected 14-byte record, got %d", len(b))
	}
	s := Sample{X: int32(int16(binary.LittleEndian.Uint16(b))), Y: int32(int16(binary.LittleEndian.Uint16(b[2:]))), Z: int32(int16(binary.LittleEndian.Uint16(b[4:]))), Timestamp: binary.LittleEndian.Uint64(b[6:]), Received: now}
	if s.Timestamp == 0 || s.Timestamp > uint64(1<<63-1) {
		return Sample{}, fmt.Errorf("SSP accelerometer: invalid timestamp")
	}
	return s, nil
}

type Filter struct {
	last      Sample
	candidate Degrees
	since     time.Time
	count     int
}

func (f *Filter) LastReceived() time.Time { return f.last.Received }
func (f *Filter) Reset()                  { *f = Filter{} }
func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
func (f *Filter) Fresh(now time.Time) bool {
	return !f.last.Received.IsZero() && !now.Before(f.last.Received) && now.Sub(f.last.Received) <= StaleAfter
}

// Consider rejects shock/freefall, ambiguous diagonals, a flat phone and replayed
// timestamps. Six samples and 350ms of stable orientation are required.
func (f *Filter) Consider(s Sample, now time.Time) (Degrees, bool, string) {
	invalid := func(reason string) (Degrees, bool, string) {
		f.since = time.Time{}
		f.count = 0
		return 0, false, reason
	}
	if s.Received.IsZero() || now.Before(s.Received) || now.Sub(s.Received) > StaleAfter {
		return invalid("STALE SAMPLE / HOLD")
	}
	if f.last.Timestamp != 0 && (s.Timestamp <= f.last.Timestamp || !s.Received.After(f.last.Received)) {
		return invalid("REPEATED OR REORDERED SAMPLE / HOLD")
	}
	if !f.last.Received.IsZero() && s.Received.Sub(f.last.Received) > 200*time.Millisecond {
		f.since = time.Time{}
		f.count = 0
	}
	f.last = s
	x, y, z := int64(s.X), int64(s.Y), int64(s.Z)
	// 0.75g..1.25g; all arithmetic is int64, including malformed int32 input.
	if abs(x) > 32768 || abs(y) > 32768 || abs(z) > 32768 {
		return invalid("OUT OF RANGE / HOLD")
	}
	length2 := x*x + y*y + z*z
	if length2 < 3072*3072 || length2 > 5120*5120 {
		return invalid("MOTION / HOLD")
	}
	if abs(z) > 3547 {
		return invalid("FLAT / HOLD")
	}
	ax, ay := abs(x), abs(y)
	major, minor := ax, ay
	d := Degrees(90)
	if x < 0 {
		d = 270
	}
	if ay >= ax {
		major, minor = ay, ax
		d = 0
		if y < 0 {
			d = 180
		}
	}
	if major < 2048 || major*4 < minor*5 {
		return invalid("BETWEEN ORIENTATIONS / HOLD")
	}
	if f.since.IsZero() || d != f.candidate {
		f.candidate = d
		f.since = s.Received
		f.count = 1
		return d, false, "SETTLING"
	}
	f.count++
	if f.count < 6 || s.Received.Sub(f.since) < 350*time.Millisecond {
		return d, false, "SETTLING"
	}
	return d, true, "FRESH SSP DATA"
}

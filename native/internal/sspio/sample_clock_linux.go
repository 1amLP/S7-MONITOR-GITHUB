//go:build linux && (amd64 || arm64)

package sspio

import (
	"errors"
	"fmt"
	"math"
	"syscall"
	"time"
	"unsafe"
)

// Native SSP requires Android sensor event BOOTTIME timestamps in nanoseconds.
// A different epoch is NOT fitted to the first sample: that would make an old
// FIFO look fresh. Pinned kernel get_current_timestamp and posix_get_boottime
// both use ktime_get_with_offset(1); report_iio_data forwards the sensor timestamp.
// Actual MCU synchronization/received samples still need device validation.
// The packed record layout is unchanged; this code only maps its clock domain.
type ClockPoint struct {
	BootNS uint64
	Host   time.Time
}

type ClockReader func() (ClockPoint, error)

// ReadSampleClock deliberately uses the kernel syscall instead of libc/Android.
// Host is taken first: mapping a sample back to time.Time is conservative by the
// duration of the syscall. Clock 7 includes suspend; Go's monotonic clock does not.
func ReadSampleClock() (ClockPoint, error) {
	host := time.Now()
	var ts syscall.Timespec
	_, _, errno := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 7, uintptr(unsafe.Pointer(&ts)), 0)
	if errno != 0 {
		return ClockPoint{}, fmt.Errorf("SSP CLOCK_BOOTTIME: %w", errno)
	}
	if ts.Sec < 0 || ts.Nsec < 0 || ts.Nsec >= 1_000_000_000 ||
		uint64(ts.Sec) > (math.MaxInt64-uint64(ts.Nsec))/1_000_000_000 {
		return ClockPoint{}, errors.New("invalid SSP boot clock")
	}
	return ClockPoint{uint64(ts.Sec)*1_000_000_000 + uint64(ts.Nsec), host}, nil
}

var ErrClockDiscontinuity = errors.New("SSP clock discontinuity; reopen sensor and discard old samples")
var ErrSampleExpired = errors.New("SSP last measurement expired on the boot clock")

// SampleClock is owned by one IIO reader. It never changes the device clock,
// hub state or timestamps. No shared calibration between accelerometer/ALS.
type SampleClock struct {
	read             ClockReader
	opened, previous ClockPoint
	maxAge           time.Duration
	failed           error
	delivered        uint64
}

const clockStepTolerance = 250 * time.Millisecond

func NewSampleClock(read ClockReader, maxAge time.Duration) (*SampleClock, error) {
	if maxAge <= 0 || maxAge > 2*time.Second {
		return nil, errors.New("invalid SSP sample age limit")
	}
	if read == nil {
		read = ReadSampleClock
	}
	p, err := read()
	if err != nil {
		return nil, err
	}
	if p.Host.IsZero() || p.BootNS == 0 || p.BootNS > math.MaxInt64 {
		return nil, errors.New("invalid SSP initial clock")
	}
	return &SampleClock{read: read, opened: p, previous: p, maxAge: maxAge}, nil
}

// Stamp is called AFTER a bounded drain, including an empty drain. Stale records
// are consumed but not delivered. A suspend/clock reset invalidates even the
// previous sample retained by a brightness/orientation controller. The caller's
// normal error path resets that controller and safely releases its sensor lease.
func (c *SampleClock) Stamp(timestamp uint64, have bool) (time.Time, bool, error) {
	if c == nil || c.read == nil {
		return time.Time{}, false, errors.New("SSP sample clock unavailable")
	}
	if c.failed != nil {
		return time.Time{}, false, c.failed
	}
	fail := func(e error) (time.Time, bool, error) {
		c.failed = e
		return time.Time{}, false, e
	}
	p, err := c.read()
	if err != nil {
		return fail(err)
	}
	if p.Host.IsZero() || p.BootNS > math.MaxInt64 || p.BootNS < c.previous.BootNS || p.Host.Before(c.previous.Host) {
		return fail(ErrClockDiscontinuity)
	}
	bootElapsed := time.Duration(p.BootNS - c.previous.BootNS)
	hostElapsed := p.Host.Sub(c.previous.Host)
	// Either direction indicates an incompatible clock or a suspended reader.
	// Do not keep a 'fresh' pre-sleep lux/rotation merely because Go time paused.
	if (bootElapsed > hostElapsed && bootElapsed-hostElapsed > clockStepTolerance) ||
		(hostElapsed > bootElapsed && hostElapsed-bootElapsed > clockStepTolerance) {
		return fail(ErrClockDiscontinuity)
	}
	c.previous = p
	noFresh := func() (time.Time, bool, error) {
		// A controller must not retain an old sample across a short suspend
		// which is smaller than the discontinuity tolerance.
		if c.delivered != 0 && time.Duration(p.BootNS-c.delivered) > c.maxAge {
			return fail(ErrSampleExpired)
		}
		return time.Time{}, false, nil
	}
	if !have {
		return noFresh()
	}
	if timestamp == 0 || timestamp > math.MaxInt64 || timestamp > p.BootNS {
		return fail(fmt.Errorf("SSP sample timestamp is invalid/future or uses a different clock: sample_ns=%d boot_ns=%d", timestamp, p.BootNS))
	}
	age := time.Duration(p.BootNS - timestamp)
	if timestamp < c.opened.BootNS || age > c.maxAge {
		return noFresh()
	}
	c.delivered = timestamp
	return p.Host.Add(-age), true, nil
}

//go:build linux && (amd64 || arm64)

package orientation

import (
	"fmt"
	"perimode/native/internal/sspio"
	"os"
	"syscall"
	"time"
)

// The shared lease serializes sensor-mask changes with auto brightness. Neither
// channel starts Android/lhd or fabricates data from a cold, uninitialised hub.
type SampleSource interface {
	Poll(time.Time) (Sample, bool, error)
	Close() error
	Name() string
}
type attrIO = sspio.Attributes
type sensorLease = sspio.Lease

func acquireSSP(io attrIO, dir, enable string) (*sensorLease, error) {
	return sspio.Acquire(io, dir, enable, 1, 50_000_000)
}
func discoverSSP(sysRoot string) (string, error) { return sspio.Discover(sysRoot, sspio.Accelerometer) }

type SSP struct {
	file          *os.File
	fd            int
	lease         *sensorLease
	name          string
	closed        bool
	lastTimestamp uint64
	clock         *sspio.SampleClock
}

func (s *SSP) Name() string { return s.name }
func OpenSSP() (SampleSource, error) {
	c, e := sspio.Open(sspio.Accelerometer)
	if c == nil {
		return nil, e
	}
	return &SSP{file: c.File, fd: c.FD, lease: c.Lease, name: c.Label + " / packed14", clock: c.Clock}, e
}
func (s *SSP) Poll(now time.Time) (Sample, bool, error) {
	if s.closed {
		return Sample{}, false, os.ErrClosed
	}
	var latest Sample
	have := false
	// Drain to the newest record. Do not manufacture fresh timestamps while
	// slowly replaying a backlog from an old orientation.
	for i := 0; i < 64; i++ {
		var b [SSPRecordSize]byte
		n, e := syscall.Read(s.fd, b[:])
		if e == syscall.EAGAIN || e == syscall.EWOULDBLOCK {
			if s.clock == nil {
				return Sample{}, false, fmt.Errorf("SSP sample clock missing")
			}
			received, fresh, err := s.clock.Stamp(latest.Timestamp, have)
			if err != nil || !fresh {
				return Sample{}, false, err
			}
			latest.Received = received
			return latest, true, nil
		}
		if e == syscall.EINTR {
			continue
		}
		if e != nil {
			return Sample{}, false, e
		}
		if n == 0 {
			return Sample{}, false, fmt.Errorf("SSP IIO stream closed")
		}
		if n != SSPRecordSize {
			return Sample{}, false, fmt.Errorf("SSP short IIO record: %d", n)
		}
		if b == ([SSPRecordSize]byte{}) {
			continue
		} // flush marker, not a vector
		sample, e := ParseSSP(b[:], now)
		if e != nil {
			return Sample{}, false, e
		}
		if sample.Timestamp <= s.lastTimestamp {
			return Sample{}, false, fmt.Errorf("SSP timestamp duplicate or regression")
		}
		s.lastTimestamp = sample.Timestamp
		latest = sample
		have = true
	}
	return Sample{}, false, fmt.Errorf("SSP excessive buffered records; refusing delayed orientation")
}
func (s *SSP) Close() error {
	s.closed = true
	// Keep both lease and reader until disabling the IIO queue is confirmed.
	if e := s.lease.Close(); e != nil {
		return e
	}
	if s.file != nil {
		f := s.file
		s.file = nil
		return f.Close()
	}
	return nil
}

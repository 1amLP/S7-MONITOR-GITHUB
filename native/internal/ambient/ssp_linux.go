//go:build linux && (amd64 || arm64)

package ambient

import (
	"fmt"
	"perimode/native/internal/sspio"
	"os"
	"syscall"
	"time"
)

type Source interface {
	Poll(time.Time) (Sample, bool, error)
	Name() string
	Close() error
}
type SSP struct {
	channel       *sspio.Channel
	fd            int
	lastTimestamp uint64
	clock         *sspio.SampleClock
	closed        bool
	name          string
}

func OpenSSP() (Source, error) {
	c, e := sspio.Open(sspio.Light)
	if c == nil {
		return nil, e
	}
	return &SSP{channel: c, fd: c.FD, name: c.Label + " / calibrated lux / packed26", clock: c.Clock}, e
}
func (s *SSP) Name() string { return s.name }
func (s *SSP) Close() error {
	s.closed = true
	if s.channel == nil {
		return nil
	}
	return s.channel.Close()
}
func (s *SSP) Poll(now time.Time) (Sample, bool, error) {
	if s.closed {
		return Sample{}, false, os.ErrClosed
	}
	var latest Sample
	have := false
	for i := 0; i < 64; i++ {
		var b [RecordSize]byte
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
		if n != RecordSize {
			return Sample{}, false, fmt.Errorf("SSP light short/closed record: %d", n)
		}
		if b == ([RecordSize]byte{}) {
			continue
		} // Flush, not zero lux. Real darkness has a timestamp.
		v, e := ParseSSP(b[:], now)
		if e != nil {
			return Sample{}, false, e
		}
		if v.Timestamp <= s.lastTimestamp {
			return Sample{}, false, fmt.Errorf("SSP light timestamp replay/regression")
		}
		s.lastTimestamp = v.Timestamp
		latest = v
		have = true
	}
	return Sample{}, false, fmt.Errorf("SSP light backlog exceeded bound; refusing stale data")
}

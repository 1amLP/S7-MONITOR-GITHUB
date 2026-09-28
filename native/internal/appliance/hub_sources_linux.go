//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"perimode/native/internal/ambient"
	"perimode/native/internal/orientation"
	"perimode/native/internal/sensorhub"
	"perimode/native/internal/sspio"
)

func awaitHubIIO(ctx context.Context, lease *sensorhub.Lease, p sspio.Profile, wanted func() bool) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !wanted() {
			return context.Canceled
		}
		if err := lease.Check(); err != nil {
			return err
		}
		_, err := sspio.Discover("/sys", p)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("hub IIO discovery: %v: %w", err, ctx.Err())
		case <-tick.C:
		}
	}
}
func (u *UI) openHubOrientation(ctx context.Context) (orientation.SampleSource, error) {
	if u.hub == nil {
		return orientation.OpenSSP()
	}
	_, revision := u.state.RotationCurrent()
	wanted := func() bool { cfg, rev := u.state.RotationCurrent(); return cfg.Automatic && rev == revision }
	ctx, finish := sensorStartup(ctx, wanted)
	defer finish()
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if e := u.hub.RetryIdleCleanup(ctx); e != nil {
		return nil, e
	}
	lease, e := u.hub.Acquire(ctx)
	if e != nil {
		return nil, e
	}
	if e = awaitHubIIO(ctx, lease, sspio.Accelerometer, wanted); e != nil {
		return nil, errors.Join(e, lease.Close())
	}
	if err := ctx.Err(); err != nil || !wanted() {
		if err == nil {
			err = context.Canceled
		}
		return nil, errors.Join(err, lease.Close())
	}
	source, e := orientation.OpenSSP()
	if source == nil {
		return nil, errors.Join(e, lease.Close())
	}
	wrapped := &hubOrientation{source: source, lease: lease}
	if e != nil {
		if ce := wrapped.Close(); ce != nil {
			return wrapped, errors.Join(e, ce)
		}
		return nil, e
	}
	return wrapped, nil
}
func (u *UI) openHubLight(ctx context.Context) (ambient.Source, error) {
	if u.hub == nil {
		return ambient.OpenSSP()
	}
	a, _ := u.state.ambientCurrent()
	wanted := func() bool {
		cfg, _ := u.state.ambientCurrent()
		return cfg.Settings.Automatic && cfg.Revision == a.Revision
	}
	ctx, finish := sensorStartup(ctx, wanted)
	defer finish()
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if e := u.hub.RetryIdleCleanup(ctx); e != nil {
		return nil, e
	}
	lease, e := u.hub.Acquire(ctx)
	if e != nil {
		return nil, e
	}
	if e = awaitHubIIO(ctx, lease, sspio.Light, wanted); e != nil {
		return nil, errors.Join(e, lease.Close())
	}
	if err := ctx.Err(); err != nil || !wanted() {
		if err == nil {
			err = context.Canceled
		}
		return nil, errors.Join(err, lease.Close())
	}
	source, e := ambient.OpenSSP()
	if source == nil {
		return nil, errors.Join(e, lease.Close())
	}
	wrapped := &hubLight{source: source, lease: lease}
	if e != nil {
		if ce := wrapped.Close(); ce != nil {
			return wrapped, errors.Join(e, ce)
		}
		return nil, e
	}
	return wrapped, nil
}

type hubOrientation struct {
	mu     sync.Mutex
	source orientation.SampleSource
	lease  *sensorhub.Lease
	closed bool
	err    error
}

func (s *hubOrientation) Name() string { return s.source.Name() + " / ISOLATED LHD" }
func (s *hubOrientation) Poll(now time.Time) (orientation.Sample, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return orientation.Sample{}, false, os.ErrClosed
	}
	if err := s.lease.Check(); err != nil {
		return orientation.Sample{}, false, err
	}
	return s.source.Poll(now)
}
func (s *hubOrientation) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if err := s.source.Close(); err != nil {
		s.err = err
		return err
	}
	s.err = s.lease.Close()
	return s.err
}

type hubLight struct {
	mu     sync.Mutex
	source ambient.Source
	lease  *sensorhub.Lease
	closed bool
	err    error
}

func (s *hubLight) Name() string { return s.source.Name() + " / ISOLATED LHD" }
func (s *hubLight) Poll(now time.Time) (ambient.Sample, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ambient.Sample{}, false, os.ErrClosed
	}
	if err := s.lease.Check(); err != nil {
		return ambient.Sample{}, false, err
	}
	return s.source.Poll(now)
}
func (s *hubLight) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if err := s.source.Close(); err != nil {
		s.err = err
		return err
	}
	s.err = s.lease.Close()
	return s.err
}

func (u *UI) hubStatus() string {
	if u.hub == nil {
		return "HUB: NOT ATTACHED"
	}
	s := u.hub.Snapshot()
	return fmt.Sprintf("HUB: %s / USERS %d / STARTS %d", s.Status, s.Consumers, s.Starts)
}

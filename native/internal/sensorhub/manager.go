// Package sensorhub owns the single hardware transport shared by the native
// accelerometer and light readers. A live helper is NOT proof of sensor data.
package sensorhub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const StartupGrace = 12 * time.Second
const stopTimeout = 2 * time.Second

var ErrStopping = errors.New("sensor hub stop not confirmed; restart blocked")

type Process interface {
	Done() <-chan struct{}
	ExitError() error
	Stop(context.Context) error
}
type Starter func(context.Context) (Process, error)
type Snapshot struct {
	Consumers int    `json:"consumers"`
	Starts    uint64 `json:"starts"`
	Running   bool   `json:"running"`
	Blocked   bool   `json:"blocked"`
	Status    string `json:"status"`
}

// gate serializes operations which may enter the kernel; mu protects only
// bookkeeping. Never hold mu across Start/Stop/ExitError: the menu and thermal
// diagnostics must not wait for a failed hardware helper to leave the kernel.
type Manager struct {
	mu          sync.Mutex
	gate        chan struct{}
	start       Starter
	process     Process
	refs        int
	starts      uint64
	closed      bool
	blocked     error
	lastError   string
	phase       string
	startCancel context.CancelFunc
}

func New(start Starter) *Manager {
	m := &Manager{start: start, gate: make(chan struct{}, 1)}
	m.gate <- struct{}{}
	return m
}
func (m *Manager) enter(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.gate:
	}
	if err := ctx.Err(); err != nil {
		m.leave()
		return err
	}
	return nil
}
func (m *Manager) leave() { m.gate <- struct{}{} }
func exited(p Process) bool {
	select {
	case <-p.Done():
		return true
	default:
		return false
	}
}
func exitError(p Process) error {
	if err := p.ExitError(); err != nil {
		return fmt.Errorf("sensor hub helper exited: %w", err)
	}
	return errors.New("sensor hub helper exited without supplying live sensor data")
}

type Lease struct {
	manager   *Manager
	process   Process
	closed    bool       // protected by manager.mu
	closeMu   sync.Mutex // serializes retryable Close without holding manager.mu
	closeDone bool
}

// Acquire does not hold the state lock while hashing files, starting a process,
// or cleaning up a prior helper. Waiting behind another operation is cancellable.
func (m *Manager) Acquire(ctx context.Context) (*Lease, error) {
	if err := m.enter(ctx); err != nil {
		return nil, err
	}
	defer m.leave()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("sensor hub manager closed")
	}
	if m.blocked != nil {
		e := errors.Join(ErrStopping, m.blocked)
		m.mu.Unlock()
		return nil, e
	}
	p := m.process
	if p != nil {
		if exited(p) {
			refs := m.refs
			m.mu.Unlock()
			e := exitError(p)
			m.mu.Lock()
			m.lastError = e.Error()
			m.mu.Unlock()
			if refs == 0 {
				e = errors.Join(e, m.stopProcess())
			}
			return nil, e
		}
		m.refs++
		m.mu.Unlock()
		return &Lease{manager: m, process: p}, nil
	}
	if m.start == nil {
		m.mu.Unlock()
		return nil, errors.New("sensor hub starter unavailable")
	}
	startCtx, cancel := context.WithCancel(ctx)
	m.startCancel = cancel
	m.phase = "STARTING"
	m.mu.Unlock()
	p, err := m.start(startCtx)
	startErr := startCtx.Err()
	cancel()
	m.mu.Lock()
	m.startCancel = nil
	m.phase = ""
	if p != nil {
		m.process = p
		m.starts++
	}
	// Close can mark this manager closed and cancel startup without waiting for
	// gate. A late successful spawn is owned and stopped, never handed to a reader.
	if err == nil && (m.closed || startErr != nil) {
		if startErr != nil {
			err = startErr
		} else {
			err = errors.New("sensor hub closed during startup")
		}
	}
	if err == nil && p == nil {
		err = errors.New("sensor hub starter returned nil process")
	}
	if err != nil {
		m.lastError = err.Error()
		m.mu.Unlock()
		if p != nil {
			err = errors.Join(err, m.stopProcess())
		}
		return nil, err
	}
	m.lastError = ""
	if exited(p) {
		m.mu.Unlock()
		err = exitError(p)
		m.mu.Lock()
		m.lastError = err.Error()
		m.mu.Unlock()
		return nil, errors.Join(err, m.stopProcess())
	}
	m.refs++
	m.mu.Unlock()
	return &Lease{manager: m, process: p}, nil
}
func (l *Lease) Check() error {
	m := l.manager
	m.mu.Lock()
	if l.closed || m.closed {
		m.mu.Unlock()
		return errors.New("sensor hub lease closed")
	}
	if m.blocked != nil {
		e := errors.Join(ErrStopping, m.blocked)
		m.mu.Unlock()
		return e
	}
	if m.process != l.process {
		m.mu.Unlock()
		return errors.New("sensor hub generation changed")
	}
	p := l.process
	m.mu.Unlock()
	if exited(p) {
		return exitError(p)
	}
	return nil
}

// stopProcess requires gate, never mu. Failure retains the exact process so an
// explicit Close can retry cleanup. A nil Stop result without exit is NOT safe.
func (m *Manager) stopProcess() error {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	return m.stopProcessContext(ctx)
}
func (m *Manager) stopProcessContext(ctx context.Context) error {
	m.mu.Lock()
	p := m.process
	if p == nil {
		m.mu.Unlock()
		return nil
	}
	m.phase = "STOPPING"
	m.mu.Unlock()
	err := p.Stop(ctx)
	if err == nil && !exited(p) {
		err = errors.New("helper Stop returned before process exit")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.phase = ""
	if err != nil {
		m.blocked = err
		m.lastError = err.Error()
		return errors.Join(ErrStopping, err)
	}
	m.process = nil
	m.blocked = nil
	return nil
}

// Close releases the reader reference once. A failed final helper cleanup is
// retryable on this same lease: a transient wake_unlock/Stop error must not
// permanently break both auto sensors until reboot. Never stop a newer helper.
func (l *Lease) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	return l.closeContext(ctx)
}
func (l *Lease) closeContext(ctx context.Context) error {
	l.closeMu.Lock()
	defer l.closeMu.Unlock()
	if l.closeDone {
		return nil
	}
	m := l.manager
	m.mu.Lock()
	if !l.closed {
		l.closed = true
		m.refs--
		if m.refs < 0 {
			m.mu.Unlock()
			panic("sensorhub: negative ownership")
		}
	}
	if m.process != l.process || m.refs != 0 {
		l.closeDone = true
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	if err := m.enter(ctx); err != nil {
		// No consumer may reuse the unconfirmed last owner's process. Another
		// gate holder may already have completed cleanup; preserve that outcome.
		m.mu.Lock()
		if m.process == l.process && m.refs == 0 {
			m.blocked = err
			m.lastError = err.Error()
		}
		m.mu.Unlock()
		return errors.Join(ErrStopping, err)
	}
	defer m.leave()
	m.mu.Lock()
	last := m.process == l.process && m.refs == 0
	m.mu.Unlock()
	if last {
		if err := m.stopProcessContext(ctx); err != nil {
			return err
		}
	}
	l.closeDone = true
	return nil
}
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	s := Snapshot{Consumers: m.refs, Starts: m.starts, Blocked: m.blocked != nil}
	p, closed, blocked, phase, last := m.process, m.closed, m.blocked, m.phase, m.lastError
	m.mu.Unlock()
	switch {
	case blocked != nil:
		s.Status = "STOP UNCONFIRMED: " + blocked.Error()
	case phase != "":
		s.Status = phase
	case closed && p != nil:
		s.Status = "CLOSING / HELPER STILL OWNED"
	case closed:
		s.Status = "STOPPED"
	case p == nil && last != "":
		s.Status = "START ERROR: " + last
	case p == nil:
		s.Status = "OFF / ON DEMAND"
	case exited(p):
		s.Status = exitError(p).Error()
	default:
		s.Running = true
		s.Status = "HELPER RUNNING / DATA VERIFIED BY SENSOR READERS"
	}
	return s
}
func (m *Manager) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	return m.CloseContext(ctx)
}
func (m *Manager) CloseContext(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	cancel := m.startCancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err := m.enter(ctx); err != nil {
		return errors.Join(ErrStopping, err)
	}
	defer m.leave()
	m.mu.Lock()
	refs := m.refs
	m.mu.Unlock()
	if refs != 0 {
		// A reader can still own an IIO queue after a failed disable. The
		// manager cannot tear down the shared helper underneath that owner.
		return fmt.Errorf("%w: %d sensor readers have not released ownership", ErrStopping, refs)
	}
	return m.stopProcessContext(ctx)
}

// RetryIdleCleanup is the explicit recovery step of a new sensor-open request.
// It never stops a helper still owned by an IIO reader and never launches one.
// This also recovers a failed cleanup before any IIO source could be returned.
func (m *Manager) RetryIdleCleanup(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, stopTimeout)
	defer cancel()
	if err := m.enter(ctx); err != nil {
		return err
	}
	defer m.leave()
	m.mu.Lock()
	p, refs, closed := m.process, m.refs, m.closed
	m.mu.Unlock()
	if closed {
		return errors.New("sensor hub manager closed")
	}
	if p == nil || refs != 0 {
		return nil
	}
	return m.stopProcessContext(ctx)
}

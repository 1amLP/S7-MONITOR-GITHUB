package appliance

import "context"

type monitorContextKey struct{}

func (s *State) monitorWake() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.monitorChange == nil {
		s.monitorChange = make(chan struct{})
	}
	return s.monitorChange
}

// Called with state.mu. Invalidation is a channel close, never a vendor call
// under the state lock. USB, thermal and input readers remain nonblocking.
func (s *State) invalidateMonitorLocked() {
	if s.monitorChange != nil {
		close(s.monitorChange)
	}
	s.monitorChange = make(chan struct{})
}

// A codec gets exactly one monitor generation and one enable interval. Cancel
// remains sticky across disable/re-enable even without a generation increment.
func (s *State) monitorContext(parent context.Context, generation uint32) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	if s.monitorChange == nil {
		s.monitorChange = make(chan struct{})
	}
	changed := s.monitorChange
	ctx = context.WithValue(ctx, monitorContextKey{}, changed)
	ready := s.Generation == generation && s.Consumer && s.Settings.Enabled && !s.ThermalPaused && s.monitorVisibleLocked()
	s.mu.Unlock()
	if !ready {
		cancel()
		return ctx, cancel
	}
	go func() {
		select {
		case <-changed:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
func (s *State) monitorGenerationCurrent(ctx context.Context, generation uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ctx != nil && ctx.Err() == nil && ctx.Value(monitorContextKey{}) == s.monitorChange &&
		s.Generation == generation && s.Consumer &&
		s.Settings.Enabled && !s.ThermalPaused && s.monitorVisibleLocked()
}

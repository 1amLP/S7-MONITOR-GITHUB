package mediaruntime

import "sync"

// A role is not reusable until Wait and its exit acknowledgement are complete.
// Holding mu during the bounded acknowledgement prevents a reconnect from
// observing the old reservation after it has received a successful S7EX.
type codecRoles struct {
	mu                  sync.Mutex
	control             bool
	active, quarantined [3]bool
}
type codecLease struct {
	registry *codecRoles
	role     byte
	finished bool // registry.mu
}

func (r *codecRoles) acquire(role byte) *codecLease {
	r.mu.Lock()
	defer r.mu.Unlock()
	if role > 2 {
		return nil
	}
	if role == 0 {
		if r.control {
			return nil
		}
		r.control = true
	} else {
		if !r.control || r.active[role] || r.quarantined[role] {
			return nil
		}
		r.active[role] = true
	}
	return &codecLease{registry: r, role: role}
}
func (l *codecLease) finish(clean bool, acknowledge func() error) bool {
	r := l.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.finished {
		return false
	}
	// Always send a negative acknowledgement too. A lost acknowledgement
	// retains quarantine even after a clean worker exit.
	if acknowledge != nil && acknowledge() != nil {
		clean = false
	}
	l.finished = true
	if l.role != 0 {
		r.active[l.role] = false
		if !clean {
			r.quarantined[l.role] = true
		}
	}
	return clean
}
func (l *codecLease) abandon() {
	r := l.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.finished {
		return
	}
	l.finished = true
	if l.role != 0 {
		r.active[l.role] = false
		r.quarantined[l.role] = true
	}
	// Loss of role zero stops the entire namespace; never re-admit control.
}

package appliance

import (
	"context"
	"sync"
)

// The presenter waits for ownership, not a retry timer, and can still stop
// while a GPU/control operation owns the gate. Zero value is ready to use.
type presentationGate struct {
	once  sync.Once
	token chan struct{}
}

func (g *presentationGate) init() {
	g.once.Do(func() { g.token = make(chan struct{}, 1); g.token <- struct{}{} })
}
func (g *presentationGate) Lock() { g.init(); <-g.token }
func (g *presentationGate) Unlock() {
	g.init()
	select {
	case g.token <- struct{}{}:
	default:
		panic("unlock of idle presentation gate")
	}
}
func (g *presentationGate) TryLock() bool {
	g.init()
	select {
	case <-g.token:
		return true
	default:
		return false
	}
}
func (g *presentationGate) LockContext(ctx context.Context) bool {
	g.init()
	select {
	case <-ctx.Done():
		return false
	case <-g.token:
	}
	if ctx.Err() != nil {
		g.Unlock()
		return false
	}
	return true
}

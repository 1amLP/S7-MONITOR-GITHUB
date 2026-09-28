// Package lifecycle tracks the appliance's fixed set of workers. It does not
// restart hardware owners behind a failed DMA/USB cleanup or kill goroutines.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const MaxWorkers = 32

type Worker struct {
	Name     string    `json:"name"`
	State    string    `json:"state"`
	Error    string    `json:"error,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
}
type entry struct {
	state Worker
	done  chan struct{}
}
type Group struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	closed  bool
	entries []*entry
}

func New(parent context.Context) *Group {
	ctx, cancel := context.WithCancel(parent)
	return &Group{ctx: ctx, cancel: cancel}
}

// Start publishes the task before spawning. Registration is bounded and duplicate
// names are refused so missing/joined tasks cannot be silently counted as active.
func (g *Group) Start(name string, fn func(context.Context) error) error {
	if fn == nil || name == "" || len(name) > 64 || strings.ContainsAny(name, "\n\r\t") {
		return errors.New("invalid worker")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.ctx.Err() != nil {
		return errors.New("worker group stopping")
	}
	if len(g.entries) >= MaxWorkers {
		return errors.New("worker registry full")
	}
	for _, e := range g.entries {
		if e.state.Name == name {
			return fmt.Errorf("duplicate worker: %s", name)
		}
	}
	e := &entry{state: Worker{Name: name, State: "RUNNING", Started: time.Now()}, done: make(chan struct{})}
	g.entries = append(g.entries, e)
	go func() {
		err := fn(g.ctx)
		// Unexpected clean exit is visible. Do not blindly respawn a codec or a
		// sensor reader whose hardware ownership may not have been returned.
		if err == nil && g.ctx.Err() == nil {
			err = errors.New("worker exited while appliance remained active")
		}
		g.mu.Lock()
		e.state.Finished = time.Now()
		if err != nil && !(g.ctx.Err() != nil && errors.Is(err, context.Canceled)) {
			e.state.State = "FAILED"
			e.state.Error = err.Error()
			if len(e.state.Error) > 1024 {
				e.state.Error = e.state.Error[:1024]
			}
		} else {
			e.state.State = "STOPPED"
		}
		g.mu.Unlock()
		close(e.done)
	}()
	return nil
}
func (g *Group) Snapshot() []Worker {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Worker, len(g.entries))
	for i, e := range g.entries {
		out[i] = e.state
	}
	return out
}

// Stop cancels all registered tasks and waits for exactly those tasks. On timeout
// the caller must retain their resources; the report names every unfinished task.
// Another Stop may wait again. It never turns an unconfirmed stop into success.
func (g *Group) Stop(ctx context.Context) error {
	g.mu.Lock()
	g.closed = true
	entries := append([]*entry(nil), g.entries...)
	g.mu.Unlock()
	g.cancel()
	for _, e := range entries {
		select {
		case <-e.done:
			continue
		default:
		}
		select {
		case <-e.done:
		case <-ctx.Done():
			pending := []string{}
			for _, p := range entries {
				select {
				case <-p.done:
				default:
					pending = append(pending, p.state.Name)
				}
			}
			return fmt.Errorf("workers did not stop [%s]: %w", strings.Join(pending, ", "), ctx.Err())
		}
	}
	return nil
}

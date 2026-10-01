//go:build linux && (amd64 || arm64)

package appliance

import (
	"fmt"
	"sync"
)

const maxControlCommands = 16
const maxControlSettings = 32
const manualBrightnessControl = "display/brightness"
const autoBrightnessControl = "display/auto-enabled"

type controlOperation struct {
	key string
	fn  func() error
}

// One existing worker owns execution. Named absolute settings replace only an
// older pending value for the same setting; one-shot commands keep their order.
// Separate bounds reserve space for final slider values even during command bursts.
type controlQueue struct {
	mu        sync.Mutex
	items     []controlOperation
	scheduled bool
	active    string
	coalesced uint64
	rejected  uint64
}

func (q *controlQueue) pending(key string) bool {
	if key == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.active == key {
		return true
	}
	for _, item := range q.items {
		if item.key == key {
			return true
		}
	}
	return false
}

func (u *UI) enqueueControl(key string, fn func() error) {
	if u.ops == nil || cap(u.ops) == 0 || fn == nil || len(key) > 96 {
		u.state.Error(fmt.Errorf("control worker unavailable or invalid command"))
		return
	}
	q := &u.controls
	q.mu.Lock()
	commands, settings := 0, 0
	for i, item := range q.items {
		if key != "" && item.key == key {
			copy(q.items[i:], q.items[i+1:])
			q.items[len(q.items)-1] = controlOperation{key, fn}
			q.coalesced++
			q.mu.Unlock()
			u.pumpControls()
			return
		}
		if item.key == "" {
			commands++
		} else {
			settings++
		}
	}
	if (key == "" && commands >= maxControlCommands) || (key != "" && settings >= maxControlSettings) {
		q.rejected++
		q.mu.Unlock()
		u.state.Error(fmt.Errorf("control queue full; wait for the current operation"))
		return
	}
	q.items = append(q.items, controlOperation{key, fn})
	q.mu.Unlock()
	u.pumpControls()
}

func (u *UI) asyncLatest(key string, fn func() error) {
	if key == "" {
		u.state.Error(fmt.Errorf("setting control key missing"))
		return
	}
	u.enqueueControl(key, fn)
}

func (u *UI) pumpControls() {
	q := &u.controls
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.scheduled || len(q.items) == 0 {
		return
	}
	select {
	case u.ops <- u.runQueuedControl:
		q.scheduled = true
	default:
		// A direct USB-link operation owns the channel. Its worker completion
		// calls pumpControls; queued settings remain owned here in the meantime.
	}
}

func (u *UI) runQueuedControl() error {
	q := &u.controls
	q.mu.Lock()
	item := q.items[0]
	copy(q.items, q.items[1:])
	q.items[len(q.items)-1] = controlOperation{}
	q.items = q.items[:len(q.items)-1]
	q.active = item.key
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		q.active, q.scheduled = "", false
		q.mu.Unlock()
		u.pumpControls()
	}()
	return item.fn()
}

func (q *controlQueue) snapshot() map[string]any {
	q.mu.Lock()
	defer q.mu.Unlock()
	return map[string]any{"pending": len(q.items), "scheduled": q.scheduled,
		"active_setting": q.active, "coalesced": q.coalesced, "rejected": q.rejected}
}

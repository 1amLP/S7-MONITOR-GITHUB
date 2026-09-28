package appliance

import (
	"context"
	"sync"
	"sync/atomic"
)

// EP0 must never wait for renderer/vendor locks while Windows polls the link.
// One worker produces bounded immutable snapshots; USB only reads the latest.
type diagnosticCache struct {
	latest    atomic.Pointer[[]byte]
	once      sync.Once
	requested chan struct{}
	revision  atomic.Uint64
}

func (d *diagnosticCache) init() { d.once.Do(func() { d.requested = make(chan struct{}, 1) }) }

func (d *diagnosticCache) read() []byte {
	d.init()
	select {
	case d.requested <- struct{}{}:
	default:
	}
	if b := d.latest.Load(); b != nil {
		return *b
	}
	return []byte(`{"status":"snapshot pending"}`)
}
func (d *diagnosticCache) run(ctx context.Context, snapshot func() []byte) error {
	d.init()
	for ctx.Err() == nil {
		d.revision.Add(1)
		b := snapshot()
		if len(b) > 256<<10 {
			b = []byte(`{"error":"snapshot exceeds limit"}`)
		}
		d.latest.Store(&b)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.requested:
		}
	}
	return ctx.Err()
}

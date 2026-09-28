package appliance

import (
	"context"
	"errors"
	"sync"
	"time"

	"perimode/native/internal/sensorhub"
)

// sensorStartup watches the exact settings revision only while a source is
// opening. Cancelling one request does not cancel another reader's hub lease.
// It covers cleanup, helper exec and IIO discovery with ONE startup budget.
// No goroutine survives the returned finish function.
func sensorStartup(parent context.Context, wanted func() bool) (context.Context, func()) {
	ctx, cancel := context.WithTimeout(parent, sensorhub.StartupGrace)
	done := make(chan struct{})
	if !wanted() {
		cancel()
		close(done)
	} else {
		go func() {
			defer close(done)
			tick := time.NewTicker(25 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if !wanted() {
						cancel()
						return
					}
				}
			}
		}()
	}
	var once sync.Once
	return ctx, func() { once.Do(func() { cancel(); <-done }) }
}

// Only use after the request has become obsolete. A cancellation joined with a
// failed device/helper release is NOT a successful cancellation: keep that error.
func sensorCancelError(err error) error {
	if err == nil || err == context.Canceled {
		return nil
	}
	if group, ok := err.(interface{ Unwrap() []error }); ok {
		var remaining []error
		for _, child := range group.Unwrap() {
			if e := sensorCancelError(child); e != nil {
				remaining = append(remaining, e)
			}
		}
		return errors.Join(remaining...)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok && sensorCancelError(wrapped.Unwrap()) == nil {
		return nil
	}
	return err
}

//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"syscall"
	"time"
)

var errInstallerEpoch = errors.New("installer transfer cancelled by USB reset")

// Retry only a request that the kernel has not accepted. EINTR and any partial
// progress are never replayed. The timer is an idle fallback for nonpollable FFS.
func installerPacket(ctx context.Context, valid func() bool, transfer func([]byte) (int, error), data []byte) (int, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if !valid() {
			return 0, errInstallerEpoch
		}
		n, err := transfer(data)
		if n != 0 || !errors.Is(err, syscall.EAGAIN) {
			return n, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

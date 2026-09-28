//go:build linux

package functionfs

import (
	"errors"
	"syscall"
)

// EIDRM means the host replaced/cancelled a SETUP, not that FunctionFS died.
// os.File wraps these errno values in PathError; direct equality is incorrect.
func RetryEventError(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) ||
		errors.Is(err, syscall.EIDRM) || errors.Is(err, syscall.ESHUTDOWN) || errors.Is(err, syscall.ECONNRESET)
}

//go:build linux && (amd64 || arm64)

package media

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

func waitVideo(fd int, events int16, deadline time.Time) error {
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return os.ErrDeadlineExceeded
		}
		p := struct {
			FD               int32
			Events, Returned int16
		}{FD: int32(fd), Events: events}
		t := syscall.NsecToTimespec(left.Nanoseconds())
		n, _, e := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&p)), 1, uintptr(unsafe.Pointer(&t)), 0, 0, 0)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 {
			return e
		}
		if n == 0 {
			return os.ErrDeadlineExceeded
		}
		if p.Returned&(8|16|32) != 0 {
			return syscall.EIO
		}
		return nil
	}
}

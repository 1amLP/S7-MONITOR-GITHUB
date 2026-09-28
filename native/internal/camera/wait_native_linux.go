//go:build linux && (amd64 || arm64)

package camera

import (
	"context"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

type nativePollFD struct {
	FD               int32
	Events, Returned int16
}

// The owner keeps every fd alive during this bounded event wait. Empty ISP
// output queues report POLLERR; exclude those until the next graph pump.
func waitNativeFrames(ctx context.Context, nodes [5]nativePollFD) error {
	if ctx == nil {
		return fmt.Errorf("camera wait requires context")
	}
	deadline := time.Now().Add(20 * time.Millisecond)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil
		}
		timeout := syscall.NsecToTimespec(left.Nanoseconds())
		n, _, err := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&nodes[0])), uintptr(len(nodes)), uintptr(unsafe.Pointer(&timeout)), 0, 0, 0)
		if err == syscall.EINTR {
			continue
		}
		if err != 0 {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		ready := false
		for i := range nodes {
			p := &nodes[i]
			if p.Returned&(0x10|0x20) != 0 {
				return syscall.ENODEV
			}
			if p.Returned&p.Events != 0 {
				ready = true
			} else if p.Returned&8 != 0 {
				p.FD = -1
			}
			p.Returned = 0
		}
		if ready {
			return nil
		}
	}
}

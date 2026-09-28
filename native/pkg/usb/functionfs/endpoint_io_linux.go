//go:build linux

package functionfs

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// Samsung 3.18 f_fs.c returns EINTR after dequeuing a synchronous USB request,
// even when part of it crossed the bus. os.File's automatic EINTR retry would
// replay an IN prefix or discard an OUT prefix. Mask only Go's preemption signal
// on this OS thread during the syscall; other goroutines remain preemptible.
func endpointIO(file *os.File, data []byte, reading bool) (n int, err error) {
	if len(data) == 0 {
		return 0, nil
	}
	return usbTransfer(file, data, reading, false)
}

// EP0 must reach the kernel even for a zero-length status acknowledgement.
func controlIO(file *os.File, data []byte, reading bool) (int, error) {
	return usbTransfer(file, data, reading, true)
}

func waitBeforeControlProgress(n int, err error) bool {
	return n <= 0 && (err == syscall.EAGAIN || err == syscall.EWOULDBLOCK)
}

func usbTransfer(file *os.File, data []byte, reading, control bool) (n int, err error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	runtime.LockOSThread()
	mask := uint64(1) << (uint(syscall.SIGURG) - 1)
	var previous uint64
	_, _, errno := syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, 0, uintptr(unsafe.Pointer(&mask)), uintptr(unsafe.Pointer(&previous)), 8, 0, 0)
	if errno != 0 {
		runtime.UnlockOSThread()
		return 0, fmt.Errorf("FunctionFS preemption mask: %w", errno)
	}
	defer func() {
		_, _, restore := syscall.RawSyscall6(syscall.SYS_RT_SIGPROCMASK, 2, uintptr(unsafe.Pointer(&previous)), 0, 8, 0, 0)
		runtime.KeepAlive(previous)
		runtime.UnlockOSThread()
		if restore != 0 {
			err = errors.Join(err, fmt.Errorf("restore FunctionFS signal mask: %w", restore))
		}
	}()
	var transferErr error
	transfer := func(fd uintptr) {
		if reading {
			n, transferErr = syscall.Read(int(fd), data)
		} else {
			n, transferErr = syscall.Write(int(fd), data)
		}
	}
	if control {
		ready := func(fd uintptr) bool { transfer(fd); return !waitBeforeControlProgress(n, transferErr) }
		if reading {
			err = raw.Read(ready)
		} else {
			err = raw.Write(ready)
		}
	} else {
		err = raw.Control(transfer)
	}
	runtime.KeepAlive(data)
	if err != nil {
		return max(n, 0), err
	}
	if transferErr == syscall.EINTR {
		// Deliberately do not wrap EINTR: retry predicates must not replay a
		// transfer whose completed prefix the kernel did not report.
		return max(n, 0), fmt.Errorf("FunctionFS %s interrupted with unknown USB progress; no replay", file.Name())
	}
	return max(n, 0), transferErr
}

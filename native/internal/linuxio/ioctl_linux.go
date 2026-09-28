//go:build linux && (amd64 || arm64)

// Package linuxio describes the LP64 Linux UAPI used by the S7 vendor kernel.
package linuxio

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// Ioctl uses a pointer argument. KeepAlive prevents the Go owner being reclaimed
// before the synchronous syscall returns. Callers must never unmap an in-flight buffer.
func Ioctl(fd int, request uintptr, argument unsafe.Pointer) error {
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(argument))
		runtime.KeepAlive(argument)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}
func IoctlValue(fd int, request, value uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, value)
	if errno != 0 {
		return errno
	}
	return nil
}
func CString(b []byte) string {
	for i, v := range b {
		if v == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
func ReadText(p string) (string, error) {
	b, e := os.ReadFile(p)
	if e != nil {
		return "", e
	}
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == 0) {
		b = b[:len(b)-1]
	}
	return string(b), nil
}
func WriteAttr(p, value string) error {
	f, e := os.OpenFile(p, os.O_WRONLY, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	b := []byte(value)
	n, e := f.Write(b)
	if e != nil {
		return e
	}
	if n != len(b) {
		return fmt.Errorf("short write to %s", p)
	}
	return nil
}

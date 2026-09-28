//go:build linux && (amd64 || arm64)

package linuxio

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// NewIONDMABuf allocates uncached system-heap memory in the pinned Samsung ABI.
// The shared descriptor owns the allocation after the client handle is freed.
func NewIONDMABuf(bytes int) (*os.File, error) {
	if bytes <= 0 || bytes > 32<<20 || bytes%4096 != 0 {
		return nil, fmt.Errorf("invalid ION extent")
	}
	ions, err := os.OpenFile("/dev/ion", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer ions.Close()
	a := struct {
		Length, Alignment uint64
		Heap, Flags       uint32
		Handle            int32
		Padding           uint32
	}{Length: uint64(bytes), Alignment: 4096, Heap: 1, Handle: -1}
	if err = Ioctl(int(ions.Fd()), 0xc0204900, unsafe.Pointer(&a)); err != nil {
		return nil, err
	}
	if a.Handle < 0 {
		return nil, fmt.Errorf("ION invalid handle")
	}
	share := struct{ Handle, FD int32 }{a.Handle, -1}
	err = Ioctl(int(ions.Fd()), 0xc0084904, unsafe.Pointer(&share))
	freeErr := Ioctl(int(ions.Fd()), 0xc0044901, unsafe.Pointer(&a.Handle))
	if err != nil || freeErr != nil || share.FD < 0 {
		if share.FD >= 0 {
			_ = syscall.Close(int(share.FD))
		}
		return nil, fmt.Errorf("ION share/free: %v / %v", err, freeErr)
	}
	syscall.CloseOnExec(int(share.FD))
	return os.NewFile(uintptr(share.FD), "s7-ion-dma"), nil
}

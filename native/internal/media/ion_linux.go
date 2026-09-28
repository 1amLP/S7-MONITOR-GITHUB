//go:build linux && (amd64 || arm64)

package media

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	ionAllocRequest = 0xc0204900
	ionFreeRequest  = 0xc0044901
	ionShareRequest = 0xc0084904
	maxScalerION    = 32 << 20
)

type ionAllocation struct {
	Length, Alignment uint64
	HeapMask, Flags   uint32
	Handle            int32
	Padding           uint32
}

type ionHandle struct{ Handle int32 }
type ionFD struct{ Handle, FD int32 }

func allocateIONWith(device *os.File, bytes int, call func(int, uintptr, unsafe.Pointer) error) (*os.File, error) {
	return allocateIONFlags(device, bytes, 0, call)
}

func allocateIONFlags(device *os.File, bytes int, flags uint32, call func(int, uintptr, unsafe.Pointer) error) (*os.File, error) {
	if device == nil || bytes <= 0 || bytes > maxScalerION || call == nil {
		return nil, fmt.Errorf("invalid scaler ION allocation")
	}
	allocation := ionAllocation{
		Length: uint64((bytes + 4095) &^ 4095), Alignment: 4096,
		HeapMask: 1, Flags: flags, Handle: -1,
	}
	fd := int(device.Fd())
	if err := call(fd, ionAllocRequest, unsafe.Pointer(&allocation)); err != nil {
		return nil, fmt.Errorf("ION_ALLOC: %w", err)
	}
	if allocation.Handle < 0 {
		return nil, fmt.Errorf("ION_ALLOC returned invalid handle")
	}
	shared := ionFD{Handle: allocation.Handle, FD: -1}
	shareErr := call(fd, ionShareRequest, unsafe.Pointer(&shared))
	handle := ionHandle{Handle: allocation.Handle}
	freeErr := call(fd, ionFreeRequest, unsafe.Pointer(&handle))
	if shareErr != nil || shared.FD < 0 || freeErr != nil {
		if shared.FD >= 0 {
			_ = syscall.Close(int(shared.FD))
		}
		var err error
		if shareErr != nil {
			err = errors.Join(err, fmt.Errorf("ION_SHARE: %w", shareErr))
		}
		if shared.FD < 0 {
			err = errors.Join(err, fmt.Errorf("ION_SHARE returned invalid fd"))
		}
		if freeErr != nil {
			err = errors.Join(err, fmt.Errorf("ION_FREE: %w", freeErr))
		}
		return nil, err
	}
	return os.NewFile(uintptr(shared.FD), "s7-rgb-dmabuf"), nil
}

// AllocateScalerDMABuf allocates one page-aligned ION buffer. The caller owns
// its fd until the scaler, GPU and DECON have all returned their references.
func AllocateScalerDMABuf(bytes int) (*os.File, error) {
	return allocateDMABuf(bytes, 0)
}

// MFC vb2-ion prepare/finish synchronize cached imported planes on e418.
func allocateCodecDMABuf(bytes int) (*os.File, error) {
	return allocateDMABuf(bytes, 1)
}

func allocateDMABuf(bytes int, flags uint32) (*os.File, error) {
	device, err := os.OpenFile("/dev/ion", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open ION: %w", err)
	}
	buffer, allocErr := allocateIONFlags(device, bytes, flags, ioctl)
	closeErr := device.Close()
	if closeErr != nil {
		if buffer != nil {
			_ = buffer.Close()
		}
		return nil, errors.Join(allocErr, fmt.Errorf("close ION: %w", closeErr))
	}
	return buffer, allocErr
}

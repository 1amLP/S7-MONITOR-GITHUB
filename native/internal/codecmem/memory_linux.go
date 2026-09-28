//go:build linux && (amd64 || arm64)

// Package codecmem owns one bounded NV12 exchange slot per isolated codec.
// Pixel ownership is transferred by the existing request/reply socket, not by
// polling shared counters. This is CPU shared memory, never a camera DMA handle.
package codecmem

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	Decoder      = 1
	Encoder      = 2
	DecoderBytes = 1280 * 720 * 3 / 2
	EncoderBytes = 2560 * 1440 * 3 / 2
	addSeals     = 1033
	getSeals     = 1034
	sizeSeals    = 1 | 2 | 4 // SEAL_SEAL | SEAL_SHRINK | SEAL_GROW, not SEAL_WRITE.
)

func Size(role uint32) (int, error) {
	switch role {
	case Decoder:
		return DecoderBytes, nil
	case Encoder:
		return EncoderBytes, nil
	default:
		return 0, fmt.Errorf("invalid codec shared-memory role %d", role)
	}
}

func seals(fd int) (uintptr, error) {
	n, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), getSeals, 0)
	if e != 0 {
		return 0, e
	}
	return n, nil
}

// New uses the Linux 3.18 memfd ABI. No pathname, partition, persistent pixels,
// ashmem unpin/reclaim, or loader mapping is involved. Size seals prevent
// truncation/growth underneath either process; write access remains necessary.
func New(role uint32) (*os.File, error) {
	size, e := Size(role)
	if e != nil {
		return nil, e
	}
	name := append([]byte(fmt.Sprintf("s7-codec-pixels-%d", role)), 0)
	fd, _, errno := syscall.Syscall(memfdCreate, uintptr(unsafe.Pointer(&name[0])), 3, 0) // CLOEXEC|ALLOW_SEALING
	runtime.KeepAlive(name)
	if errno != 0 {
		return nil, fmt.Errorf("create codec pixel memfd: %w", errno)
	}
	f := os.NewFile(fd, "codec-pixels")
	fail := func(e error) (*os.File, error) { f.Close(); return nil, e }
	if e = f.Truncate(int64(size)); e != nil {
		return fail(e)
	}
	if _, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, addSeals, sizeSeals); errno != 0 {
		return fail(errno)
	}
	if e = Validate(int(fd), role); e != nil {
		return fail(e)
	}
	return f, nil
}

func Validate(fd int, role uint32) error {
	size, e := Size(role)
	if e != nil {
		return e
	}
	var st syscall.Stat_t
	if e = syscall.Fstat(fd, &st); e != nil {
		return e
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Size != int64(size) {
		return fmt.Errorf("codec pixel fd type/size mismatch")
	}
	s, e := seals(fd)
	if e != nil {
		return e
	}
	if s != sizeSeals {
		return fmt.Errorf("codec pixel fd must have exact immutable size seals, got %#x", s)
	}
	return nil
}

// Map owns only the mapping, not the supplied fd. Decoder clients read; encoder
// clients write. The worker uses the opposite direction. mmap never has EXEC.
func Map(fd int, role uint32, writer bool) ([]byte, error) {
	if e := Validate(fd, role); e != nil {
		return nil, e
	}
	n, _ := Size(role)
	prot := syscall.PROT_READ
	if writer {
		prot |= syscall.PROT_WRITE
	}
	return syscall.Mmap(fd, 0, n, prot, syscall.MAP_SHARED)
}
func Unmap(b []byte) error {
	if b == nil {
		return nil
	}
	return syscall.Munmap(b)
}

//go:build linux && (amd64 || arm64)

// Package fimcdma owns ION mappings and V4L2 DMABUF imports for the pending native
// FIMC graph. No device is opened from init(), probe(), or the UI. An allocator
// and fully configured queue must be opened explicitly by a future graph owner.
package fimcdma

import (
	"errors"
	"fmt"
	"perimode/native/internal/linuxio"
	"sync"
	"syscall"
	"unsafe"
)

// Legacy ION LP64, observed in the supplied libion.so; never fall back to an
// unrelated allocator/secure heap on ENOTTY. Only uncached SYSTEM heap is used.
const (
	ionAlloc          = 0xc0204900
	ionFree           = 0xc0044901
	ionShare          = 0xc0084904
	MaxAllocation     = 64 << 20
	MaxAllocatorBytes = 192 << 20
)

type ionAllocation struct {
	Length, Alignment uint64
	HeapMask, Flags   uint32
	Handle            int32
	Padding           uint32
}
type ionHandle struct{ Handle int32 }
type ionFD struct{ Handle, FD int32 }

var ErrBusy = errors.New("DMA mapping is owned by a queued request; access/release refused")
var ErrQuarantined = errors.New("DMA ownership unproven; retain mappings and queue until successful STREAMOFF")

// One ownership lock also covers allocations shared by different graph nodes.
// Callbacks passed to WithCPU must not reenter this package or retain the slice.
var ownership sync.Mutex

type ionOps interface {
	ioctl(uintptr, unsafe.Pointer) error
	mmap(int, int) ([]byte, error)
	munmap([]byte) error
	closeFD(int) error
	close() error
}
type systemION struct{ fd int }

func (s *systemION) ioctl(r uintptr, p unsafe.Pointer) error { return linuxio.Ioctl(s.fd, r, p) }
func (*systemION) mmap(fd, n int) ([]byte, error) {
	return syscall.Mmap(fd, 0, n, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
}
func (*systemION) munmap(b []byte) error { return syscall.Munmap(b) }
func (*systemION) closeFD(fd int) error  { return syscall.Close(fd) }
func (s *systemION) close() error        { e := syscall.Close(s.fd); s.fd = -1; return e }

type Allocator struct {
	mu        sync.Mutex
	ops       ionOps
	allocated int
	closed    bool
	live      map[*Allocation]bool
	handles   map[int32]bool
}
type Allocation struct {
	owner  *Allocator
	fd     int
	data   []byte
	size   int
	pins   int
	closed bool
	orphan bool
}

func VerifyIONABI() error {
	if unsafe.Sizeof(ionAllocation{}) != 32 || unsafe.Offsetof(ionAllocation{}.Handle) != 24 || unsafe.Sizeof(ionHandle{}) != 4 || unsafe.Sizeof(ionFD{}) != 8 {
		return fmt.Errorf("legacy ION LP64 ABI mismatch")
	}
	return nil
}
func OpenAllocator() (*Allocator, error) {
	if e := VerifyIONABI(); e != nil {
		return nil, e
	}
	fd, e := syscall.Open("/dev/ion", syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	var st syscall.Stat_t
	if e = syscall.Fstat(fd, &st); e != nil || st.Mode&syscall.S_IFMT != syscall.S_IFCHR {
		syscall.Close(fd)
		return nil, fmt.Errorf("/dev/ion is not a character device: %v", e)
	}
	return newAllocator(&systemION{fd}), nil
}
func newAllocator(ops ionOps) *Allocator {
	return &Allocator{ops: ops, live: map[*Allocation]bool{}, handles: map[int32]bool{}}
}
func (a *Allocator) Allocate(size int) (out *Allocation, err error) {
	if a == nil {
		return nil, fmt.Errorf("nil ION allocator")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, fmt.Errorf("closed ION allocator")
	}
	if size <= 0 || size > MaxAllocation {
		return nil, fmt.Errorf("invalid ION allocation size")
	}
	n := (size + 4095) &^ 4095
	if a.allocated > MaxAllocatorBytes-n {
		return nil, fmt.Errorf("ION allocation budget exceeded")
	}
	arg := ionAllocation{Length: uint64(n), Alignment: 4096, HeapMask: 1, Flags: 0, Handle: -1}
	if err = a.ops.ioctl(ionAlloc, unsafe.Pointer(&arg)); err != nil {
		return nil, err
	}
	if arg.Handle < 0 {
		return nil, fmt.Errorf("invalid ION handle")
	}
	a.handles[arg.Handle] = true
	h := ionHandle{Handle: arg.Handle}
	defer func() {
		if e := a.ops.ioctl(ionFree, unsafe.Pointer(&h)); e != nil {
			err = errors.Join(err, fmt.Errorf("release ION handle: %w", e))
		} else {
			delete(a.handles, h.Handle)
		}
		// A failed handle release is retained and reported, not hidden as success.
		if err != nil && out != nil {
			ownership.Lock()
			if len(out.data) > 0 {
				if e := a.ops.munmap(out.data); e != nil {
					err = errors.Join(err, e)
					out.orphan = true
					ownership.Unlock()
					out = nil
					return
				}
				out.data = nil
			}
			err = errors.Join(err, a.ops.closeFD(out.fd))
			delete(a.live, out)
			a.allocated -= out.size
			out.closed = true
			ownership.Unlock()
			out = nil
		}
	}()
	share := ionFD{Handle: h.Handle, FD: -1}
	if err = a.ops.ioctl(ionShare, unsafe.Pointer(&share)); err != nil {
		return nil, err
	}
	if share.FD < 0 {
		return nil, fmt.Errorf("invalid ION shared FD")
	}
	b, e := a.ops.mmap(int(share.FD), n)
	if e != nil {
		err = errors.Join(e, a.ops.closeFD(int(share.FD)))
		return nil, err
	}
	out = &Allocation{owner: a, fd: int(share.FD), data: b, size: n}
	a.live[out] = true
	a.allocated += n
	if len(b) != n {
		return out, fmt.Errorf("ION mapping extent mismatch")
	}
	return out, nil
}
func (b *Allocation) Size() int {
	if b == nil {
		return 0
	}
	return b.size
}
func (b *Allocation) WithCPU(fn func([]byte) error) error {
	ownership.Lock()
	defer ownership.Unlock()
	if b == nil || b.closed {
		return fmt.Errorf("closed DMA mapping")
	}
	if b.pins != 0 {
		return ErrBusy
	}
	if fn == nil {
		return fmt.Errorf("nil DMA callback")
	}
	return fn(b.data)
}
func (b *Allocation) Close() error {
	if b == nil {
		return nil
	}
	a := b.owner
	a.mu.Lock()
	defer a.mu.Unlock()
	ownership.Lock()
	defer ownership.Unlock()
	if b.closed {
		return nil
	}
	if b.pins != 0 {
		return ErrBusy
	}
	if len(b.data) != 0 {
		if e := a.ops.munmap(b.data); e != nil {
			return e
		}
		b.data = nil
	}
	// On Linux close(EINTR) must not be retried: the descriptor may be reused.
	e := a.ops.closeFD(b.fd)
	b.fd = -1
	b.closed = true
	delete(a.live, b)
	a.allocated -= b.size
	return e
}
func (a *Allocator) Close() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	var err error
	ownership.Lock()
	for b := range a.live {
		if !b.orphan || b.pins != 0 {
			continue
		}
		if len(b.data) > 0 {
			if e := a.ops.munmap(b.data); e != nil {
				err = errors.Join(err, e)
				continue
			}
			b.data = nil
		}
		err = errors.Join(err, a.ops.closeFD(b.fd))
		b.fd = -1
		b.closed = true
		delete(a.live, b)
		a.allocated -= b.size
	}
	ownership.Unlock()
	if len(a.live) > 0 {
		return errors.Join(err, fmt.Errorf("%d DMA mappings still live: %w", len(a.live), ErrBusy))
	}

	for h := range a.handles {
		x := ionHandle{Handle: h}
		if e := a.ops.ioctl(ionFree, unsafe.Pointer(&x)); e != nil {
			err = errors.Join(err, e)
		} else {
			delete(a.handles, h)
		}
	}
	if err != nil {
		return err
	}
	a.closed = true
	return a.ops.close()
}

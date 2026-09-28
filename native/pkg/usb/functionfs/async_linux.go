//go:build linux && (amd64 || arm64)

package functionfs

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

var ErrAsyncOwnership = errors.New("FunctionFS async requests still own their buffers")

type aioControl struct {
	Data            uint64
	Key, Flags      uint32
	Opcode          uint16
	Priority        int16
	FD              uint32
	Buffer, Bytes   uint64
	Offset          int64
	Reserved        uint64
	Notify, EventFD uint32
}
type aioEvent struct {
	Data, Object    uint64
	Result, Result2 int64
}
type asyncSlot struct {
	control *aioControl
	buffer  []byte
	pending bool
	token   uint64
}
type Completion struct {
	Slot, Bytes int
	Err         error
}

// AsyncEndpoint has one owner. IOCBs and payloads live in mmap, not movable or
// collectable Go memory. Cancellation is not permission to reuse a buffer.
type AsyncEndpoint struct {
	ep       *Endpoint
	context  uintptr
	memory   []byte
	slots    []asyncSlot
	reading  bool
	sequence uint64
	poison   error
	closed   bool
}

func OpenAsyncEndpoint(mount string, number int, reading bool, count, capacity int) (a *AsyncEndpoint, err error) {
	if count < 1 || count > 4 || capacity < 64 || capacity > (4<<20)+512 || unsafe.Sizeof(aioControl{}) != 64 || unsafe.Sizeof(aioEvent{}) != 32 {
		return nil, fmt.Errorf("invalid FunctionFS AIO layout or queue bound")
	}
	a = &AsyncEndpoint{reading: reading}
	defer func() {
		if err != nil {
			err = errors.Join(err, a.Close())
		}
	}()
	a.ep, err = OpenEndpointNonblocking(mount, number, reading)
	if err != nil {
		return a, err
	}
	_, _, e := syscall.Syscall(syscall.SYS_IO_SETUP, uintptr(count), uintptr(unsafe.Pointer(&a.context)), 0)
	if e != 0 {
		return a, e
	}
	stride := (capacity + 4095) &^ 4095
	a.memory, err = syscall.Mmap(-1, 0, 4096+count*stride, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return a, err
	}
	a.slots = make([]asyncSlot, count)
	for i := range a.slots {
		a.slots[i].control = (*aioControl)(unsafe.Pointer(&a.memory[i*64]))
		a.slots[i].buffer = a.memory[4096+i*stride : 4096+i*stride+capacity]
	}
	return a, nil
}

func (a *AsyncEndpoint) Buffer(slot int) ([]byte, error) {
	if a.closed {
		return nil, os.ErrClosed
	}
	if a.poison != nil {
		return nil, a.poison
	}
	if slot < 0 || slot >= len(a.slots) {
		return nil, syscall.EINVAL
	}
	if a.slots[slot].pending {
		return nil, syscall.EAGAIN
	}
	return a.slots[slot].buffer, nil
}
func (a *AsyncEndpoint) Pending() int {
	n := 0
	for _, s := range a.slots {
		if s.pending {
			n++
		}
	}
	return n
}
func (a *AsyncEndpoint) Submit(slot, n int) error {
	return a.SubmitSlice(slot, 0, n)
}

func (a *AsyncEndpoint) SubmitSlice(slot, offset, n int) error {
	b, err := a.Buffer(slot)
	if err != nil {
		return err
	}
	if n <= 0 || offset < 0 || offset > len(b) || n > len(b)-offset || a.sequence == ^uint64(0) {
		return syscall.EINVAL
	}
	a.sequence++
	s := &a.slots[slot]
	*s.control = aioControl{Data: a.sequence, FD: uint32(a.ep.file.Fd()), Buffer: uint64(uintptr(unsafe.Pointer(&b[offset]))), Bytes: uint64(n)}
	if !a.reading {
		s.control.Opcode = 1
	}
	ptr := uintptr(unsafe.Pointer(s.control))
	r, _, e := syscall.Syscall(syscall.SYS_IO_SUBMIT, a.context, 1, uintptr(unsafe.Pointer(&ptr)))
	if r == 1 {
		s.pending = true
		s.token = a.sequence
		return nil
	}
	if e != 0 {
		return e
	}
	return syscall.EAGAIN
}

func (a *AsyncEndpoint) Reap(wait time.Duration) ([]Completion, error) {
	if a.closed {
		return nil, os.ErrClosed
	}
	if a.poison != nil {
		return nil, a.poison
	}
	if a.Pending() == 0 {
		return nil, nil
	}
	var events [4]aioEvent
	minimum := uintptr(0)
	if wait > 0 {
		minimum = 1
	}
	timeout := syscall.NsecToTimespec(wait.Nanoseconds())
	n, _, e := syscall.Syscall6(syscall.SYS_IO_GETEVENTS, a.context, minimum, uintptr(len(a.slots)), uintptr(unsafe.Pointer(&events[0])), uintptr(unsafe.Pointer(&timeout)), 0)
	if e == syscall.EINTR {
		return nil, nil
	}
	if e != 0 {
		return nil, e
	}
	if n > uintptr(len(a.slots)) {
		a.poison = ErrAsyncOwnership
		return nil, a.poison
	}
	var done []Completion
	for _, v := range events[:int(n)] {
		result, err := a.complete(v)
		if err != nil {
			return done, err
		}
		done = append(done, result)
	}
	return done, nil
}

func (a *AsyncEndpoint) complete(v aioEvent) (Completion, error) {
	index := -1
	for i, s := range a.slots {
		if s.pending && s.token == v.Data && uint64(uintptr(unsafe.Pointer(s.control))) == v.Object {
			index = i
			break
		}
	}
	if index < 0 {
		a.poison = ErrAsyncOwnership
		return Completion{}, a.poison
	}
	s := &a.slots[index]
	// Samsung ffs_user_copy_worker calls aio_complete(ret, ret), while other
	// Linux AIO implementations leave result2 zero. Neither permits overflow.
	if v.Result > int64(s.control.Bytes) || v.Result < -4095 || (v.Result2 != 0 && v.Result2 != v.Result) {
		a.poison = ErrAsyncOwnership
		return Completion{}, a.poison
	}
	s.pending = false
	result := Completion{Slot: index}
	if v.Result < 0 {
		result.Err = syscall.Errno(-v.Result)
	} else {
		result.Bytes = int(v.Result)
	}
	return result, nil
}

func (a *AsyncEndpoint) Cancel() error {
	if a.closed {
		return nil
	}
	if a.poison != nil {
		return a.poison
	}
	for i := range a.slots {
		s := &a.slots[i]
		if !s.pending {
			continue
		}
		var event aioEvent
		_, _, e := syscall.Syscall(syscall.SYS_IO_CANCEL, a.context, uintptr(unsafe.Pointer(s.control)), uintptr(unsafe.Pointer(&event)))
		// The pinned 3.18 kernel returns EINPROGRESS and delivers every result
		// through io_getevents. EINVAL may mean completion is already queued.
		if e != 0 && e != syscall.EINPROGRESS && e != syscall.EINVAL {
			return errors.Join(ErrAsyncOwnership, e)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for a.Pending() != 0 {
		if time.Now().After(deadline) {
			return ErrAsyncOwnership
		}
		if _, err := a.Reap(50 * time.Millisecond); err != nil && !errors.Is(err, syscall.EINTR) {
			return errors.Join(ErrAsyncOwnership, err)
		}
	}
	return nil
}

func (a *AsyncEndpoint) Close() error {
	if a == nil || a.closed {
		return nil
	}
	if err := a.Cancel(); err != nil {
		return err
	}
	if a.context != 0 {
		_, _, e := syscall.Syscall(syscall.SYS_IO_DESTROY, a.context, 0, 0)
		if e != 0 {
			return errors.Join(ErrAsyncOwnership, e)
		}
		a.context = 0
	}
	if a.memory != nil {
		if err := syscall.Munmap(a.memory); err != nil {
			return err
		}
		a.memory = nil
		a.slots = nil
	}
	if a.ep != nil {
		if err := a.ep.Close(); err != nil {
			return err
		}
		a.ep = nil
	}
	a.closed = true
	return nil
}

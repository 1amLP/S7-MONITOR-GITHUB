//go:build linux && (amd64 || arm64)

package fimcdma

import (
	"fmt"
	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
	"runtime"
	"syscall"
	"unsafe"
)

const memoryDMABUF = 4

// PreparedNode borrows an already configured node fd. Its graph owner must keep
// that fd open until Queue.Close succeeds, including all quarantine cases.
type PreparedNode struct {
	kernelABI        string
	emptyLeaderStart bool
	importsRequested bool
	fd               int
	kind             uint32
	planes           int
	released         bool
	expected         *media.Format
	role             MetadataRole
	call             func(uintptr, unsafe.Pointer) error
	ready            func() (bool, error)
}

func NewPreparedNode(fd int, kind uint32, planes int) (*PreparedNode, error) {
	if (kind != media.Output && kind != media.Capture) || planes < 2 || planes > 3 {
		return nil, fmt.Errorf("invalid prepared FIMC queue")
	}
	if e := verifyVideoFD(fd); e != nil {
		return nil, e
	}
	n := &PreparedNode{fd: fd, kind: kind, planes: planes}
	n.ready = n.completionReady
	return n, nil
}
func (n *PreparedNode) ioctl(r uintptr, p unsafe.Pointer) error {
	if n.released {
		return fmt.Errorf("released node imports")
	}
	if n.call != nil {
		return n.call(r, p)
	}
	return linuxio.Ioctl(n.fd, r, p)
}
func (n *PreparedNode) buffer(r uintptr, b *media.Buffer, p []media.Plane) error {
	if len(p) != n.planes {
		return fmt.Errorf("invalid FIMC plane array")
	}
	b.PlanesPointer = uint64(uintptr(unsafe.Pointer(&p[0])))
	b.Length = uint32(len(p))
	e := n.ioctl(r, unsafe.Pointer(b))
	runtime.KeepAlive(p)
	if e != nil {
		return e
	}
	if b.Length != uint32(len(p)) {
		return fmt.Errorf("driver changed FIMC plane count")
	}
	return nil
}
func (n *PreparedNode) Queue(i uint32, p []Plane) error {
	if len(p) != n.planes {
		return fmt.Errorf("wrong FIMC plane count")
	}
	b := media.Buffer{Index: i, Type: n.kind, Memory: memoryDMABUF}
	a := make([]media.Plane, len(p))
	for j, x := range p {
		if x.FD < 0 || uint64(x.FD) > 0x7fffffff || x.Length == 0 {
			return fmt.Errorf("invalid DMA import")
		}
		a[j].Length = x.Length
		a[j].Memory = uint64(x.FD)
		if n.kind == media.Output && n.expected != nil {
			payload := n.expected.PlaneSize(j)
			if payload == 0 || payload > x.Length {
				return fmt.Errorf("FIMC output payload exceeds DMA allocation")
			}
			a[j].Used = payload
		}
	}
	// The native allocator page-aligns lengths. OUTPUT bytesused must describe
	// sizeimage, not that padding: vb2 otherwise substitutes the whole length.
	// Explicit fencing is not requested; no vendor fence flag/fd is guessed.
	return n.buffer(media.QueueBuffer, &b, a)
}
func (n *PreparedNode) Dequeue() (Completion, error) {
	if n.ready != nil {
		ready, err := n.ready()
		if err != nil {
			return Completion{}, err
		}
		if !ready {
			return Completion{}, syscall.EAGAIN
		}
	}
	b := media.Buffer{Type: n.kind, Memory: memoryDMABUF}
	p := make([]media.Plane, n.planes)
	if e := n.buffer(media.DequeueBuffer, &b, p); e != nil {
		return Completion{}, e
	}
	if b.Type != n.kind || b.Memory != memoryDMABUF {
		return Completion{}, fmt.Errorf("driver changed FIMC buffer type")
	}
	c := Completion{Index: b.Index, Flags: b.Flags, Lengths: make([]uint32, n.planes), Used: make([]uint32, n.planes), Offsets: make([]uint32, n.planes)}
	for i, v := range p {
		c.Lengths[i] = v.Length
		c.Used[i] = v.Used
		c.Offsets[i] = v.Offset
	}
	return c, nil
}

// FIMC logs every empty DQBUF as an error. Its vb2 poll callback exposes the
// same completion queue without dequeueing or flooding the kernel ring.
func (n *PreparedNode) completionReady() (bool, error) {
	p := struct {
		FD               int32
		Events, Returned int16
	}{FD: int32(n.fd), Events: 1 | 0x40}
	if n.kind == media.Output {
		p.Events = 4 | 0x100
	}
	timeout := syscall.Timespec{}
	_, _, err := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&p)), 1, uintptr(unsafe.Pointer(&timeout)), 0, 0, 0)
	if err != 0 {
		return false, err
	}
	if p.Returned&(0x10|0x20) != 0 {
		return false, syscall.ENODEV
	}
	// POLLERR on an empty, just-started ISP output is normal. Drain only a
	// completion; graph progress already has a bounded watchdog.
	return p.Returned&p.Events != 0, nil
}
func (n *PreparedNode) Start() error { k := n.kind; return n.ioctl(media.StreamOn, unsafe.Pointer(&k)) }
func (n *PreparedNode) Stop() error  { k := n.kind; return n.ioctl(media.StreamOff, unsafe.Pointer(&k)) }
func (n *PreparedNode) Release() error {
	if n.released {
		return nil
	}
	r := media.Request{Type: n.kind, Memory: memoryDMABUF}
	if e := n.ioctl(media.RequestBuffers, unsafe.Pointer(&r)); e != nil {
		return e
	}
	if r.Count != 0 {
		return fmt.Errorf("driver retained imports")
	}
	n.released = true
	return nil
}

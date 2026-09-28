//go:build linux && (amd64 || arm64)

package fimg2d

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"perimode/native/internal/linuxio"
)

type memorySignature struct {
	target  uint8
	sources [g2dMaxSources]uint8
}

// e418 aliases userptr.length and dmabuf.offset but never initializes offset
// on USERPTR -> DMABUF. Keep each kernel context's memory roles immutable.
// Contexts persist across frames; there is no per-frame open/close.
type taskContexts struct {
	mu               sync.Mutex
	files            map[memorySignature]*os.File
	closed, poisoned bool
}

func commandSignature(cmd *task, sources []image) (memorySignature, error) {
	var key memorySignature
	if cmd == nil || len(sources) == 0 || len(sources) > g2dMaxSources || int(cmd.NumSources) != len(sources) || cmd.Sources != uintptr(unsafe.Pointer(&sources[0])) {
		return key, fmt.Errorf("invalid G2D command sources")
	}
	check := func(im image, target bool) error {
		if im.Memory == bufferEmpty && !target && im.Flags&colorFill != 0 && im.NumPlanes == 0 {
			return nil
		}
		if (im.Memory != bufferUser && im.Memory != bufferDMA) || im.NumPlanes != 1 || im.Plane[0].Offset != 0 {
			return fmt.Errorf("G2D requires fixed memory roles and zero-offset single planes")
		}
		return nil
	}
	if err := check(cmd.Target, true); err != nil {
		return key, err
	}
	key.target = cmd.Target.Memory
	for i, source := range sources {
		if err := check(source, false); err != nil {
			return key, err
		}
		key.sources[i] = source.Memory
	}
	return key, nil
}

func openTaskFile() (*os.File, error) {
	f, err := os.OpenFile("/dev/fimg2d", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var st syscall.Stat_t
	err = syscall.Fstat(int(f.Fd()), &st)
	if err == nil {
		var identity []byte
		identity, err = os.ReadFile("/sys/class/misc/fimg2d/dev")
		if err == nil {
			err = checkDeviceIdentity(st, identity)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (p *taskContexts) process(cmd *task, sources []image) error {
	pins, err := pinTaskSources(cmd, sources)
	if err != nil {
		return err
	}
	defer pins.Unpin()
	key, err := commandSignature(cmd, sources)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return os.ErrClosed
	}
	if p.poisoned {
		return ErrComposerPoisoned
	}
	f := p.files[key]
	if f == nil {
		if len(p.files) >= 16 {
			return fmt.Errorf("G2D context bound exceeded")
		}
		f, err = openTaskFile()
		if err != nil {
			return err
		}
		if p.files == nil {
			p.files = make(map[memorySignature]*os.File)
		}
		p.files[key] = f
	}
	err = linuxio.Ioctl(int(f.Fd()), processIOCTL, unsafe.Pointer(cmd))
	runtime.KeepAlive(sources)
	if err != nil || cmd.Flags&processError != 0 {
		p.poisoned = true
	}
	return err
}

// Sources is a UAPI integer address, so Go cannot relocate it when a caller's
// stack grows. Pin the real descriptors, then derive the address at submission.
func pinTaskSources(cmd *task, sources []image) (*runtime.Pinner, error) {
	if cmd == nil || len(sources) == 0 || len(sources) > g2dMaxSources || int(cmd.NumSources) != len(sources) {
		return nil, fmt.Errorf("invalid G2D command sources")
	}
	pins := new(runtime.Pinner)
	pins.Pin(cmd)
	pins.Pin(&sources[0])
	cmd.Sources = uintptr(unsafe.Pointer(&sources[0]))
	return pins, nil
}

func (p *taskContexts) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.poisoned {
		return ErrComposerPoisoned
	}
	p.closed = true
	var err error
	for key, f := range p.files {
		err = errors.Join(err, f.Close())
		delete(p.files, key)
	}
	return err
}

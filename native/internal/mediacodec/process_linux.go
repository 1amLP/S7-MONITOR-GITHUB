//go:build linux && (amd64 || arm64)

package mediacodec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"perimode/native/internal/childproc"
	"perimode/native/internal/codecmem"
	"perimode/native/internal/media"
	"perimode/native/internal/mediaruntime"
)

const (
	BridgePath     = "/opt/s7-codec/bin/mediacodec-bridge"
	RuntimeLibrary = "/system/lib64/libmediandk.so"
	LinkerPath     = "/opt/s7-hub/bin/linker64"
)

// No mixing with the lhd adapter libraries: its libutils/liblog intentionally
// implement a different, tiny ABI and cannot satisfy the media runtime.
func Available() error {
	if _, e := mediaruntime.LoadPin(); e != nil {
		return e
	}
	st, e := os.Lstat(BridgePath)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || st.Size() == 0 {
		return fmt.Errorf("MediaCodec bridge unavailable")
	}
	return nil
}

var quarantine = struct {
	sync.Mutex
	role [3]bool
}{}

func blocked(role uint32) bool {
	quarantine.Lock()
	defer quarantine.Unlock()
	return quarantine.role[role]
}
func block(role uint32) { quarantine.Lock(); quarantine.role[role] = true; quarantine.Unlock() }

type session struct {
	tx        sync.Mutex // Serializes calls with cooperative cancellation/close.
	life      sync.Mutex // Never held by a vendor transaction.
	ctx       context.Context
	closeOnce sync.Once
	closeErr  error
	closed    bool // guarded by tx
	finished  bool // guarded by life
	forced    bool // guarded by life
	remote    bool
	control   net.Conn
	conn      net.Conn
	cmd       *exec.Cmd
	done      chan struct{}
	waitErr   error // written before closing done
	serial    uint32
	role      uint32
	poisoned  error
	cancel    func() bool
	buffer    []byte
	pixels    []byte // Mapping accessed/unmapped only under tx, never exposed to callers.
}

func start(ctx context.Context, role uint32, command *exec.Cmd) (*session, error) {
	if role != roleDecode && role != roleEncode {
		return nil, fmt.Errorf("invalid MediaCodec role")
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if blocked(role) {
		return nil, media.ErrQuarantined
	}
	fds, e := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	parent := os.NewFile(uintptr(fds[0]), "codec-parent")
	child := os.NewFile(uintptr(fds[1]), "codec-worker")
	defer parent.Close()
	defer child.Close()
	conn, e := net.FileConn(parent)
	if e != nil {
		return nil, e
	}
	pixelsFile, e := codecmem.New(role)
	if e != nil {
		conn.Close()
		return nil, e
	}
	defer pixelsFile.Close()
	pixels, e := codecmem.Map(int(pixelsFile.Fd()), role, role == roleEncode)
	if e != nil {
		conn.Close()
		return nil, e
	}
	command.ExtraFiles = []*os.File{child, pixelsFile}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// No stdout capture containing pixels, and no unbounded worker log queue.
	command.Stdout = nil
	command.Stderr = nil
	if e = childproc.Start(command); e != nil {
		codecmem.Unmap(pixels)
		conn.Close()
		return nil, e
	}
	s := &session{conn: conn, cmd: command, done: make(chan struct{}), role: role, pixels: pixels}
	go func() { s.waitErr = childproc.Wait(command); close(s.done) }()
	s.watchContext(ctx)
	return s, nil
}
func nativeStart(ctx context.Context, role uint32) (*session, error) {
	if role != roleDecode && role != roleEncode {
		return nil, fmt.Errorf("invalid MediaCodec role")
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if e := Available(); e != nil {
		return nil, e
	}
	if blocked(role) {
		return nil, media.ErrQuarantined
	}
	ch, e := mediaruntime.OpenChannel(ctx, role)
	if e != nil {
		return nil, e
	}
	s := &session{conn: ch.Data, control: ch.Control, role: role, remote: true, pixels: ch.Pixels}
	s.watchContext(ctx)
	return s, nil
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// call also serializes against context cancellation, not just the public
// codec's mutex. A complete transaction may finish before cooperative CLOSE.
// A partial IPC transaction is never replayed into an uncertain stream.
func (s *session) call(h header, data []byte, timeout time.Duration) (header, []byte, error) {
	s.tx.Lock()
	defer s.tx.Unlock()
	if s.closed {
		return header{}, nil, os.ErrClosed
	}
	if s.ctx != nil && s.ctx.Err() != nil {
		return header{}, nil, s.ctx.Err()
	}
	return s.callLocked(h, data, timeout)
}

func (s *session) callLocked(h header, data []byte, timeout time.Duration) (header, []byte, error) {
	if s.poisoned != nil {
		return header{}, nil, s.poisoned
	}
	if len(data) > maxPayload || s.serial == ^uint32(0) {
		return header{}, nil, fmt.Errorf("MediaCodec IPC limit")
	}
	if h.storage == storagePixels {
		if s.role != roleEncode || h.op != opSubmit || len(data) != 0 || h.size == 0 || int(h.size) > len(s.pixels) {
			return header{}, nil, fmt.Errorf("invalid shared input extent")
		}
	} else {
		if h.storage != 0 {
			return header{}, nil, fmt.Errorf("unknown MediaCodec storage mode")
		}
		h.size = uint32(len(data))
	}
	s.serial++
	h.serial = s.serial
	raw := h.marshal()
	if e := s.conn.SetDeadline(time.Now().Add(timeout)); e != nil {
		return header{}, nil, e
	}
	transaction := func() (header, []byte, error) {
		if e := writeAll(s.conn, raw[:]); e != nil {
			return header{}, nil, e
		}
		if e := writeAll(s.conn, data); e != nil {
			return header{}, nil, e
		}
		if _, e := io.ReadFull(s.conn, raw[:]); e != nil {
			return header{}, nil, e
		}
		reply, e := response(raw[:], h.op, h.serial)
		if e != nil {
			return header{}, nil, e
		}
		bound := uint32(0)
		if h.op == opDrain {
			if s.role == roleDecode {
				bound = 1280 * 720 * 3 / 2
			} else {
				bound = 4 << 20
			}
		}
		if reply.size > bound {
			return header{}, nil, fmt.Errorf("MediaCodec output exceeds negotiated bound")
		}
		if int(reply.size) > cap(s.buffer) {
			s.buffer = make([]byte, reply.size)
		} else {
			s.buffer = s.buffer[:reply.size]
		}
		if reply.storage == storagePixels {
			if s.role != roleDecode || reply.size != codecmem.DecoderBytes || len(s.pixels) != codecmem.DecoderBytes {
				return header{}, nil, fmt.Errorf("unexpected shared MediaCodec response")
			}
			// Copy while tx still owns the mapping. Callers get Go-owned bytes which
			// remain valid even if cancellation closes/unmaps the session concurrently.
			copy(s.buffer, s.pixels)
		} else {
			if s.role == roleDecode && reply.size != 0 {
				return header{}, nil, fmt.Errorf("inline decoded pixels are not protocol v3")
			}
			if _, e := io.ReadFull(s.conn, s.buffer); e != nil {
				return header{}, nil, e
			}
		}
		return reply, s.buffer, nil
	}
	reply, b, e := transaction()
	if e != nil {
		s.poisoned = fmt.Errorf("MediaCodec IPC op %d: %w", h.op, e)
		return header{}, nil, s.poisoned
	}
	if reply.status == -11 {
		return reply, nil, syscall.EAGAIN
	}
	if reply.status != 0 {
		return reply, nil, fmt.Errorf("AMediaCodec op %d failed: status %d (no alternate codec selected)", h.op, reply.status)
	}
	return reply, b, nil
}

// Context cancellation is an ordinary camera/USB lifecycle event, not evidence
// of lost hardware ownership. Preserve both sockets while a bounded stop/delete
// runs. Only an unresponsive worker is forcibly aborted and quarantined.
const cancelGrace = 1200 * time.Millisecond

func (s *session) watchContext(ctx context.Context) {
	s.ctx = ctx
	s.life.Lock()
	s.cancel = context.AfterFunc(ctx, func() {
		deadline := time.AfterFunc(cancelGrace, s.abort)
		defer deadline.Stop()
		_ = s.close()
	})
	s.life.Unlock()
}

func (s *session) abort() {
	s.life.Lock()
	defer s.life.Unlock()
	if s.finished || s.forced {
		return
	}
	s.forced = true
	// No codec mutex: this must interrupt an in-flight socket wait.
	_ = s.conn.Close()
	if s.remote {
		_ = s.control.Close()
	} else {
		_ = s.cmd.Process.Kill()
	}
}

func (s *session) close() error {
	s.closeOnce.Do(func() {
		s.life.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		s.life.Unlock()
		s.tx.Lock()
		s.closed = true
		_, _, clean := s.callLocked(header{op: opClose}, nil, 400*time.Millisecond)
		s.tx.Unlock()
		if clean != nil {
			s.abort() // Partial protocol/failed cleanup cannot be continued.
		}
		var exit error
		if s.remote {
			exit = mediaruntime.ConfirmExit(s.control)
		} else {
			timer := time.NewTimer(400 * time.Millisecond)
			select {
			case <-s.done:
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
				s.abort()
				select {
				case <-s.done:
				case <-time.After(400 * time.Millisecond):
					exit = fmt.Errorf("MediaCodec worker exit unconfirmed")
				}
			}
			if exit == nil {
				exit = s.waitErr // Only read after done.
			}
		}
		s.life.Lock()
		s.finished = true
		if s.forced {
			exit = errors.Join(exit, fmt.Errorf("MediaCodec worker required forced cancellation"))
		}
		s.life.Unlock()
		if clean != nil || exit != nil {
			block(s.role)
			s.closeErr = errors.Join(media.ErrQuarantined, clean, exit)
		}
		if s.remote {
			_ = s.control.Close()
		}
		_ = s.conn.Close()
		s.tx.Lock()
		s.buffer = nil
		if e := codecmem.Unmap(s.pixels); e != nil {
			s.closeErr = errors.Join(s.closeErr, e)
		}
		s.pixels = nil
		s.tx.Unlock()
	})
	return s.closeErr
}

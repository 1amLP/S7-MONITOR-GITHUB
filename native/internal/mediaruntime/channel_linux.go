//go:build linux && (amd64 || arm64)

package mediaruntime

import (
	"context"
	"errors"
	"fmt"
	"perimode/native/internal/codecmem"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Channel separates codec pixels from process lifetime. Closing Control aborts
// a worker even while the vendor call is stuck and cannot read its data socket.
// No goroutine on the broker races the codec for bytes from the data stream.
type Channel struct {
	Data      net.Conn
	Control   net.Conn
	Pixels    []byte
	closeOnce sync.Once
	closeErr  error
}

func (c *Channel) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = errors.Join(c.Data.Close(), c.Control.Close(), codecmem.Unmap(c.Pixels))
		c.Pixels = nil
	})
	return c.closeErr
}

func socketPair() (*os.File, *os.File, error) {
	fds, e := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if e != nil {
		return nil, nil, e
	}
	return os.NewFile(uintptr(fds[0]), "codec-client"), os.NewFile(uintptr(fds[1]), "codec-worker"), nil
}

func sendChannel(c *net.UnixConn, role byte, f, pixels *os.File) error {
	b := []byte{'S', '7', 'R', 'A', role, 1, 0, 0}
	n, o, e := c.WriteMsgUnix(b, syscall.UnixRights(int(f.Fd()), int(pixels.Fd())), nil)
	if e != nil {
		return e
	}
	if o != syscall.CmsgSpace(8) {
		return io.ErrShortWrite
	}
	return writeAll(c, b[n:])
}

// Protocol v3 transfers exactly two descriptors: a stream socket and a sealed
// fixed-size raw-pixel memfd, in that order. Validate BEFORE exposing either.
func receiveChannel(c *net.UnixConn, role byte) (net.Conn, []byte, error) {
	var b [8]byte
	control := make([]byte, syscall.CmsgSpace(4*8))
	n, on, flags, _, e := c.ReadMsgUnix(b[:], control)
	var fds []int
	messages, pe := syscall.ParseSocketControlMessage(control[:on])
	for _, m := range messages {
		r, re := syscall.ParseUnixRights(&m)
		if re != nil {
			pe = errors.Join(pe, re)
		} else {
			fds = append(fds, r...)
		}
	}
	// Apply CLOEXEC to every received fd before any other path can exec. Go's
	// UnixConn receive operation uses MSG_CMSG_CLOEXEC on Linux as well.
	for _, fd := range fds {
		syscall.CloseOnExec(fd)
	}
	defer func() {
		for _, fd := range fds {
			_ = syscall.Close(fd)
		}
	}()
	if e != nil {
		return nil, nil, e
	}
	if pe != nil || flags&(syscall.MSG_CTRUNC|syscall.MSG_TRUNC) != 0 || len(fds) != 2 {
		return nil, nil, errors.New("invalid MediaCodec descriptor transfer")
	}
	if _, e = io.ReadFull(c, b[n:]); e != nil {
		return nil, nil, e
	}
	if string(b[:4]) != "S7RA" || b[4] != role || b[5] != 1 || b[6] != 0 || b[7] != 0 {
		return nil, nil, errors.New("media broker v3 role acknowledgement mismatch")
	}
	fd := fds[0]
	var st syscall.Stat_t
	if e = syscall.Fstat(fd, &st); e != nil {
		return nil, nil, e
	}
	kind, e := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if e != nil || st.Mode&syscall.S_IFMT != syscall.S_IFSOCK || kind != syscall.SOCK_STREAM {
		return nil, nil, errors.New("MediaCodec descriptor is not a stream socket")
	}
	pixels, e := codecmem.Map(fds[1], uint32(role), role == codecmem.Encoder)
	if e != nil {
		return nil, nil, fmt.Errorf("MediaCodec pixel descriptor: %w", e)
	}
	f := os.NewFile(uintptr(fd), "codec-data")
	conn, e := net.FileConn(f)
	_ = f.Close()
	fds = fds[1:]
	if e != nil {
		_ = codecmem.Unmap(pixels)
		return nil, nil, e
	}
	return conn, pixels, nil
}

// serveCodec owns Wait exactly once. A client abort, runtime stop, or failure to
// deliver the data fd terminates this ONE worker. Unknown OMX cleanup is latched
// by its caller; it never admits another worker over an uncertain remote owner.
func serveCodec(ctx context.Context, c *net.UnixConn, role byte, makeCommand func(byte) *exec.Cmd) bool {
	return serveCodecFinal(ctx, c, role, makeCommand, func(clean bool, ack func() error) bool {
		if ack() != nil {
			return false
		}
		return clean
	})
}
func serveCodecFinal(ctx context.Context, c *net.UnixConn, role byte, makeCommand func(byte) *exec.Cmd, finish func(bool, func() error) bool) (clean bool) {
	client, worker, e := socketPair()
	if e != nil {
		return false
	}
	defer client.Close()
	defer worker.Close()
	pixels, e := codecmem.New(uint32(role))
	if e != nil {
		return false
	}
	defer pixels.Close()
	cmd := makeCommand(role)
	cmd.ExtraFiles = []*os.File{worker, pixels}
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
	if e = cmd.Start(); e != nil {
		return false
	}
	worker.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	if e = sendChannel(c, role, client, pixels); e != nil {
		_ = cmd.Process.Kill()
		<-done
		return false
	}
	client.Close()
	_ = c.SetDeadline(time.Time{})
	// Control is silent until process exit; any data/EOF is cancellation.
	lost := make(chan struct{})
	go func() { var b [1]byte; _, _ = c.Read(b[:]); close(lost) }()
	var result error
	select {
	case result = <-done:
		clean = result == nil
	case <-lost:
		_ = cmd.Process.Kill()
		result = <-done
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		result = <-done
	}
	// A pending cancellation never races with the next role admission.
	select {
	case <-lost:
		clean = false
	default:
	}
	if ctx.Err() != nil {
		clean = false
	}
	_ = c.SetReadDeadline(time.Now())
	<-lost
	trailer := []byte{'S', '7', 'E', 'X', 0, 0, 0, 0}
	if !clean {
		trailer[4] = 1
	}
	_ = c.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	// Send only AFTER Wait; a codec STOP response alone does not release its owner.
	return finish(clean, func() error { return writeAll(c, trailer) })
}

func connectChannel(ctx context.Context, path string, role uint32) (*Channel, error) {
	if role != 1 && role != 2 {
		return nil, errors.New("invalid codec runtime role")
	}
	d := net.Dialer{Timeout: 300 * time.Millisecond}
	conn, e := d.DialContext(ctx, "unix", path)
	if e != nil {
		return nil, e
	}
	c, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		return nil, errors.New("codec channel is not Unix socket")
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	if e = writeAll(c, []byte{'S', '7', 'R', '3', byte(role), 0, 0, 0}); e != nil {
		c.Close()
		return nil, e
	}
	data, pixels, e := receiveChannel(c, byte(role))
	if e != nil {
		c.Close()
		return nil, e
	}
	if e = ctx.Err(); e != nil {
		data.Close()
		codecmem.Unmap(pixels)
		c.Close()
		return nil, e
	}
	if e = c.SetDeadline(time.Time{}); e != nil {
		data.Close()
		codecmem.Unmap(pixels)
		c.Close()
		return nil, e
	}
	return &Channel{Data: data, Control: c, Pixels: pixels}, nil
}

func OpenChannel(ctx context.Context, role uint32) (*Channel, error) {
	s := Snapshot()
	if !s.Ready {
		return nil, fmt.Errorf("MediaCodec native services unavailable: %s: %s", s.State, s.Error)
	}
	return connectChannel(ctx, Socket, role)
}

package monitor

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

type Status struct {
	Settings
	Generation   uint32 `json:"generation"`
	Acknowledged uint32 `json:"acknowledged"`
	HostState    uint32 `json:"host_state"`
	HostError    uint32 `json:"host_error"`
	HostPresent  bool   `json:"host_present"`
	Consumer     bool   `json:"consumer"`
	Frames       uint64 `json:"frames"`
	Dropped      uint64 `json:"dropped"`
	Bytes        uint64 `json:"bytes"`
}
type subscriber struct {
	frames chan Frame
	conn   net.Conn
}
type Broker struct {
	mu            sync.Mutex
	webcam        [16]byte
	state         Status
	keyRequest    uint32
	needKey       bool
	previous, pts uint64
	sub           *subscriber
	lastFrame     time.Time
	lastHostPoll  time.Time
}

// Camera preference is a separate control record, never a monitor generation change.
func (b *Broker) SetWebcam(width, height, fps int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	copy(b.webcam[:], "S7W1")
	le.PutUint32(b.webcam[4:], uint32(width))
	le.PutUint32(b.webcam[8:], uint32(height))
	le.PutUint32(b.webcam[12:], uint32(fps))
}
func (b *Broker) Webcam() [16]byte { b.mu.Lock(); defer b.mu.Unlock(); return b.webcam }

func NewBroker() *Broker {
	return &Broker{state: Status{Settings: DefaultSettings(), Generation: 1}, needKey: true, keyRequest: 1}
}
func (b *Broker) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state
	s.HostPresent = !b.lastHostPoll.IsZero() && time.Since(b.lastHostPoll) < 3*time.Second
	if s.HostState == 2 && time.Since(b.lastFrame) > 3*time.Second {
		s.HostState = 0
	}
	return s
}
func (b *Broker) drainLocked() {
	if b.sub != nil {
		for {
			select {
			case <-b.sub.frames:
				b.state.Dropped++
			default:
				return
			}
		}
	}
}
func (b *Broker) keyLocked() {
	if !b.needKey {
		b.keyRequest++
	}
	b.needKey = true
}
func (b *Broker) Configure(s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state.Settings != s {
		b.state.Settings = s
		b.state.Generation++
		if b.state.Generation == 0 {
			b.state.Generation = 1
		}
		b.state.Acknowledged = 0
		b.state.HostState = 0
		b.state.HostError = 0
		b.previous = 0
		b.pts = 0
		b.keyRequest++
		b.needKey = true
		b.drainLocked()
	}
	return nil
}
func (b *Broker) RequestKeyframe() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.keyRequest++
	b.needKey = true
	b.drainLocked()
}
func (b *Broker) HostDisconnected() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state.Acknowledged = 0
	b.state.HostState = 0
	b.state.HostError = 0
	b.lastHostPoll = time.Time{}
	b.previous = 0
	b.pts = 0
	b.keyRequest++
	b.needKey = true
	b.drainLocked()
}
func (b *Broker) HostStatus(data []byte) error {
	if len(data) != 12 {
		return errors.New("host status size")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rev, state, code := le.Uint32(data), le.Uint32(data[4:]), le.Uint32(data[8:])
	if rev != b.state.Generation {
		return nil
	}
	if state > 3 {
		return errors.New("invalid host state")
	}
	b.state.Acknowledged = rev
	b.state.HostState = state
	b.state.HostError = code
	return nil
}
func (b *Broker) Configuration() [ConfigSize]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	// This request is the host driver's heartbeat, independent of video enable.
	b.lastHostPoll = time.Now()
	var out [ConfigSize]byte
	copy(out[:], "S7C1")
	le.PutUint16(out[4:], 2)
	var flags uint16
	if b.state.Enabled {
		flags |= 1
	}
	if b.sub != nil {
		flags |= 2
	}
	le.PutUint16(out[6:], flags)
	le.PutUint32(out[8:], b.state.Generation)
	w, h := b.state.Settings.Dimensions()
	le.PutUint32(out[12:], w)
	le.PutUint32(out[16:], h)
	le.PutUint32(out[20:], b.state.FPS)
	le.PutUint32(out[24:], b.state.Bitrate)
	le.PutUint32(out[28:], b.keyRequest)
	le.PutUint32(out[32:], b.state.Acknowledged)
	le.PutUint32(out[36:], b.state.HostState)
	le.PutUint32(out[40:], b.state.HostError)
	le.PutUint32(out[44:], b.state.GOPSeconds)
	return out
}

// Takes ownership of immutable payload. Queue capacity is three access units.
func (b *Broker) Publish(f Frame) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state.Bytes += uint64(len(f.Payload) + HeaderSize)
	w, h := b.state.Settings.Dimensions()
	fw, fh := f.Dimensions()
	if b.sub == nil || !b.state.Enabled || f.Generation != b.state.Generation || f.FPS != b.state.FPS || w != fw || h != fh {
		b.state.Dropped++
		return nil
	}
	if f.Sequence <= b.previous || (b.previous != 0 && f.PTS <= b.pts) {
		return errors.New("nonmonotonic monitor sequence/timestamp")
	}
	if f.Sequence != b.previous+1 {
		b.keyLocked()
	}
	b.previous = f.Sequence
	b.pts = f.PTS
	if b.needKey && !f.Key {
		b.state.Dropped++
		return nil
	}
	if f.Key {
		b.needKey = false
	}
	select {
	case b.sub.frames <- f:
		b.state.Frames++
		b.lastFrame = time.Now()
		b.state.Acknowledged = f.Generation
		b.state.HostState = 2
		b.state.HostError = 0
	default:
		b.state.Dropped++
		b.drainLocked()
		b.keyLocked()
	}
	return nil
}
func (b *Broker) Serve(ctx context.Context, token []byte) error {
	l, e := net.Listen("tcp4", Address)
	if e != nil {
		return e
	}
	return b.ServeListener(ctx, l, token)
}
func (b *Broker) ServeListener(ctx context.Context, l net.Listener, token []byte) error {
	defer l.Close()
	if len(token) != 32 {
		return errors.New("monitor requires 32 byte application token")
	}
	stop := context.AfterFunc(ctx, func() {
		l.Close()
		b.mu.Lock()
		var c net.Conn
		if b.sub != nil {
			c = b.sub.conn
		}
		b.mu.Unlock()
		if c != nil {
			c.Close()
		}
	})
	defer stop()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		b.serveConsumer(ctx, conn, token)
	}
}
func (b *Broker) serveConsumer(ctx context.Context, c net.Conn, token []byte) {
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	var supplied [32]byte
	if _, e := io.ReadFull(c, supplied[:]); e != nil || subtle.ConstantTimeCompare(token, supplied[:]) != 1 {
		return
	}
	sub := &subscriber{frames: make(chan Frame, 3), conn: c}
	b.mu.Lock()
	b.sub = sub
	b.state.Consumer = true
	b.keyRequest++
	b.needKey = true
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.sub == sub {
			b.sub = nil
			b.state.Consumer = false
			b.keyRequest++
			b.needKey = true
		}
		b.mu.Unlock()
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer c.Close()
		var lastIDR uint32
		for {
			var data [ConfigSize]byte
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, e := io.ReadFull(c, data[:]); e != nil {
				return
			}
			s, idr, e := ClientConfiguration(data[:])
			if e != nil || b.Configure(s) != nil {
				return
			}
			if idr != lastIDR {
				lastIDR = idr
				b.RequestKeyframe()
			}
		}
	}()
	defer func() { c.Close(); <-done }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case f := <-sub.frames:
			header := f.Header()
			_ = c.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
			if e := writeAll(c, header[:]); e != nil {
				return
			}
			if e := writeAll(c, f.Payload); e != nil {
				return
			}
		}
	}
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n <= 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

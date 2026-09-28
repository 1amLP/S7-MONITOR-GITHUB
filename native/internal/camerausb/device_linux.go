//go:build linux && (amd64 || arm64)

// Package camerausb is the private, encoded-only WinUSB camera output.
package camerausb

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"syscall"
	"time"

	"perimode/native/internal/camera"
	"perimode/native/internal/media"
	"perimode/native/internal/uvcout"
	"perimode/native/pkg/cameramode"
	"perimode/native/pkg/camerawire"
	"perimode/native/pkg/usb/configfs"
	"perimode/native/pkg/usb/functionfs"
)

const mount = "/dev/s7-camera"
const FunctionName = "ffs.s7camera"

type queued struct {
	bytes, offset, submitted int
	frame                    bool
}
type Device struct {
	ep                          *functionfs.Ep0
	rx, tx                      *functionfs.AsyncEndpoint
	unmount                     func() error
	cancel                      context.CancelFunc
	done                        chan struct{}
	events                      chan functionfs.EventType
	epError                     chan error
	authorize                   func(camerawire.Command) error
	currentMode                 func(uint64) (camera.Mode, error)
	enabled, wanted, on, closed bool
	mode                        camera.Mode
	token, session, sequence    uint64
	lastPTS                     int64
	havePTS                     bool
	boundary                    bool
	queued                      [3]queued
	order                       []int
	reply                       *camerawire.Header
	stats                       uvcout.Stats
}

func Open(parent context.Context, g *configfs.Gadget, c *configfs.Config, authorize func(camerawire.Command) error, currentMode func(uint64) (camera.Mode, error)) (d *Device, err error) {
	if authorize == nil || currentMode == nil {
		return nil, fmt.Errorf("camera transport owner check missing")
	}
	ctx, cancel := context.WithCancel(parent)
	d = &Device{cancel: cancel, events: make(chan functionfs.EventType, 16), epError: make(chan error, 1), authorize: authorize, currentMode: currentMode}
	defer func() {
		if err != nil {
			err = errors.Join(err, d.Close())
		}
	}()
	if _, err = g.CreateFunction(FunctionName); err != nil {
		return d, err
	}
	if d.unmount, err = functionfs.MountPrivate("s7camera", mount, "functionfs_s7"); err != nil {
		return d, err
	}
	if d.ep, err = functionfs.NewEp0Nonblocking(mount); err != nil {
		return d, err
	}
	fs, hs := camerawire.USBDescriptors()
	if err = d.ep.WriteWinUSBDescriptors(fs, hs, camerawire.InterfaceGUID); err != nil {
		return d, err
	}
	if err = d.ep.WriteStrings(0x409, []string{"S7 Camera Transport"}); err != nil {
		return d, err
	}
	if d.rx, err = functionfs.OpenAsyncEndpoint(mount, 1, true, 1, 512); err != nil {
		return d, err
	}
	if d.tx, err = functionfs.OpenAsyncEndpoint(mount, 2, false, 3, camerawire.MaxFrame+camerawire.Size+1); err != nil {
		return d, err
	}
	if err = c.AddFunction(FunctionName); err != nil {
		return d, err
	}
	d.done = make(chan struct{})
	go d.runEP0(ctx)
	return d, nil
}

func (d *Device) runEP0(ctx context.Context) {
	defer close(d.done)
	for ctx.Err() == nil {
		e, err := d.ep.ReadEvent()
		if functionfs.RetryEventError(err) {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				d.epError <- err
			}
			return
		}
		if e.Type == functionfs.EventSetup {
			d.ep.Stall(e.Setup.RequestType&0x80 != 0)
			continue
		}
		select {
		case d.events <- e.Type:
		case <-ctx.Done():
			return
		}
	}
}

func (d *Device) reap() error {
	completed, err := d.tx.Reap(0)
	if err != nil {
		return err
	}
	for _, v := range completed {
		if len(d.order) == 0 || d.order[0] != v.Slot {
			return errors.Join(camera.ErrOwnership, fmt.Errorf("camera bulk completion order changed"))
		}
		q := &d.queued[v.Slot]
		if v.Err != nil {
			return v.Err
		}
		if q.submitted == 0 || v.Bytes != q.submitted {
			return io.ErrShortWrite
		}
		q.offset += v.Bytes
		q.submitted = 0
		if q.offset == q.bytes {
			if q.frame {
				d.stats.Outstanding--
				d.stats.CompletedFrames++
			}
			d.queued[v.Slot] = queued{}
			d.order = d.order[1:]
		}
	}
	if d.reply != nil {
		if err = d.send(*d.reply, nil); errors.Is(err, syscall.EAGAIN) {
			return d.pump()
		}
		if err != nil {
			return err
		}
		d.reply = nil
	}
	return d.pump()
}

func (d *Device) pump() error {
	if len(d.order) == 0 {
		return nil
	}
	i := d.order[0]
	q := &d.queued[i]
	if q.submitted != 0 {
		return nil
	}
	// f_fs.c allocates a kernel bounce buffer for each request. Bound it to
	// 64 KiB instead of requesting a multi-megabyte contiguous allocation.
	n := min(q.bytes-q.offset, 64<<10)
	err := d.tx.SubmitSlice(i, q.offset, n)
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
		return nil
	}
	if err != nil {
		return err
	}
	q.submitted = n
	return nil
}

func (d *Device) send(h camerawire.Header, payload []byte) error {
	header, err := h.Marshal()
	if err != nil {
		return err
	}
	if len(payload) != int(h.Bytes) {
		return camerawire.ErrWire
	}
	for i := range d.queued {
		if d.queued[i].bytes != 0 {
			continue
		}
		b, e := d.tx.Buffer(i)
		if errors.Is(e, syscall.EAGAIN) {
			continue
		}
		if e != nil {
			return e
		}
		copy(b, header[:])
		copy(b[camerawire.Size:], payload)
		n := h.PacketBytes()
		if n > camerawire.Size+len(payload) {
			b[n-1] = 0
		}
		d.queued[i] = queued{bytes: n, frame: h.Kind == camerawire.Frame}
		d.order = append(d.order, i)
		if e = d.pump(); e != nil {
			d.order = d.order[:len(d.order)-1]
			d.queued[i] = queued{}
			return e
		}
		return nil
	}
	return syscall.EAGAIN
}

func (d *Device) Events(emit func(uint32) error) (result error) {
	defer func() {
		if result != nil {
			result = d.transferFailure(result, emit)
		}
	}()
	if d.closed {
		return os.ErrClosed
	}
	select {
	case err := <-d.epError:
		return err
	default:
	}
	for {
		select {
		case event := <-d.events:
			switch event {
			case functionfs.EventEnable:
				d.enabled = true
			case functionfs.EventDisable, functionfs.EventUnbind:
				d.enabled = false
				d.wanted = false
				if err := d.Stop(); err != nil {
					return err
				}
				if err := d.rx.Cancel(); err != nil {
					return err
				}
				if err := emit(uvcout.EventDisconnect); err != nil {
					return err
				}
			}
		default:
			goto drained
		}
	}
drained:
	if !d.enabled {
		return nil
	}
	if err := d.reap(); err != nil {
		return d.transferFailure(err, emit)
	}
	completed, err := d.rx.Reap(0)
	if err != nil {
		return err
	}
	for _, v := range completed {
		if v.Err != nil {
			return d.transferFailure(v.Err, emit)
		}
		b, e := d.rx.Buffer(v.Slot)
		if e != nil {
			return e
		}
		command, e := camerawire.ParseCommand(b[:v.Bytes])
		if e != nil {
			continue
		}
		if command.Kind == camerawire.Stop {
			if command.Session == d.session && command.Token == d.token {
				d.wanted = false
				if e = emit(uvcout.EventStreamOff); e != nil {
					return e
				}
			}
			continue
		}
		if e = d.authorize(command); e != nil {
			d.reply = &camerawire.Header{Kind: camerawire.Failure, Session: command.Session, Error: 1}
			continue
		}
		if d.wanted {
			d.wanted = false
			if e = emit(uvcout.EventStreamOff); e != nil {
				return e
			}
		}
		if e = d.Stop(); e != nil {
			return e
		}
		d.token, d.session, d.sequence = command.Token, command.Session, 0
		d.mode = camera.Mode(command.Mode)
		d.wanted = true
		d.reply = &camerawire.Header{Kind: camerawire.Ack, Session: d.session, Mode: command.Mode}
		if e = emit(uvcout.EventStreamOn); e != nil {
			return e
		}
	}
	if d.wanted {
		mode, modeErr := d.currentMode(d.token)
		if modeErr != nil {
			d.wanted = false
			if err = emit(uvcout.EventStreamOff); err != nil {
				return err
			}
		} else {
			d.mode = mode
		}
	}
	if d.rx.Pending() == 0 {
		if err = d.rx.Submit(0, 512); err != nil && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
	return d.reap()
}

func (d *Device) transferFailure(err error, emit func(uint32) error) error {
	if errors.Is(err, camera.ErrOwnership) || errors.Is(err, functionfs.ErrAsyncOwnership) {
		return err
	}
	if !errors.Is(err, syscall.ENODEV) && !errors.Is(err, syscall.ESHUTDOWN) && !errors.Is(err, syscall.ECONNRESET) {
		return err
	}
	d.wanted = false
	if e := d.Stop(); e != nil {
		return e
	}
	if e := d.rx.Cancel(); e != nil {
		return errors.Join(camera.ErrOwnership, e)
	}
	// A host pipe abort need not unconfigure the whole gadget. Keep the control
	// listener available; only EP0 Disable/Unbind changes d.enabled.
	return emit(uvcout.EventStreamOff)
}

func (d *Device) Mode() (camera.Mode, error) {
	if !d.wanted {
		return camera.Mode{}, fmt.Errorf("no active WinUSB camera request")
	}
	return d.mode, nil
}
func (d *Device) Start(m camera.Mode) error {
	if !d.enabled || !d.wanted || m != d.mode {
		return fmt.Errorf("camera transport start differs from owner request")
	}
	d.on = true
	d.havePTS = false
	return nil
}
func (d *Device) Streaming(m camera.Mode) bool { return d.on && d.wanted && d.enabled && m == d.mode }

// Keep already queued AUs intact. Mark the new encoder's first IDR rather than
// cancelling USB halfway through an old packet on a local setting change.
func (d *Device) BeginCapture(m camera.Mode) error {
	if !d.Streaming(m) {
		return fmt.Errorf("camera stream not ready for encoder boundary")
	}
	d.boundary = true
	return nil
}
func (d *Device) Stats() uvcout.Stats { return d.stats }
func (d *Device) Submit(p media.Encoded) error {
	if !d.Streaming(d.mode) {
		return fmt.Errorf("WinUSB camera stream stopped")
	}
	if err := d.reap(); err != nil {
		return err
	}
	if len(p.Data) < 4 || len(p.Data) > camerawire.MaxFrame || p.PTS < 0 || (d.havePTS && p.PTS <= d.lastPTS) || d.sequence == ^uint64(0) {
		return camerawire.ErrWire
	}
	h := camerawire.Header{Kind: camerawire.Frame, Session: d.session, Sequence: d.sequence + 1, PTS: p.PTS,
		Mode: cameramode.Mode(d.mode), Bytes: uint32(len(p.Data)), CRC: crc32.ChecksumIEEE(p.Data)}
	if p.Key {
		h.Flags = camerawire.KeyFrame
	}
	if d.boundary {
		if !p.Key {
			return fmt.Errorf("new encoder segment must begin with IDR")
		}
		h.Flags |= camerawire.Discontinuity
	}
	if err := d.send(h, p.Data); err != nil {
		return err
	}
	d.sequence++
	d.lastPTS = p.PTS
	d.havePTS = true
	d.boundary = false
	d.stats.QueuedFrames++
	d.stats.QueuedBytes += uint64(len(p.Data))
	d.stats.Outstanding++
	return nil
}
func (d *Device) Stop() error {
	if d.tx == nil {
		return nil
	}
	if err := d.tx.Cancel(); err != nil {
		return errors.Join(camera.ErrOwnership, err)
	}
	d.stats.DiscardedOnStop += uint64(d.stats.Outstanding)
	d.stats.Outstanding = 0
	d.queued = [3]queued{}
	d.order = nil
	d.reply = nil
	d.on = false
	d.havePTS = false
	return nil
}
func (d *Device) Close() error {
	if d == nil || d.closed {
		return nil
	}
	if err := d.Stop(); err != nil {
		return err
	}
	if d.rx != nil {
		if err := d.rx.Close(); err != nil {
			return errors.Join(camera.ErrOwnership, err)
		}
	}
	if d.tx != nil {
		if err := d.tx.Close(); err != nil {
			return errors.Join(camera.ErrOwnership, err)
		}
	}
	d.cancel()
	if d.ep != nil {
		if err := d.ep.Close(); err != nil {
			return err
		}
	}
	if d.done != nil {
		select {
		case <-d.done:
		case <-time.After(time.Second):
			return errors.Join(camera.ErrOwnership, fmt.Errorf("camera control task still active"))
		}
	}
	if d.unmount != nil {
		if err := d.unmount(); err != nil {
			return err
		}
	}
	d.closed = true
	return nil
}

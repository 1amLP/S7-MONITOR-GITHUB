//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"perimode/native/internal/installermsd"
	"perimode/native/pkg/usb/configfs"
	"perimode/native/pkg/usb/functionfs"
)

type installerDisk struct {
	g         *configfs.Gadget
	ep        *functionfs.Ep0
	out, in   *functionfs.Endpoint
	image     *os.File
	unmount   func() error
	cancel    context.CancelFunc
	done      chan struct{}
	eventDone chan struct{}
	serialTTY string
	mu        sync.Mutex
	scsi      *installermsd.Disk
	epoch     atomic.Uint64
	enabled   atomic.Bool
	onError   func(error)
	onEject   func()
}

func openInstallerDisk(parent context.Context, image *os.File, size int64, onError func(error), onEject func()) (d *installerDisk, err error) {
	d = &installerDisk{image: image, onError: onError, onEject: onEject}
	defer func() {
		if err != nil {
			err = errors.Join(err, d.close())
		}
	}()
	d.scsi, err = installermsd.New(size, PinnedSerial)
	if err != nil {
		return d, err
	}
	d.g = configfs.NewGadget("s7setup")
	d.g.Path = "/config/usb_gadget/s7setup"
	if err = d.g.Create(); err != nil {
		d.g = nil
		return d, err
	}
	for _, fn := range []func() error{
		func() error { return d.g.SetVendor(0x04e8) }, func() error { return d.g.SetProduct(0xa7c3) },
		func() error { return d.g.SetDevice(0x0100) }, func() error { return d.g.SetUSB(0x0200) },
		func() error { return d.g.SetClass(0xef) }, func() error { return d.g.SetSubClass(2) }, func() error { return d.g.SetProtocol(1) },
	} {
		if err = fn(); err != nil {
			return d, err
		}
	}
	strings := d.g.Strings(0x409)
	if err = strings.Create(); err != nil {
		return d, err
	}
	if err = strings.SetManufacturer("S7"); err != nil {
		return d, err
	}
	if err = strings.SetProduct("S7 SETUP"); err != nil {
		return d, err
	}
	if err = strings.SetSerialNumber(PinnedSerial); err != nil {
		return d, err
	}
	c := d.g.Config("c.1")
	if err = c.Create(); err != nil {
		return d, err
	}
	if err = c.SetMaxPower(500); err != nil {
		return d, err
	}
	if _, err = d.g.CreateFunction("ffs.s7setupdisk"); err != nil {
		return d, err
	}
	d.unmount, err = functionfs.MountPrivate("s7setupdisk", "/dev/s7-setup-disk", "functionfs_s7")
	if err != nil {
		return d, err
	}
	d.ep, err = functionfs.NewEp0Nonblocking("/dev/s7-setup-disk")
	if err != nil {
		return d, err
	}
	inter := []byte{9, 4, 0, 0, 2, 8, 6, 0x50, 1}
	fs := [][]byte{inter, {7, 5, 1, 2, 64, 0, 0}, {7, 5, 0x81, 2, 64, 0, 0}}
	hs := [][]byte{inter, {7, 5, 1, 2, 0, 2, 0}, {7, 5, 0x81, 2, 0, 2, 0}}
	if err = d.ep.WriteDescriptors(fs, hs, nil); err != nil {
		return d, err
	}
	if err = d.ep.WriteStrings(0x409, []string{"S7 read-only installer"}); err != nil {
		return d, err
	}
	d.out, err = functionfs.OpenEndpointNonblocking("/dev/s7-setup-disk", 1, true)
	if err != nil {
		return d, err
	}
	d.in, err = functionfs.OpenEndpointNonblocking("/dev/s7-setup-disk", 2, false)
	if err != nil {
		return d, err
	}
	if err = c.AddFunction("ffs.s7setupdisk"); err != nil {
		return d, err
	}
	acm, e := d.g.CreateFunction("acm.setup")
	if e != nil {
		return d, e
	}
	port, e := read(acm.Path + "/port_num")
	if e != nil {
		return d, e
	}
	n, e := strconv.Atoi(port)
	if e != nil || n < 0 || n > 15 {
		return d, fmt.Errorf("invalid ACM port")
	}
	d.serialTTY = fmt.Sprintf("/dev/ttyGS%d", n)
	if err = c.AddFunction("acm.setup"); err != nil {
		return d, err
	}
	ctx, cancel := context.WithCancel(parent)
	d.cancel = cancel
	d.done, d.eventDone = make(chan struct{}), make(chan struct{})
	go d.events(ctx)
	go d.serve(ctx)
	return d, nil
}

func (d *installerDisk) events(ctx context.Context) {
	defer close(d.eventDone)
	for ctx.Err() == nil {
		e, err := d.ep.ReadEvent()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if functionfs.RetryEventError(err) {
				time.Sleep(2 * time.Millisecond)
				continue
			}
			d.onError(fmt.Errorf("installer EP0: %w", err))
			return
		}
		switch e.Type {
		case functionfs.EventEnable:
			d.enabled.Store(true)
		case functionfs.EventDisable, functionfs.EventUnbind:
			d.enabled.Store(false)
			d.epoch.Add(1)
		case functionfs.EventSetup:
			s := e.Setup
			switch {
			case s.RequestType == 0xa1 && s.Request == 0xfe && s.Value == 0 && s.Length == 1:
				_, err = d.ep.Write([]byte{0})
			case s.RequestType == 0x21 && s.Request == 0xff && s.Value == 0 && s.Length == 0:
				d.epoch.Add(1)
				d.mu.Lock()
				d.scsi.Reset()
				d.mu.Unlock()
				err = d.ep.ReadExactly(nil)
			default:
				d.ep.Stall(s.RequestType&0x80 != 0)
			}
			if err != nil && ctx.Err() == nil && !functionfs.RetryEventError(err) {
				d.onError(fmt.Errorf("installer setup: %w", err))
			}
		}
	}
}

func (d *installerDisk) serve(ctx context.Context) {
	defer close(d.done)
	var cbw [31]byte
	buffer := make([]byte, 64<<10)
	for ctx.Err() == nil {
		if !d.enabled.Load() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
				continue
			}
		}
		epoch := d.epoch.Load()
		valid := func() bool { return d.enabled.Load() && d.epoch.Load() == epoch }
		readPacket := func(b []byte) (int, error) { return installerPacket(ctx, valid, d.out.ReadPacket, b) }
		writePacket := func(b []byte) (int, error) { return installerPacket(ctx, valid, d.in.WritePacket, b) }
		n, err := readPacket(cbw[:])
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, errInstallerEpoch) || functionfs.RetryEventError(err) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			d.onError(fmt.Errorf("installer CBW: %w", err))
			return
		}
		if !valid() {
			continue
		}
		c, err := installermsd.ParseCBW(cbw[:n])
		if err != nil {
			d.onError(err)
			return
		}
		d.mu.Lock()
		r := d.scsi.Execute(c)
		d.mu.Unlock()
		var sent uint32
		if c.In {
			if r.ReadBytes != 0 {
				for sent < r.ReadBytes && err == nil {
					part := buffer[:min(len(buffer), int(r.ReadBytes-sent))]
					_, err = d.image.ReadAt(part, r.Offset+int64(sent))
					if err == nil {
						n, err = writePacket(part)
						if err == nil && n != len(part) {
							err = io.ErrShortWrite
						}
						sent += uint32(n)
					}
				}
			} else if len(r.Data) != 0 {
				n, err = writePacket(r.Data)
				sent = uint32(n)
				if err == nil && n != len(r.Data) {
					err = io.ErrShortWrite
				}
			}
			// End a short IN data stage before sending the distinct CSW transfer.
			if err == nil && sent < c.Length && sent%512 == 0 {
				_, err = writePacket(nil)
			}
		} else {
			// Drain rejected write data into a bounded scratch buffer. The image
			// is opened O_RDONLY and no command has any write-to-file path.
			var received uint32
			for received < c.Length && err == nil {
				part := buffer[:min(len(buffer), int(c.Length-received))]
				n, err = readPacket(part)
				received += uint32(n)
				if err == nil && n == 0 {
					err = io.ErrUnexpectedEOF
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		if !valid() || errors.Is(err, errInstallerEpoch) {
			continue
		}
		if err != nil {
			d.onError(fmt.Errorf("installer data: %w", err))
			return
		}
		csw := installermsd.CSW(c, sent, r.Status)
		n, err = writePacket(csw[:])
		if ctx.Err() != nil {
			return
		}
		if d.epoch.Load() != epoch || errors.Is(err, errInstallerEpoch) {
			continue
		}
		if err != nil || n != len(csw) {
			d.onError(fmt.Errorf("installer status: %d %v", n, err))
			return
		}
		if r.Eject {
			d.onEject()
			return
		}
	}
}

func (d *installerDisk) close() error {
	if d == nil {
		return nil
	}
	if d.g != nil {
		v, e := d.g.GetUDC()
		if e != nil {
			return e
		}
		if v != "" {
			if e = d.g.SetUDC(""); e != nil {
				return e
			}
		}
	}
	if d.cancel != nil {
		d.cancel()
	}
	// UDC unbind returns every queued request before its owner is released.
	if d.out != nil {
		_ = d.out.Close()
	}
	if d.in != nil {
		_ = d.in.Close()
	}
	if d.ep != nil {
		_ = d.ep.Close()
	}
	for _, done := range []chan struct{}{d.done, d.eventDone} {
		if done != nil {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				return fmt.Errorf("installer IO still owns resources")
			}
		}
	}
	if d.image != nil {
		if err := d.image.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			return err
		}
		d.image = nil
	}
	if d.unmount != nil {
		if err := d.unmount(); err != nil {
			return err
		}
		d.unmount = nil
	}
	if d.g != nil {
		if err := d.g.Delete(); err != nil {
			return err
		}
		d.g = nil
	}
	return nil
}

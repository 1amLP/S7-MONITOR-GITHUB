//go:build linux

package appliance

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"sync/atomic"
	"syscall"
	"time"

	"perimode/native/pkg/usb/configfs"
	"perimode/native/pkg/usb/functionfs"
)

const debugInterfaceGUID = "{79D1B2BC-AB1A-4AB1-AE69-6FAB83AC15B0}"

// The pinned Samsung FunctionFS enables/disables endpoints with a do/while.
// A zero-endpoint function dereferences invalid memory in that kernel.
// Protocol 73 carries requests and large snapshots on bulk, not shared EP0.
func debugUSBDescriptors() (fs, hs [][]byte) {
	inter := []byte{9, 4, 0, 0, 2, 0xff, 0x53, 0x73, 1}
	return [][]byte{inter, {7, 5, 0x01, 2, 64, 0, 0}, {7, 5, 0x81, 2, 64, 0, 0}},
		[][]byte{inter, {7, 5, 0x01, 2, 0, 2, 0}, {7, 5, 0x81, 2, 0, 2, 0}}
}

type debugUSB struct {
	packageData           *packageDelivery
	allowUI               bool
	ep                    *functionfs.Ep0
	ui                    *debugUI
	diag                  func() []byte
	cancel                context.CancelFunc
	done                  chan struct{}
	unmount               func() error
	input, output         *functionfs.Endpoint
	inputDone, outputDone chan struct{}
	jobs                  chan debugBulkJob
	busy                  atomic.Bool
}

type debugBulkJob struct {
	command debugCommand
	result  *debugResult
}

func parseDebugBulkCommand(packet []byte) (debugCommand, error) {
	if len(packet) == 24 && string(packet[:4]) == "UI01" && binary.LittleEndian.Uint32(packet[4:]) != 0 {
		c := debugCommand{Sequence: binary.LittleEndian.Uint32(packet[4:]), Kind: binary.LittleEndian.Uint32(packet[8:]), X: int32(binary.LittleEndian.Uint32(packet[12:])), Y: int32(binary.LittleEndian.Uint32(packet[16:])), Capture: binary.LittleEndian.Uint32(packet[20:])}
		if c.Kind == 10 && c.X == 0 && c.Y == 0 && c.Capture == 0 {
			return c, nil
		}
		if c.Kind == 11 && c.X >= 0 && c.Y > 0 && c.Y <= packageChunkBytes && c.Capture != 0 {
			return c, nil
		}
	}
	if len(packet) == 24 && string(packet[:4]) == "UI01" && binary.LittleEndian.Uint32(packet[8:]) == 9 &&
		binary.LittleEndian.Uint32(packet[4:]) != 0 && binary.LittleEndian.Uint64(packet[12:20]) == 0 && binary.LittleEndian.Uint32(packet[20:]) == 0 {
		return debugCommand{Sequence: binary.LittleEndian.Uint32(packet[4:]), Kind: 9}, nil
	}
	return parseDebugCommand(packet)
}

func debugErrorResult(seq uint32, err error) debugResult {
	meta, _ := json.Marshal(map[string]string{"error": err.Error()})
	return debugResult{sequence: seq, status: 3, meta: meta}
}

func debugWireHeader(r debugResult) []byte {
	h := make([]byte, 32)
	copy(h, "UI01")
	for i, v := range []uint32{r.sequence, r.status, uint32(len(r.meta)), uint32(len(r.pixels))} {
		binary.LittleEndian.PutUint32(h[4+i*4:], v)
	}
	checksum := crc32.Update(0, crc32.IEEETable, r.meta)
	checksum = crc32.Update(checksum, crc32.IEEETable, r.pixels)
	binary.LittleEndian.PutUint32(h[20:], checksum)
	binary.LittleEndian.PutUint32(h[24:], 0x31435243) // CRC1: metadata + pixels, in wire order.
	return h
}

func openDebugUSB(parent context.Context, g *configfs.Gadget, c *configfs.Config, ui *debugUI, diag func() []byte) (out *debugUSB, err error) {
	ctx, cancel := context.WithCancel(parent)
	d := &debugUSB{ui: ui, diag: diag, cancel: cancel, allowUI: ui != nil}
	if d.ui == nil {
		d.ui = newDebugUI()
	}
	d.packageData, err = openPackageDelivery(packagePinPath, packagePayloadPath)
	if err != nil {
		log.Printf("Windows package unavailable: %v", err)
		err = nil
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, d.close())
		}
	}()
	if _, err = g.CreateFunction("ffs.s7diagnostics"); err != nil {
		return nil, err
	}
	if d.unmount, err = functionfs.MountPrivate("s7diagnostics", "/dev/s7-diagnostics", "functionfs_s7"); err != nil {
		return nil, err
	}
	if d.ep, err = functionfs.NewEp0Nonblocking("/dev/s7-diagnostics"); err != nil {
		return nil, err
	}
	fs, hs := debugUSBDescriptors()
	if err = d.ep.WriteWinUSBDescriptors(fs, hs, debugInterfaceGUID); err != nil {
		return nil, err
	}
	if err = d.ep.WriteStrings(0x409, []string{"S7 Native Diagnostics"}); err != nil {
		return nil, err
	}
	if d.input, err = functionfs.OpenEndpointNonblocking("/dev/s7-diagnostics", 1, true); err != nil {
		return nil, err
	}
	if d.output, err = functionfs.OpenEndpointNonblocking("/dev/s7-diagnostics", 2, false); err != nil {
		return nil, err
	}
	if err = c.AddFunction("ffs.s7diagnostics"); err != nil {
		return nil, err
	}
	d.done = make(chan struct{})
	d.inputDone, d.outputDone = make(chan struct{}), make(chan struct{})
	d.jobs = make(chan debugBulkJob, 1)
	go d.run(ctx)
	go d.readBulk(ctx)
	go d.writeBulk(ctx)
	return d, nil
}

func (d *debugUSB) run(ctx context.Context) {
	defer close(d.done)
	for ctx.Err() == nil {
		e, err := d.ep.ReadEvent()
		if functionfs.RetryEventError(err) {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("diagnostic EP0 stopped: %v", err)
			}
			return
		}
		switch e.Type {
		case functionfs.EventDisable, functionfs.EventUnbind:
			d.ui.disconnect()
		case functionfs.EventSetup:
			// Protocol 73 sends all inspection traffic on bulk, never shared EP0.
			d.ep.Stall(e.Setup.RequestType&0x80 != 0)
		}
	}
}

func (d *debugUSB) readBulk(ctx context.Context) {
	defer close(d.inputDone)
	var packet [512]byte
	for ctx.Err() == nil {
		n, err := d.input.Read(packet[:])
		if functionfs.RetryEventError(err) || errors.Is(err, syscall.ENODEV) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("diagnostic OUT stopped: %v", err)
			}
			return
		}
		if n != 24 {
			continue
		}
		seq := binary.LittleEndian.Uint32(packet[4:])
		d.ui.mu.Lock()
		epoch := d.ui.epoch
		d.ui.mu.Unlock()
		c, parseErr := parseDebugBulkCommand(packet[:n])
		if parseErr != nil {
			continue
		}
		c.Epoch = epoch
		if !d.busy.CompareAndSwap(false, true) {
			// The old IN transfer may be abandoned forever. The independent OUT
			// reader must still deliver an explicit Recovery to the UI owner.
			if c.Kind == debugRecovery && d.allowUI {
				select {
				case d.ui.requests <- c:
				default:
				}
			}
			continue
		}
		job := debugBulkJob{command: c}
		if c.Kind == 10 || c.Kind == 11 {
			r := d.packageData.response(c)
			job.result = &r
		} else if !d.allowUI {
			r := debugErrorResult(seq, fmt.Errorf("UI diagnostics disabled"))
			job.result = &r
		} else if c.Kind == 9 {
			meta := d.diag()
			if len(meta) > 256*1024 {
				meta = []byte(`{"error":"diagnostics too large"}`)
			}
			r := debugResult{sequence: seq, status: 2, meta: meta}
			job.result = &r
		} else if err := d.ui.submit(packet[:n], time.Now()); err != nil {
			r := debugErrorResult(seq, err)
			job.result = &r
		}
		select {
		case d.jobs <- job:
		case <-ctx.Done():
			d.busy.Store(false)
			return
		}
	}
}

func (d *debugUSB) writeBulk(ctx context.Context) {
	defer close(d.outputDone)
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-d.jobs:
			func() {
				defer d.busy.Store(false)
				r := job.result
				until := time.Now().Add(10 * time.Second)
				for r == nil && ctx.Err() == nil {
					d.ui.mu.Lock()
					epoch, value := d.ui.epoch, d.ui.result
					d.ui.mu.Unlock()
					if epoch != job.command.Epoch {
						return
					}
					if value.sequence == job.command.Sequence && value.status >= 2 {
						r = &value
						break
					}
					if time.Now().After(until) {
						value = debugErrorResult(job.command.Sequence, fmt.Errorf("UI command deadline"))
						r = &value
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				if r == nil {
					return
				}
				sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				for _, part := range [][]byte{debugWireHeader(*r), r.meta, r.pixels} {
					if err := writeDebugBytes(sendCtx, d.output, part); err != nil {
						if ctx.Err() == nil {
							log.Printf("diagnostic IN: %v", err)
						}
						return
					}
				}
			}()
		}
	}
}

func writeDebugBytes(ctx context.Context, w io.Writer, data []byte) error {
	for len(data) > 0 && ctx.Err() == nil {
		part := data[:min(len(data), 64<<10)]
		n, err := w.Write(part)
		if n == 0 && (errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR)) {
			time.Sleep(time.Millisecond)
			continue
		}
		if err != nil {
			return err
		}
		if n != len(part) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return ctx.Err()
}

func (d *debugUSB) close() error {
	if d == nil {
		return nil
	}
	d.cancel()
	if d.input != nil {
		_ = d.input.Close()
	}
	if d.output != nil {
		_ = d.output.Close()
	}
	if d.ep != nil {
		_ = d.ep.Close()
	}
	if d.done != nil {
		select {
		case <-d.done:
		case <-time.After(2 * time.Second):
			return fmt.Errorf("diagnostic EP0 still pending")
		}
	}
	for _, done := range []chan struct{}{d.inputDone, d.outputDone} {
		if done != nil {
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				return fmt.Errorf("diagnostic bulk still pending")
			}
		}
	}
	if d.unmount != nil {
		if err := d.unmount(); err != nil {
			return err
		}
	}
	if d.packageData != nil {
		return d.packageData.file.Close()
	}
	return nil
}

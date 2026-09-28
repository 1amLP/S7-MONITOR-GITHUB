//go:build linux

package appliance

import (
	"context"
	"errors"
	"fmt"
	"perimode/native/internal/camera"
	"perimode/native/internal/camerausb"
	"perimode/native/internal/linuxio"
	"perimode/native/pkg/monitor"
	"perimode/native/pkg/usb/configfs"
	"perimode/native/pkg/usb/functionfs"
	"perimode/native/pkg/usb/uvcmode"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	gadget       = "/config/usb_gadget/s7native"
	config       = gadget + "/configs/c.1"
	controller   = "15400000.dwc3"
	monitorMount = "/dev/s7-monitor"
)

func read(p string) (string, error) { s, e := linuxio.ReadText(p); return strings.TrimSpace(s), e }
func writeAttr(p, v string) error   { return linuxio.WriteAttr(p, v+"\n") }

type Transport struct {
	cameraFunction          *configfs.UVCFunction
	cameraPrivate           *camerausb.Device
	cameraTable             uvcmode.Table
	cameraProvisionErr      error
	cameraProvider          camera.Provider
	cameraTask              *cameraTask
	cameraVC, nextInterface uint8
	mu                      sync.Mutex
	g                       *configfs.Gadget
	c                       *configfs.Config
	ep0                     *functionfs.Ep0
	touch                   *touchUSB
	unmount                 func() error
	ctx                     context.Context
	cancel                  context.CancelFunc
	eventsDone              chan struct{}
	receiverCancel          context.CancelFunc
	receiverDone            chan struct{}
	state                   *State
	diag                    func() []byte
	diagCache               []byte
	debug                   *debugUSB
	osLink                  bool
	audio                   *configfs.AudioFunction
	audioProvisionErr       error
	bound                   bool
	boundView               atomic.Bool
	closed                  bool
}

func NewTransport(parent context.Context, s *State, serial string, diag func() []byte, debug *debugUI) (t *Transport, err error) {
	// No legacy Android gadget takeover; a native boot starts with none bound.
	entries, e := filepath.Glob("/config/usb_gadget/*/UDC")
	if e != nil {
		return nil, e
	}
	for _, p := range entries {
		v, e := read(p)
		if e != nil || v != "" {
			return nil, fmt.Errorf("foreign/busy gadget %s: %s %v", p, v, e)
		}
	}
	if _, e = os.Stat("/sys/class/udc/" + controller); e != nil {
		return nil, fmt.Errorf("pinned S7 controller missing: %w", e)
	}
	ctx, cancel := context.WithCancel(parent)
	t = &Transport{ctx: ctx, cancel: cancel, state: s, diag: diag, nextInterface: 3}
	owned := t
	defer func() {
		if err != nil {
			cancel()
			err = errors.Join(err, owned.Close())
		}
	}()
	t.g = configfs.NewGadget("s7native")
	t.g.Path = gadget
	if err = t.g.Create(); err != nil {
		t.g = nil
		return nil, err
	}
	// A7C0 belongs to the old Android layout, whose MI_00 is ADB. Reusing it
	// makes Windows retain ADB DeviceInterfaceGUIDs on the native monitor.
	for _, fn := range []func() error{func() error { return t.g.SetVendor(0x04e8) }, func() error { return t.g.SetProduct(0xa7c1) }, func() error { return t.g.SetDevice(0x0403) }, func() error { return t.g.SetUSB(0x0200) }, func() error { return t.g.SetClass(0) }, func() error { return t.g.SetSubClass(0) }, func() error { return t.g.SetProtocol(0) }} {
		if err = fn(); err != nil {
			return nil, err
		}
	}
	st := t.g.Strings(0x409)
	if err = st.Create(); err != nil {
		return nil, err
	}
	for _, fn := range []func() error{func() error { return st.SetManufacturer("S7 native laboratory") }, func() error { return st.SetProduct("S7 Native H264 - EXPERIMENTAL") }, func() error { return st.SetSerialNumber(serial) }} {
		if err = fn(); err != nil {
			return nil, err
		}
	}
	t.c = t.g.Config("c.1")
	if err = t.c.Create(); err != nil {
		return nil, err
	}
	if err = t.c.SetMaxPower(500); err != nil {
		return nil, err
	}
	if _, err = t.g.CreateFunction("ffs.s7monitor"); err != nil {
		return nil, err
	}
	if t.unmount, err = functionfs.MountPrivate("s7monitor", monitorMount, "functionfs_s7"); err != nil {
		return nil, err
	}
	if t.ep0, err = functionfs.NewEp0Nonblocking(monitorMount); err != nil {
		return nil, err
	}
	fs, hs := monitor.USBDescriptors()
	if err = t.ep0.WriteWinUSBDescriptors(fs, hs, monitor.InterfaceGUID); err != nil {
		return nil, err
	}
	if err = t.ep0.WriteStrings(0x409, []string{"S7 native H264 receiver"}); err != nil {
		return nil, err
	}
	if err = t.c.AddFunction("ffs.s7monitor"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	pollingHz := s.PadPollingHz
	s.mu.Unlock()
	if t.touch, err = newTouchUSBRate(pollingHz); err != nil {
		return nil, err
	}
	s.cameraUSBRemoved()
	// Keep the composite descriptor stable for the entire boot. Audio streams
	// stay silent/off in State until selected, but their interfaces must exist
	// before the first UDC bind or enabling audio would tear down the monitor.
	if e = t.createAudioLocked(); e != nil {
		t.audioProvisionErr = e
		s.mu.Lock()
		s.AudioError = "USB AUDIO ENDPOINTS UNAVAILABLE: " + e.Error()
		s.mu.Unlock()
	}
	trialFPS := s.USBTrialFPS
	var cameraTable uvcmode.Table
	if trialFPS != 0 {
		cameraTable, e = uvcmode.NewUSBTrial(trialFPS)
	} else {
		cameraTable, e = uvcmode.NewPublic()
	}
	if e != nil {
		return nil, e
	}
	if e = t.provisionCameraLocked(cameraTable); e != nil {
		t.cameraProvisionErr = e
		s.cameraStatus("USB CAMERA ENDPOINTS UNAVAILABLE", e)
	} else {
		s.cameraPublishedTable(camera.InspectModes(nil, s.CameraCurrent().Settings), cameraTable)
	}
	// Append diagnostics last: monitor, touch, audio and camera keep their indices.
	{
		var debugErr error
		t.debug, debugErr = openDebugUSB(ctx, t.g, t.c, debug, diag)
		if debugErr != nil {
			s.Error(fmt.Errorf("diagnostic USB setup: %w", debugErr))
		}
		if t.debug != nil && t.debug.packageData != nil {
			s.mu.Lock()
			s.PackageRelease = t.debug.packageData.pin.Release
			s.mu.Unlock()
		}
	}
	for _, p := range [][2]string{{"use", "0"}, {"b_vendor_code", "0x40"}, {"qw_sign", "MSFT100"}} {
		if err = writeAttr(gadget+"/os_desc/"+p[0], p[1]); err != nil {
			return nil, err
		}
	}
	if err = os.Symlink(config, gadget+"/os_desc/c.1"); err != nil {
		return nil, err
	}
	t.osLink = true
	if err = writeAttr(gadget+"/os_desc/use", "1"); err != nil {
		return nil, err
	}
	t.eventsDone = make(chan struct{})
	t.touch.webcam = s.CameraPreference
	t.touch.cameraStatus = s.CameraControlStatus
	t.touch.cameraCommand = s.CameraControlCommand
	t.touch.cameraPropertyStatus = s.cameraPropertyStatus
	t.touch.cameraPropertyCommand = s.cameraPropertyCommand
	t.touch.cameraDisconnected = s.CameraControlDisconnected
	t.touch.endpointStatus = s.EndpointConfiguration
	t.touch.endpointAck = s.EndpointAcknowledgement
	t.touch.sniperState = s
	t.touch.startNative(ctx)
	go t.events()
	s.setLinkAvailable()
	return t, nil
}
func (t *Transport) Bound() bool { return t.boundView.Load() }
func (t *Transport) Toggle() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return os.ErrClosed
	}
	if t.bound {
		if e := t.stopCameraLocked(); e != nil {
			return e
		}
		t.touch.blockInput()
		t.touch.blockVolume()
		if e := t.g.SetUDC(""); e != nil {
			t.touch.mu.Lock()
			t.touch.volumeSuspended = false
			t.touch.volumeConnection(t.touch.enabled)
			t.touch.mu.Unlock()
			return e
		}
		t.bound = false
		t.boundView.Store(false)
		t.state.setLinkBound(false)
		t.state.Disconnect()
		return nil
	}
	if t.receiverDone != nil {
		select {
		case <-t.receiverDone:
		default:
			return fmt.Errorf("previous USB read still pending; refusing rapid rebind")
		}
	}
	if e := t.g.SetUDC(controller); e != nil {
		return e
	}
	t.bound = true
	t.boundView.Store(true)
	t.state.setLinkBound(true)
	t.startCameraLocked()
	return nil
}
func (t *Transport) stopReceiver() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.receiverCancel != nil {
		t.receiverCancel()
	}
	t.state.Disconnect()
}
func (t *Transport) startReceiver() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.ctx.Err() != nil {
		return
	}
	previous := t.receiverDone
	if t.receiverCancel != nil {
		t.receiverCancel()
	}
	ctx, cancel := context.WithCancel(t.ctx)
	t.receiverCancel = cancel
	t.receiverDone = make(chan struct{})
	done := t.receiverDone
	go func() {
		defer close(done)
		// ENABLE can follow DISABLE before the cancelled read has unwound.
		// Keep a serial ownership chain without blocking EP0 or losing ENABLE.
		if previous != nil {
			<-previous
		}
		if ctx.Err() != nil {
			return
		}
		ep, e := functionfs.OpenEndpointNonblocking(monitorMount, 1, true)
		if e != nil {
			t.state.Error(e)
			return
		}
		defer ep.Close()
		stop := context.AfterFunc(ctx, func() { _ = ep.Close() })
		defer stop()
		parser := monitor.NewFramer(t.state.Publish, t.state.RejectTransportFrame)
		buf := make([]byte, 64<<10)
		for ctx.Err() == nil {
			n, e := ep.Read(buf)
			if n > 0 {
				if x := parser.Feed(buf[:n]); x != nil {
					t.state.Fault("monitor", x)
					t.state.Keyframe()
					parser = monitor.NewFramer(t.state.Publish, t.state.RejectTransportFrame)
				}
			}
			if e != nil {
				if ctx.Err() != nil {
					return
				}
				if functionfs.RetryEventError(e) || errors.Is(e, syscall.ENODEV) {
					time.Sleep(time.Millisecond)
					continue
				}
				t.state.Fault("usb", fmt.Errorf("USB receive: %w", e))
				log.Printf("S7 monitor bulk receiver stopped: %v", e)
				return
			}
			if n == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()
}
func (t *Transport) events() {
	defer close(t.eventsDone)
	for t.ctx.Err() == nil {
		e, err := t.ep0.ReadEvent()
		if err != nil {
			if functionfs.RetryEventError(err) {
				time.Sleep(time.Millisecond)
				continue
			}
			if t.ctx.Err() == nil {
				t.state.Fault("usb", fmt.Errorf("USB ep0: %w", err))
				log.Printf("S7 monitor EP0 stopped: %v", err)
			}
			return
		}
		switch e.Type {
		case functionfs.EventEnable:
			log.Print("S7 monitor USB enabled")
			t.state.linkUSBEvent(true, false, time.Now())
			t.startReceiver()
		case functionfs.EventDisable, functionfs.EventUnbind:
			log.Printf("S7 monitor USB event %d: receiver stop", e.Type)
			t.state.linkUSBEvent(false, false, time.Now())
			t.stopReceiver()
		case functionfs.EventSuspend:
			log.Print("S7 monitor USB suspended")
			t.state.linkUSBEvent(true, true, time.Now())
		case functionfs.EventResume:
			log.Print("S7 monitor USB resumed")
			t.state.linkUSBEvent(true, false, time.Now())
		case functionfs.EventSetup:
			t.control(e.Setup)
		}
	}
}
func (t *Transport) control(q functionfs.UsbCtrlRequest) {
	in := q.RequestType&0x80 != 0
	switch {
	case q.RequestType == 0xc1 && q.Index == 0 && q.Request == 0x55 && q.Length == 32:
		b := t.state.SniperControl(q.Value)
		if _, e := t.ep0.Write(b[:]); e != nil {
			t.state.Error(e)
		}
	case q.RequestType == 0xc1 && q.Index == 0 && q.Request == 0x51 && q.Value == 0 && q.Length == 48:
		b := t.state.Configuration()
		if _, e := t.ep0.Write(b[:]); e != nil {
			t.state.Error(e)
		}
	case q.RequestType == 0x41 && q.Index == 0 && q.Request == 0x52 && q.Value == 0 && q.Length == 12:
		var b [12]byte
		if e := t.ep0.ReadExactly(b[:]); e != nil {
			t.state.Error(e)
		} else {
			t.state.Error(t.state.HostStatus(b[:]))
		}
	case q.RequestType == 0xc1 && q.Index == 0 && q.Request == 0x54 && q.Length > 0 && q.Length <= 4096:
		// Read-only diagnostic snapshot pages, not an arbitrary command channel.
		if q.Value == 0 {
			t.diagCache = t.diag()
			if len(t.diagCache) > 256*1024 {
				t.diagCache = []byte(`{"error":"diagnostic snapshot too large"}`)
			}
		}
		b := t.diagCache
		start := int(q.Value) * 4096
		if start >= len(b) {
			_, _ = t.ep0.Write([]byte{32})
			return
		}
		end := min(len(b), start+int(q.Length))
		n, e := t.ep0.Write(b[start:end])
		if e == nil && n != end-start {
			e = io.ErrShortWrite
		}
		t.state.Error(e)
	default:
		t.ep0.Stall(in)
	}
}
func (t *Transport) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	t.cancel()
	t.mu.Lock()
	cameraErr := t.stopCameraLocked()
	t.mu.Unlock()
	if cameraErr != nil {
		return cameraErr
	}
	if t.touch != nil {
		t.touch.stop()
	}
	if t.g != nil {
		if current, e := t.g.GetUDC(); e == nil && current != "" {
			if e = t.g.SetUDC(""); e != nil {
				return e
			}
		}
	}
	t.stopReceiver()
	t.mu.Lock()
	receiverDone := t.receiverDone
	t.bound = false
	t.boundView.Store(false)
	t.state.setLinkBound(false)
	t.mu.Unlock()
	if receiverDone != nil {
		select {
		case <-receiverDone:
		case <-time.After(2 * time.Second):
			return fmt.Errorf("USB receiver stuck; resources retained until reboot")
		}
	}
	if t.ep0 != nil {
		_ = t.ep0.Close()
	}
	if t.eventsDone != nil {
		select {
		case <-t.eventsDone:
		case <-time.After(2 * time.Second):
			return fmt.Errorf("ep0 still pending")
		}
	}
	if t.touch != nil {
		if e := t.touch.closeAfterUnbind(); e != nil {
			return e
		}
	}
	if t.cameraFunction != nil {
		if e := os.Remove(config + "/" + configfs.UVCFunctionName); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if e := t.cameraFunction.Close(); e != nil {
			return e
		}
		t.cameraFunction = nil
	}
	if t.cameraPrivate != nil {
		if e := t.cameraPrivate.Close(); e != nil {
			return e
		}
		t.cameraPrivate = nil
	}
	if t.osLink {
		_ = writeAttr(gadget+"/os_desc/use", "0")
		if e := os.Remove(gadget + "/os_desc/c.1"); e != nil {
			return e
		}
	}
	if t.debug != nil {
		if e := t.debug.close(); e != nil {
			return e
		}
	}
	if t.unmount != nil {
		if e := t.unmount(); e != nil {
			return e
		}
	}
	if t.g != nil {
		return t.g.Delete()
	}
	return nil
}

func (t *Transport) createAudioLocked() error {
	if t.audio != nil {
		return nil
	}
	if t.bound {
		if t.audioProvisionErr != nil {
			return fmt.Errorf("audio endpoints unavailable at USB bind: %w", t.audioProvisionErr)
		}
		return fmt.Errorf("audio endpoints were not provisioned before USB bind")
	}
	a, err := t.g.CreateAudioFunction()
	if err != nil {
		return err
	}
	if err = t.c.AddFunction(configfs.AudioFunctionName); err != nil {
		return errors.Join(err, a.Close())
	}
	t.audio = a
	t.audioProvisionErr = nil
	t.nextInterface += 3
	return nil
}

// EnsureAudio only verifies or retries pre-bind provisioning. It never rebinds
// an active monitor; ordinary speaker/microphone toggles only start PCM workers.
func (t *Transport) EnsureAudio() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return os.ErrClosed
	}
	return t.createAudioLocked()
}
func (t *Transport) AudioProvisioned() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.closed && t.audio != nil
}
func (t *Transport) AudioActivity() (bool, bool, error) {
	t.mu.Lock()
	a, bound, closed := t.audio, t.bound, t.closed
	t.mu.Unlock()
	if a == nil || !bound || closed {
		return false, false, nil
	}
	return a.Activity()
}

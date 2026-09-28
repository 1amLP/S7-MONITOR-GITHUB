//go:build linux

package appliance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"perimode/native/pkg/cameractl"
	"perimode/native/pkg/cameraprop"
	"perimode/native/pkg/hid"
	"perimode/native/pkg/usb/functionfs"
)

const touchFunction = "ffs.s7touch"
const touchMount = "/dev/s7-touch"
const touchAddress = "127.0.0.1:39574"

// Volume keys keep the original 125 Hz service cadence independently of touch.
const volumeServiceInterval = 8 * time.Millisecond

type touchUSB struct {
	pollingHz                                   int
	pollingChanged                              chan struct{}
	motionChanged                               chan struct{}
	timing                                      TouchTimingSnapshot
	pendingTouch, lastTouch                     hid.TouchFrame
	havePendingTouch                            bool
	lastTouchAt                                 time.Time
	path, link                                  string
	linked                                      bool
	unmount                                     func() error
	ep0                                         *functionfs.Ep0
	input                                       [2]*os.File
	descriptor                                  [2][]byte
	controls                                    [2]*hid.DigitizerControl
	webcam                                      func() [16]byte
	cameraStatus                                func() [cameractl.ReportBytes]byte
	cameraCommand                               func([]byte) error
	cameraPropertyStatus                        func() [cameraprop.Size]byte
	cameraPropertyCommand                       func([]byte) error
	cameraDisconnected                          func()
	endpointStatus                              func() [16]byte
	endpointAck                                 func([]byte) error
	sniperState                                 *State
	sniper                                      sniperHIDState
	mu                                          sync.Mutex
	enabled                                     bool
	seen                                        [2]bool
	active                                      bool
	tracker                                     hid.ContactTracker
	gate                                        hid.ContactGate
	kind                                        byte
	cancel                                      context.CancelFunc
	done                                        chan struct{}
	eventDone                                   chan struct{}
	reports                                     chan touchReport
	writerDone                                  chan struct{}
	writerCancel                                context.CancelFunc
	write                                       func(int, []byte) error
	failures                                    chan<- error
	generation                                  uint64
	stopping                                    bool
	volume                                      hid.VolumeButtons
	volumeStatus                                PCVolumeStatus
	volumeNeutral, volumeSuspended, volumeFault bool
	volumeGeneration                            uint64
	volumeCurrent                               byte
	volumeRetry                                 time.Time
}

// Every queued report owns fixed-size storage until its kernel IO returns.
type touchReport struct {
	index       int
	data        [hid.DigitizerReportBytes]byte
	generation  uint64
	queuedAt    time.Time
	sniper      hid.SniperMessage
	sniperFinal bool
}

// These count FunctionFS writes, not host receipt or the physical sensor rate.
type TouchTimingSnapshot struct {
	SubmittedFrames uint64           `json:"submitted_frames"`
	DeferredFrames  uint64           `json:"deferred_frames"`
	CoalescedFrames uint64           `json:"coalesced_frames"`
	QueuedReports   uint64           `json:"queued_reports"`
	WrittenReports  uint64           `json:"completed_writes"`
	WriteErrors     uint64           `json:"write_errors"`
	StaleReports    uint64           `json:"stale_reports"`
	QueuePeak       int              `json:"queue_peak"`
	QueueWait       InputStageTiming `json:"queue_wait"`
	WriteTime       InputStageTiming `json:"write_time"`
}

func (t *touchUSB) touchTiming() TouchTimingSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.timing
}

func newTouchUSB() (t *touchUSB, err error) { return newTouchUSBRate(90) }
func newTouchUSBRate(hz int) (t *touchUSB, err error) {
	if !validPadPolling(hz) {
		return nil, fmt.Errorf("touchpad report limit is fixed at 90 Hz")
	}
	// Advertise the fastest endpoint once. The selected rate is enforced by the
	// bounded software scheduler, so changing it never re-enumerates USB.
	fsInterval, hsInterval, err := padUSBIntervals(1000)
	if err != nil {
		return nil, err
	}
	t = &touchUSB{pollingHz: hz, pollingChanged: make(chan struct{}, 1), path: filepath.Join(gadget, "functions", touchFunction), link: filepath.Join(config, touchFunction)}
	if current, e := read(gadget + "/UDC"); e != nil || current != "" {
		return nil, errors.New("touch creation requires unbound USB")
	}
	if err = os.Mkdir(t.path, 0755); err != nil {
		return nil, err
	}
	owned := t
	defer func() {
		if err != nil {
			err = errors.Join(err, owned.closeAfterUnbind())
		}
	}()
	if t.unmount, err = functionfs.MountPrivate("s7touch", touchMount, "functionfs_s7"); err != nil {
		return nil, err
	}
	if t.ep0, err = functionfs.NewEp0Nonblocking(touchMount); err != nil {
		return nil, err
	}
	var fs, hs [][]byte
	for i := 0; i < 2; i++ {
		t.descriptor[i], err = hid.DigitizerDescriptor(byte(i + 1))
		if err != nil {
			return nil, err
		}
		t.controls[i], _ = hid.NewDigitizerControl(byte(i + 1))
		if i == 0 {
			t.descriptor[i] = append(t.descriptor[i], hid.ConsumerVolumeDescriptor()...)
			t.descriptor[i] = append(t.descriptor[i], hid.EndpointStateDescriptor()...)
			t.descriptor[i] = append(t.descriptor[i], hid.SniperDescriptor()...)
		}
		inter := []byte{9, 4, byte(i), 0, 1, 3, 0, 0, byte(i + 1)}
		d := t.hidDescriptor(i)
		fi, hi := byte(1), byte(4)
		if i == 1 {
			fi, hi = fsInterval, hsInterval
		}
		fs = append(fs, inter, d, []byte{7, 5, byte(0x81 + i), 3, 64, 0, fi})
		hs = append(hs, inter, d, []byte{7, 5, byte(0x81 + i), 3, 64, 0, hi})
	}
	if err = t.ep0.WriteDescriptors(fs, hs, nil); err != nil {
		return nil, err
	}
	if err = t.ep0.WriteStrings(0x409, []string{"S7 Touchscreen", "S7 Precision Touchpad"}); err != nil {
		return nil, err
	}
	for i := range t.input {
		t.input[i], err = os.OpenFile(filepath.Join(touchMount, fmt.Sprintf("ep%d", i+1)), os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
	}
	if err = os.Symlink(t.path, t.link); err != nil {
		return nil, err
	}
	t.linked = true
	return t, nil
}

func (t *touchUSB) hidDescriptor(i int) []byte {
	return []byte{9, 0x21, 0x11, 1, 0, 1, 0x22, byte(len(t.descriptor[i])), byte(len(t.descriptor[i]) >> 8)}
}

func (t *touchUSB) status() (screen, pad, active bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.enabled && t.seen[0], t.enabled && t.seen[1] && t.controls[1].ContactsEnabled(), t.active
}

// Called under the state lock: only queue admission, never USB IO.
func (t *touchUSB) writeReport(index int, data []byte) error {
	if t.stopping || t.reports == nil {
		return context.Canceled
	}
	var report touchReport
	report.index = index
	report.generation = t.generation
	report.queuedAt = time.Now()
	if len(data) != len(report.data) {
		return errors.New("invalid HID report size")
	}
	copy(report.data[:], data)
	select {
	case t.reports <- report:
		t.timing.QueuedReports++
		t.timing.QueuePeak = max(t.timing.QueuePeak, len(t.reports))
		return nil
	default:
		t.gate.BlockUntilRelease()
		err := errors.New("touch output queue full; reset USB to release host contacts")
		t.reportFailure(err)
		return err
	}
}

// Never call a blocking callback while the touch state lock is held.
func (t *touchUSB) reportFailure(err error) {
	if t.failures != nil {
		select {
		case t.failures <- err:
		default:
		}
	}
}

func (t *touchUSB) startWriter(ctx context.Context) {
	ctx, t.writerCancel = context.WithCancel(ctx)
	t.reports = make(chan touchReport, 8)
	t.motionChanged = make(chan struct{}, 1)
	t.writerDone = make(chan struct{})
	write := t.write
	if write == nil {
		write = func(index int, data []byte) error {
			t.mu.Lock()
			generation, volumeGeneration := t.generation, t.volumeGeneration
			isVolume := len(data) == 2 && data[0] == hid.ConsumerVolumeReportID
			t.mu.Unlock()
			deadline := time.Now().Add(100 * time.Millisecond)
			for ctx.Err() == nil {
				if time.Now().After(deadline) {
					return fmt.Errorf("HID write deadline: %w", context.DeadlineExceeded)
				}
				t.mu.Lock()
				valid := t.enabled && !t.stopping && generation == t.generation
				if isVolume {
					valid = valid && !t.volumeSuspended && volumeGeneration == t.volumeGeneration
				}
				t.mu.Unlock()
				if !valid {
					return context.Canceled
				}
				n, e := syscall.Write(int(t.input[index].Fd()), data)
				if e == nil {
					if n != len(data) {
						return io.ErrShortWrite
					}
					return nil
				}
				if !errors.Is(e, syscall.EAGAIN) && !errors.Is(e, syscall.EINTR) {
					return e
				}
				time.Sleep(time.Millisecond)
			}
			return ctx.Err()
		}
	}
	go func() {
		defer close(t.writerDone)
		volumeTick := time.NewTicker(volumeServiceInterval)
		defer volumeTick.Stop()
		motion := time.NewTimer(time.Hour)
		defer motion.Stop()
		for {
			if !motion.Stop() {
				select {
				case <-motion.C:
				default:
				}
			}
			var motionReady <-chan time.Time
			if delay, pending := t.motionDelay(time.Now()); pending {
				motion.Reset(delay)
				motionReady = motion.C
			}
			select {
			case <-ctx.Done():
				return
			case <-t.pollingChanged:
				// Recompute the motion deadline without resetting volume service.
			case <-t.motionChanged:
			case <-motionReady:
				t.flushMotion(time.Now())
			case <-volumeTick.C:
				t.writeVolume(time.Now(), write)
				t.checkSniperRelease(time.Now())
			case report := <-t.reports:
				started := time.Now()
				inputAllowed := report.data[0] != hid.SniperInputID || t.sniperReportAllowed(report)
				t.mu.Lock()
				valid := !t.stopping && t.enabled && report.generation == t.generation && inputAllowed
				if valid && !report.queuedAt.IsZero() {
					t.timing.QueueWait.observe(started.Sub(report.queuedAt))
				}
				if !valid {
					t.timing.StaleReports++
					t.sniperReportDone(report, context.Canceled)
				}
				t.mu.Unlock()
				if !valid {
					continue
				}
				// Kernel may block despite O_NONBLOCK. stop() must reach unbind without this lock.
				e := write(report.index, report.data[:])
				t.mu.Lock()
				t.timing.WriteTime.observe(time.Since(started))
				t.sniperReportDone(report, e)
				if e == nil {
					t.timing.WrittenReports++
				} else {
					t.timing.WriteErrors++
					t.active = false
					t.gate.BlockUntilRelease()
				}
				t.mu.Unlock()
				if e != nil {
					if ctx.Err() != nil {
						return
					}
					t.reportFailure(fmt.Errorf("HID writer: %w", e))
					log.Printf("HID writer: %v", e)
				}
			}
		}
	}()
}

func (t *touchUSB) releaseLocked(scan uint16) {
	t.havePendingTouch = false
	t.lastTouch = hid.TouchFrame{}
	if t.kind != 0 && t.enabled {
		reports, n, err := t.tracker.Reports(t.kind, scan, nil)
		if err == nil {
			for i := 0; i < n; i++ {
				if t.writeReport(int(t.kind-1), reports[i][:]) != nil {
					break
				}
			}
		}
	}
	t.tracker.Reset()
	t.kind = 0
	t.active = false
}

func (t *touchUSB) submit(f hid.TouchFrame) error { return t.submitAt(f, time.Now()) }
func (t *touchUSB) submitLocked(f hid.TouchFrame) error {
	if !t.gate.Accept(f.Count) {
		return nil
	}
	if !t.enabled || f.Kind == 0 {
		t.releaseLocked(f.ScanTime)
		return nil
	}
	if t.kind != f.Kind {
		t.releaseLocked(f.ScanTime)
		t.kind = f.Kind
	}
	if !t.controls[f.Kind-1].ContactsEnabled() {
		t.releaseLocked(f.ScanTime)
		return nil
	}
	reports, n, err := t.tracker.Reports(f.Kind, f.ScanTime, f.Contacts[:f.Count])
	if err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		if err = t.writeReport(int(f.Kind-1), reports[i][:]); err != nil {
			t.releaseLocked(f.ScanTime)
			return err
		}
	}
	t.active = f.Count > 0
	return nil
}

func (t *touchUSB) control(s functionfs.UsbCtrlRequest) {
	if s.Value != 0x300|hid.EndpointStateReportID && s.Value != 0x300|hid.SniperFeatureID {
		log.Printf("HID control interface=%d type=%02x request=%02x value=%04x length=%d", s.Index, s.RequestType, s.Request, s.Value, s.Length)
	}
	index := int(s.Index)
	if index < 0 || index >= 2 {
		t.ep0.Stall(s.RequestType&0x80 != 0)
		return
	}
	if index == 0 && s.Value == 0x300|hid.SniperFeatureID && s.Length == hid.SniperBytes {
		if s.RequestType == 0xa1 && s.Request == 1 {
			report := t.sniperStatus()
			_, _ = t.ep0.Write(report[:])
			return
		}
		if s.RequestType == 0x21 && s.Request == 9 {
			var report [hid.SniperBytes]byte
			if err := t.ep0.ReadExactly(report[:]); err == nil {
				if err = t.sniperCommand(report[:]); err != nil {
					log.Printf("Sniper HID: %v", err)
				}
			}
			return
		}
		t.ep0.Stall(s.RequestType&0x80 != 0)
		return
	}
	if index == 0 && s.Value == 0x300|hid.EndpointStateReportID && s.Length == 17 {
		if s.RequestType == 0xa1 && s.Request == 1 && t.endpointStatus != nil {
			var report [17]byte
			report[0] = hid.EndpointStateReportID
			payload := t.endpointStatus()
			copy(report[1:], payload[:])
			_, err := t.ep0.Write(report[:])
			if err != nil {
				log.Printf("endpoint state read: %v", err)
			}
			return
		}
		if s.RequestType == 0x21 && s.Request == 9 && t.endpointAck != nil {
			var report [17]byte
			if err := t.ep0.ReadExactly(report[:]); err == nil && report[0] == hid.EndpointStateReportID {
				if err := t.endpointAck(report[1:]); err != nil {
					log.Printf("endpoint acknowledgement: %v", err)
				}
			}
			return
		}
		t.ep0.Stall(s.RequestType&0x80 != 0)
		return
	}
	// Feature 8 has no relationship to touchscreen contacts. Do not hold the
	// touch lock while calling the camera state owner, and never do device I/O there.
	if index == 0 && s.Value == 0x300|uint16(cameraprop.ReportID) {
		if s.RequestType == 0xa1 && s.Request == 1 && t.cameraPropertyStatus != nil {
			report := t.cameraPropertyStatus()
			_, _ = t.ep0.Write(report[:min(int(s.Length), len(report))])
			return
		}
		if s.RequestType == 0x21 && s.Request == 9 && s.Length == cameraprop.Size && t.cameraPropertyCommand != nil {
			var report [cameraprop.Size]byte
			if err := t.ep0.ReadExactly(report[:]); err == nil {
				if err = t.cameraPropertyCommand(report[:]); err != nil {
					log.Printf("camera property rejected: %v", err)
				}
			}
			return
		}
		t.ep0.Stall(s.RequestType&0x80 != 0)
		return
	}
	if index == 0 && s.Value == 0x300|uint16(cameractl.ReportID) {
		if s.RequestType == 0xa1 && s.Request == 1 && t.cameraStatus != nil {
			report := t.cameraStatus()
			n := min(int(s.Length), len(report))
			_, _ = t.ep0.Write(report[:n])
			return
		}
		if s.RequestType == 0x21 && s.Request == 9 && s.Length == cameractl.ReportBytes && t.cameraCommand != nil {
			var report [cameractl.ReportBytes]byte
			if err := t.ep0.ReadExactly(report[:]); err == nil {
				if e := t.cameraCommand(report[:]); e != nil {
					log.Printf("camera control rejected: %v", e)
				}
			}
			return
		}
		t.ep0.Stall(s.RequestType&0x80 != 0)
		return
	}
	in := s.RequestType&0x80 != 0
	var answer []byte
	valid := false
	t.mu.Lock()
	switch {
	case index == 0 && s.RequestType == 0xa1 && s.Request == 1 && s.Value == 0x100|uint16(hid.ConsumerVolumeReportID):
		r := hid.VolumeReport(t.volumeCurrent)
		answer = r[:]
		valid = true
	case s.RequestType == 0x81 && s.Request == 6:
		if s.Value>>8 == 0x22 {
			answer = t.descriptor[index]
			valid = true
		} else if s.Value>>8 == 0x21 {
			answer = t.hidDescriptor(index)
			valid = true
		}
	case s.RequestType == 0xa1 && s.Request == 1 && s.Value>>8 == 3:
		if index == 0 && byte(s.Value) == hid.SniperMaximumID {
			answer, valid = []byte{hid.SniperMaximumID, hid.MaxContacts}, true
			break
		}
		var err error
		answer, err = t.controls[index].Feature(byte(s.Value))
		if err == nil && index == 0 && byte(s.Value) == 7 && t.webcam != nil {
			data := t.webcam()
			copy(answer[1:], data[:])
		}
		valid = err == nil
		if valid && byte(s.Value) == 3 {
			t.seen[index] = true
		}
	case s.RequestType == 0xa1 && s.Request == 3:
		answer = []byte{1}
		valid = true
	case s.RequestType == 0xa1 && s.Request == 2:
		answer = []byte{0}
		valid = true
	case index == 0 && s.RequestType == 0x21 && s.Request == 0x0a && byte(s.Value) == hid.ConsumerVolumeReportID && s.Length == 0:
		valid = s.Value>>8 == 0
	case s.RequestType == 0x21 && s.Request == 0x0a && s.Length == 0:
		valid = true
	case s.RequestType == 0x21 && s.Request == 0x0b && s.Value == 1 && s.Length == 0:
		valid = true
	case index == 1 && s.RequestType == 0x21 && s.Request == 9 && s.Value>>8 == 3 && (byte(s.Value) == 5 || byte(s.Value) == 6) && s.Length == 2:
		t.mu.Unlock()
		var data [2]byte
		readError := t.ep0.ReadExactly(data[:])
		t.mu.Lock()
		if err := readError; err == nil {
			t.releaseLocked(0)
			t.gate.BlockUntilRelease()
			if err = t.controls[index].SetFeature(byte(s.Value), data[:]); err != nil {
				log.Printf("HID feature rejected: %v", err)
			}
			log.Printf("HID feature interface=%d report=%d value=%d mode=%d selective=%d", index, data[0], data[1], t.controls[index].InputMode, t.controls[index].Selective)
		}
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	if !valid {
		t.ep0.Stall(in)
		return
	}
	if in {
		if int(s.Length) < len(answer) {
			answer = answer[:s.Length]
		}
		_, _ = t.ep0.Write(answer)
	} else {
		_ = t.ep0.ReadExactly(nil)
	}
}

// startNative consumes evdev frames directly. No Android app, socket or token service.
func (t *touchUSB) startNative(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	t.cancel = cancel
	t.eventDone = make(chan struct{})
	t.startWriter(ctx)
	go func() {
		defer close(t.eventDone)
		for ctx.Err() == nil {
			e, err := t.ep0.ReadEvent()
			if err != nil {
				if functionfs.RetryEventError(err) {
					time.Sleep(time.Millisecond)
					continue
				}
				if ctx.Err() == nil {
					t.reportFailure(fmt.Errorf("native HID events: %w", err))
				}
				return
			}
			if (e.Type == functionfs.EventDisable || e.Type == functionfs.EventUnbind) && t.cameraDisconnected != nil {
				t.cameraDisconnected()
			}
			if e.Type == functionfs.EventSetup {
				t.control(e.Setup)
				continue
			}
			t.mu.Lock()
			if e.Type == functionfs.EventEnable {
				t.enabled = true
				t.volumeSuspended = false
				t.volumeConnection(true)
			} else if e.Type == functionfs.EventDisable || e.Type == functionfs.EventUnbind {
				t.enabled = false
				t.volumeConnection(false)
				t.generation++
				t.havePendingTouch = false
				t.lastTouch = hid.TouchFrame{}
				t.active = false
				t.tracker.Reset()
				t.sniper = sniperHIDState{}
				t.gate.BlockUntilRelease()
				t.kind = 0
				for _, c := range t.controls {
					c.Reset()
				}
				t.seen = [2]bool{}
			} else if e.Type == functionfs.EventSuspend {
				t.volumeSuspended = true
				t.volumeConnection(false)
			} else if e.Type == functionfs.EventResume {
				t.volumeSuspended = false
				t.volumeConnection(t.enabled)
			}
			t.mu.Unlock()
		}
	}()
}

func (t *touchUSB) blockInput() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.releaseLocked(0)
	if err := t.releaseSniperLocked(time.Now()); err != nil {
		t.reportFailure(err)
	}
	t.gate.BlockUntilRelease()
}

// Cancel producers without waiting on kernel IO. Join only AFTER UDC unbind.
func (t *touchUSB) stop() {
	if t.cancel != nil {
		t.cancel()
	}
	if t.writerCancel != nil {
		t.writerCancel()
	}
	t.mu.Lock()
	t.stopping = true
	t.volumeConnection(false)
	t.generation++
	t.havePendingTouch = false
	t.lastTouch = hid.TouchFrame{}
	t.active = false
	t.enabled = false
	t.tracker.Reset()
	t.gate.BlockUntilRelease()
	t.kind = 0
	t.mu.Unlock()
}

func (t *touchUSB) closeAfterUnbind() error {
	if t == nil {
		return nil
	}
	t.stop()
	var err error
	// Keep file descriptors and buffers alive until pending writes really return.
	for _, done := range []chan struct{}{t.done, t.writerDone} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			return errors.New("touch IO still pending after unbind; resources retained")
		}
	}
	if t.ep0 != nil {
		err = errors.Join(err, t.ep0.Close())
	}
	if t.eventDone != nil {
		select {
		case <-t.eventDone:
		case <-time.After(2 * time.Second):
			return errors.Join(err, errors.New("touch control shutdown deadline"))
		}
	}
	for _, f := range t.input {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
	}
	if t.linked {
		if e := verifyTouchLink(t.link, t.path); e != nil {
			return errors.Join(err, e)
		}
		err = errors.Join(err, os.Remove(t.link))
		t.linked = false
	}
	if t.unmount != nil {
		if e := t.unmount(); e != nil {
			return errors.Join(err, e)
		}
	}
	if t.path != "" {
		if e := os.Remove(t.path); !errors.Is(e, os.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}
	return err
}

func verifyTouchLink(link, expected string) error {
	// ConfigFS renders symlinks as relative paths even after absolute symlink().
	target, err := filepath.EvalSymlinks(link)
	if err != nil {
		return fmt.Errorf("resolve touch link ownership: %w", err)
	}
	if target != expected {
		return fmt.Errorf("touch link changed ownership: got %q, expected %q", target, expected)
	}
	return nil
}

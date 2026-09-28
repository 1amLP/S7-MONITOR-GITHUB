//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"perimode/native/internal/linuxio"
	"perimode/native/pkg/hid"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

type AbsInfo struct{ Value, Minimum, Maximum, Fuzz, Flat, Resolution int32 }
type Event struct {
	Seconds, Microseconds int64
	Type, Code            uint16
	Value                 int32
}
type InputFrame struct {
	// Epoch is sampled before the kernel read, not while dispatching the batch.
	// Frames read before an orientation change can never unlock its all-up gate.
	Epoch    uint64
	Contacts []hid.Contact
	Desync   bool
	// EventTimeUS is the kernel SYN_REPORT timestamp. Only differences between
	// these timestamps are meaningful; do not subtract a Go monotonic timestamp.
	EventTimeUS int64
	ReadAt      time.Time
}

func inputEventTimeUS(e Event) int64 {
	if e.Seconds < 0 || e.Microseconds < 0 || e.Microseconds >= 1_000_000 || e.Seconds > (1<<63-1-e.Microseconds)/1_000_000 {
		return 0
	}
	return e.Seconds*1_000_000 + e.Microseconds
}

// Mapping and HID scan time must retain sample spacing when the UI catches up
// with a batch. Synthetic all-up boundaries/tests fall back to the caller time.
func (f InputFrame) sampleTime(now time.Time) (milliseconds int64, scan uint16) {
	us := f.EventTimeUS
	if us <= 0 {
		us = now.UnixMicro()
	}
	return us / 1000, uint16(us / 100)
}

type InputStageTiming struct {
	Count   uint64 `json:"count"`
	TotalUS int64  `json:"total_us"`
	MaxUS   int64  `json:"max_us"`
}

func (s *InputStageTiming) observe(d time.Duration) {
	if d < 0 {
		return
	}
	s.Count++
	s.TotalUS += d.Microseconds()
	s.MaxUS = max(s.MaxUS, d.Microseconds())
}

type InputTimingSnapshot struct {
	ReadFrames         uint64           `json:"read_frames"`
	ActiveFrames       uint64           `json:"active_frames"`
	EvdevIntervals     InputStageTiming `json:"active_evdev_intervals"`
	ReadToUI           InputStageTiming `json:"read_to_ui"`
	UIProcessing       InputStageTiming `json:"ui_processing"`
	IgnoredEpochFrames uint64           `json:"ignored_epoch_frames"`
	WaitingAllUpFrames uint64           `json:"waiting_all_up_frames"`
	MenuNoLayoutFrames uint64           `json:"menu_no_layout_frames"`
	CancelledMenuTaps  uint64           `json:"cancelled_menu_taps"`
	AcceptedMenuTaps   uint64           `json:"accepted_menu_taps"`
	MenuMotionCancels  uint64           `json:"menu_motion_cancels"`
	MenuTargetCancels  uint64           `json:"menu_target_cancels"`
	MenuMultiCancels   uint64           `json:"menu_multitouch_cancels"`
	MenuQueueWait      InputStageTiming `json:"menu_queue_wait"`
	MenuRender         InputStageTiming `json:"menu_render"`
}

type inputDiscard uint8

const (
	inputOldEpoch inputDiscard = iota
	inputHeldBarrier
	inputNoMenuLayout
	inputCancelledTap
)

func (m *inputTiming) discarded(reason inputDiscard) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch reason {
	case inputOldEpoch:
		m.stats.IgnoredEpochFrames++
	case inputHeldBarrier:
		m.stats.WaitingAllUpFrames++
	case inputNoMenuLayout:
		m.stats.MenuNoLayoutFrames++
	case inputCancelledTap:
		m.stats.CancelledMenuTaps++
	}
}

func (m *inputTiming) menuPaint(queued, started, finished time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats.MenuQueueWait.observe(started.Sub(queued))
	m.stats.MenuRender.observe(finished.Sub(started))
}

func (m *inputTiming) menuTapAccepted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats.AcceptedMenuTaps++
}

func (m *inputTiming) menuTapCancelled(multitouch, motion bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats.CancelledMenuTaps++
	if multitouch {
		m.stats.MenuMultiCancels++
	} else if motion {
		m.stats.MenuMotionCancels++
	} else {
		m.stats.MenuTargetCancels++
	}
}

// Read and UI counters have different owners. No filesystem, framebuffer or
// transport work runs under this lock. Evdev intervals are not a sensor claim.
type inputTiming struct {
	mu              sync.Mutex
	stats           InputTimingSnapshot
	previousEventUS int64
	previousEpoch   uint64
	previousActive  bool
}

func (m *inputTiming) observeRead(f InputFrame) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats.ReadFrames++
	active := !f.Desync && len(f.Contacts) > 0 && f.EventTimeUS > 0
	if active {
		m.stats.ActiveFrames++
		if m.previousActive && m.previousEpoch == f.Epoch && f.EventTimeUS > m.previousEventUS {
			delta := f.EventTimeUS - m.previousEventUS
			if delta <= (1<<63-1)/int64(time.Microsecond) {
				m.stats.EvdevIntervals.observe(time.Duration(delta) * time.Microsecond)
			}
		}
	}
	m.previousActive, m.previousEpoch, m.previousEventUS = active, f.Epoch, f.EventTimeUS
}

func (m *inputTiming) observeUI(f InputFrame, started, finished time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !f.ReadAt.IsZero() {
		m.stats.ReadToUI.observe(started.Sub(f.ReadAt))
	}
	m.stats.UIProcessing.observe(finished.Sub(started))
}

func (m *inputTiming) snapshot() InputTimingSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stats
}

type inputReadiness struct{ fd int }

func newInputReadiness(fd int) (*inputReadiness, error) {
	epoll, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	if err = syscall.EpollCtl(epoll, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)}); err != nil {
		_ = syscall.Close(epoll)
		return nil, err
	}
	return &inputReadiness{fd: epoll}, nil
}

func (r *inputReadiness) wait() error {
	var events [1]syscall.EpollEvent
	// Data wakes immediately. The timeout bounds cancellation and epoch checks
	// while idle; it is not a polling interval imposed on incoming samples.
	_, err := syscall.EpollWait(r.fd, events[:], 20)
	if err == syscall.EINTR {
		return nil
	}
	return err
}

func (r *inputReadiness) close() { _ = syscall.Close(r.fd) }

type slot struct {
	tracking       int32
	x, y           int32
	id             byte
	xValid, yValid bool
	admitted       bool
}
type InputState struct {
	slots           [32]slot
	count, selected int
	nextID          byte
	lost            bool
	x, y            AbsInfo
	rotate          bool
}

func NewInputState(count int, x, y AbsInfo, rotate bool) (*InputState, error) {
	if count < 1 || count > 32 || x.Maximum <= x.Minimum || y.Maximum <= y.Minimum {
		return nil, fmt.Errorf("invalid evdev axes/slots")
	}
	s := &InputState{count: count, x: x, y: y, rotate: rotate}
	for i := range s.slots {
		s.slots[i].tracking = -1
	}
	return s, nil
}
func scale(v int32, a AbsInfo) uint16 {
	v = max(a.Minimum, min(a.Maximum, v))
	return uint16((int64(v) - int64(a.Minimum)) * 32767 / (int64(a.Maximum) - int64(a.Minimum)))
}
func (s *InputState) Feed(e Event) (InputFrame, bool) {
	if e.Type == 0 && e.Code == 3 {
		s.loseSync()
		return InputFrame{Desync: true}, true
	}
	if s.lost {
		return InputFrame{}, false
	}
	if e.Type == 3 {
		if e.Code == 0x2f {
			if e.Value < 0 || int(e.Value) >= s.count {
				s.loseSync()
				return InputFrame{Desync: true}, true
			}
			s.selected = int(e.Value)
		}
		sl := &s.slots[s.selected]
		switch e.Code {
		case 0x39:
			if e.Value < 0 {
				sl.tracking = -1
				sl.admitted = false
				sl.xValid, sl.yValid = false, false
				break
			}
			if sl.tracking != e.Value {
				sl.tracking = e.Value
				sl.admitted = false
				sl.xValid, sl.yValid = false, false
				if s.admittedCount() < hid.MaxContacts {
					id, ok := s.nextContactID()
					if !ok {
						s.loseSync()
						return InputFrame{Desync: true}, true
					}
					sl.id, sl.admitted = id, true
				}
			}
		case 0x35:
			sl.x = e.Value
			sl.xValid = true
		case 0x36:
			sl.y = e.Value
			sl.yValid = true
		}
	}
	if e.Type == 0 && e.Code == 0 {
		f := InputFrame{EventTimeUS: inputEventTimeUS(e)}
		for _, sl := range s.slots[:s.count] {
			if sl.tracking >= 0 && sl.admitted {
				if !sl.xValid || !sl.yValid {
					s.loseSync()
					return InputFrame{Desync: true}, true
				}
				x, y := scale(sl.x, s.x), scale(sl.y, s.y)
				if s.rotate {
					x, y = y, 32767-x
				}
				f.Contacts = append(f.Contacts, hid.Contact{ID: sl.id, X: x, Y: y, Tip: true, Confidence: true})
			}
		}
		if len(f.Contacts) > hid.MaxContacts {
			return InputFrame{Desync: true}, true
		}
		return f, true
	}
	return InputFrame{}, false
}

func (s *InputState) admittedCount() int {
	n := 0
	for _, sl := range s.slots[:s.count] {
		if sl.tracking >= 0 && sl.admitted {
			n++
		}
	}
	return n
}

func (s *InputState) nextContactID() (byte, bool) {
	for tries := 0; tries < 32; tries++ {
		candidate := s.nextID
		s.nextID = (s.nextID + 1) % 32
		used := false
		for _, other := range s.slots[:s.count] {
			if other.tracking >= 0 && other.admitted && other.id == candidate {
				used = true
				break
			}
		}
		if !used {
			return candidate, true
		}
	}
	return 0, false
}

func (s *InputState) loseSync() {
	s.lost = true
	for i := range s.slots {
		s.slots[i].tracking = -1
		s.slots[i].admitted = false
		s.slots[i].xValid, s.slots[i].yValid = false, false
	}
}
func (s *InputState) ResyncAllUp(fd int) bool {
	// SYN_DROPPED is recovered only after querying the actual kernel tracking state
	// and observing all-up. Old contacts are not inferred from missing events.
	data := make([]int32, s.count+1)
	data[0] = 0x39
	request := uintptr(0x80000000 | ((len(data) * 4) << 16) | (int('E') << 8) | 0x0a)
	if linuxio.Ioctl(fd, request, unsafe.Pointer(&data[0])) != nil {
		return false
	}
	for _, id := range data[1:] {
		if id >= 0 {
			return false
		}
	}
	for i := range s.slots {
		s.slots[i].tracking = -1
		s.slots[i].admitted = false
		s.slots[i].xValid, s.slots[i].yValid = false, false
	}
	s.lost = false
	return true
}
func abs(fd int, code byte) (AbsInfo, error) {
	var a AbsInfo
	e := linuxio.Ioctl(fd, uintptr(0x80184540)+uintptr(code), unsafe.Pointer(&a))
	return a, e
}

func physicalKeyDevice(name string) (allowed, gpio bool) {
	switch name {
	case "gpio_keys", "gpio-keys":
		return true, true
	case "sec_touchkey":
		return true, false
	default:
		return false, false
	}
}

func physicalTouchDevice(name string) bool {
	return name == "sec_touchscreen" || name == "fts_touch"
}

func newPhysicalTouchState(name string, x, y, slots AbsInfo, xErr, yErr, slotErr error, rotate bool) (*InputState, error) {
	if !physicalTouchDevice(name) {
		return nil, nil
	}
	if err := errors.Join(xErr, yErr, slotErr); err != nil {
		return nil, fmt.Errorf("touch axes %s: %w", name, err)
	}
	if slots.Minimum != 0 || slots.Maximum < 0 {
		return nil, fmt.Errorf("touch slots %s: invalid range %d..%d", name, slots.Minimum, slots.Maximum)
	}
	return NewInputState(int(slots.Maximum)+1, x, y, rotate)
}

func feedInputState(s *InputState, e Event, resync func() bool) (InputFrame, bool) {
	if s.lost && e.Type == 0 && e.Code == 0 && resync() {
		return InputFrame{EventTimeUS: inputEventTimeUS(e)}, true
	}
	return s.Feed(e)
}

// StartInputs opens only existing evdev nodes. No EVIOCGRAB, no input injection.
func StartInputs(ctx context.Context, rotate bool, epoch func() uint64, onKey func(uint16), onVolume func(VolumeInput), onFrame func(InputFrame), onError func(error)) int {
	return StartInputsTracked(ctx, rotate, epoch, onKey, onVolume, onFrame, onError, nil)
}

// StartInputsTracked registers every actual evdev reader before it can run.
// Shutdown can therefore join physical input callbacks before closing USB/HID.
func StartInputsTracked(ctx context.Context, rotate bool, epoch func() uint64, onKey func(uint16), onVolume func(VolumeInput), onFrame func(InputFrame), onError func(error), launch func(string, func(context.Context) error) error) int {
	return StartInputsWithFeedbackTracked(ctx, rotate, epoch, onKey, onVolume, onFrame, onError, nil, launch)
}

func StartInputsWithFeedbackTracked(ctx context.Context, rotate bool, epoch func() uint64, onKey func(uint16), onVolume func(VolumeInput), onFrame func(InputFrame), onError func(error), onPress func(uint16), launch func(string, func(context.Context) error) error) int {
	paths, _ := filepath.Glob("/dev/input/event*")
	started := 0
	for _, p := range paths {
		f, e := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if e != nil {
			onError(e)
			continue
		}
		var name [128]byte
		_ = linuxio.Ioctl(int(f.Fd()), 0x80804506, unsafe.Pointer(&name[0]))
		// Only the pinned physical keys and touchscreen; do not treat arbitrary USB
		// keyboards/sensors as privileged Home/Power controls.
		n := linuxio.CString(name[:])
		keyDevice, gpioDevice := physicalKeyDevice(n)
		allowed := keyDevice || physicalTouchDevice(n)
		var state *InputState
		if physicalTouchDevice(n) {
			x, ex := abs(int(f.Fd()), 0x35)
			y, ey := abs(int(f.Fd()), 0x36)
			sl, es := abs(int(f.Fd()), 0x2f)
			state, e = newPhysicalTouchState(n, x, y, sl, ex, ey, es, rotate)
			if e != nil {
				onError(e)
				f.Close()
				continue
			}
		}
		if !allowed {
			f.Close()
			continue
		}
		run := func(worker context.Context) error {
			func(ctx context.Context, file *os.File, s *InputState, keyDevice, gpioDevice bool) {
				defer file.Close()
				fd := int(file.Fd())
				if e := syscall.SetNonblock(fd, true); e != nil {
					onError(e)
					return
				}
				readiness, e := newInputReadiness(fd)
				if e != nil {
					onError(fmt.Errorf("evdev readiness %s: %w", file.Name(), e))
					return
				}
				defer readiness.close()
				lastEpoch := ^uint64(0)
				b := make([]byte, 24*32)
				var keys *physicalKeys
				if keyDevice || s != nil {
					keys = newPhysicalKeys(gpioDevice)
					if keys.gpio && onVolume != nil {
						onVolume(VolumeInput{Lost: true})
						defer onVolume(VolumeInput{Lost: true})
					}
				}
				for ctx.Err() == nil {
					if keys != nil && keys.powerHoldDue(time.Now()) {
						bits, keyErr := readPhysicalKeys(fd)
						if keyErr != nil {
							onError(keyErr)
						} else if bits[116/8]&(1<<(116%8)) != 0 {
							onKey(localPowerHold)
						}
					}
					if keys != nil && !keys.ready && keys.boundary {
						bits, e := readPhysicalKeys(fd)
						if e != nil {
							onError(e)
							return
						}
						if keys.resync(bits) && keys.gpio && onVolume != nil {
							onVolume(VolumeInput{AllUp: true})
						}
					}
					generation := uint64(0)
					if epoch != nil {
						generation = epoch()
					}
					if generation != lastEpoch {
						lastEpoch = generation
						if s != nil {
							s.lost = true
							if s.ResyncAllUp(fd) {
								onFrame(InputFrame{Epoch: generation, ReadAt: time.Now()})
							}
						}
					}
					n, e := syscall.Read(fd, b)
					if e == syscall.EINTR {
						continue
					}
					if e == syscall.EAGAIN {
						if e = readiness.wait(); e != nil {
							onError(fmt.Errorf("evdev wait %s: %w", file.Name(), e))
							return
						}
						continue
					}
					if e != nil {
						if ctx.Err() == nil {
							onError(fmt.Errorf("evdev %s: %w", file.Name(), e))
						}
						return
					}
					if n == 0 {
						onError(fmt.Errorf("evdev %s: unexpected EOF", file.Name()))
						return
					}
					readAt := time.Now()
					if n%24 != 0 {
						onError(fmt.Errorf("truncated input event"))
						return
					}
					for off := 0; off < n; off += 24 {
						a := b[off:]
						ev := Event{Seconds: int64(binary.LittleEndian.Uint64(a)), Microseconds: int64(binary.LittleEndian.Uint64(a[8:])), Type: binary.LittleEndian.Uint16(a[16:]), Code: binary.LittleEndian.Uint16(a[18:]), Value: int32(binary.LittleEndian.Uint32(a[20:]))}
						if keys != nil {
							press := keys.ready && ev.Type == 1 && ev.Value == 1 && keys.allowed(ev.Code) && !keys.down[ev.Code]
							key, volume := keys.feed(ev)
							if press && onPress != nil {
								onPress(ev.Code)
							}
							if key != 0 {
								onKey(key)
							}
							if volume != nil && onVolume != nil {
								onVolume(*volume)
							}
						}
						if s != nil {
							if frame, ready := feedInputState(s, ev, func() bool { return s.ResyncAllUp(fd) }); ready {
								frame.Epoch = generation
								frame.ReadAt = readAt
								onFrame(frame)
							}
						}
					}
				}
			}(worker, f, state, keyDevice, gpioDevice)
			return worker.Err()
		}
		if launch != nil {
			if err := launch("input/"+filepath.Base(p)+"/"+n, run); err != nil {
				_ = f.Close()
				onError(err)
				continue
			}
		} else {
			go run(ctx)
		}
		started++
	}
	return started
}

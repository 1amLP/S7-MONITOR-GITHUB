package camera

import (
	"errors"
	"fmt"
	"perimode/native/internal/camera/fimcshot"
	"perimode/native/internal/media"
	"math"
	"os"
	"time"
)

var ErrControlsUnsupported = errors.New("native control descriptor/update interface unavailable")
var ErrFocusPending = errors.New("previous focus command is still awaiting the capture worker")

// Selection is separate from Settings: a control edit must NOT change the USB
// mode, CaptureKey or stream epoch. Zero means the source-derived defaults.
type ControlSelection struct {
	Custom bool              `json:"custom"`
	Values fimcshot.Controls `json:"values"`
}

func (s ControlSelection) resolve(d ControlDescriptor) (fimcshot.Controls, error) {
	if !s.Custom {
		if s.Values != (fimcshot.Controls{}) {
			return fimcshot.Controls{}, fmt.Errorf("nonzero controls with custom disabled")
		}
		return d.Defaults, d.Defaults.Validate(d.Limits)
	}
	if s.Values.Trigger != fimcshot.FocusIdle {
		return fimcshot.Controls{}, fmt.Errorf("focus edge must use TriggerFocus, not saved controls")
	}
	return s.Values, s.Values.Validate(d.Limits)
}
func (s ControlSelection) Persistent() ControlSelection {
	s.Values.Trigger = fimcshot.FocusIdle
	s.Values.AELocked = false
	s.Values.AWBLocked = false
	return s
}

// ControlSlot order is fixed for the persistence schema, not the video-node order.
func ControlSlot(key CaptureKey) int {
	i := 0
	for _, sensor := range []Sensor{Rear, Front} {
		for _, mode := range Modes(sensor) {
			if key == (CaptureKey{sensor, mode}) {
				return i
			}
			i++
		}
	}
	return -1
}

type ControlPreferences [10]ControlSelection

func (p ControlPreferences) Validate() error {
	for i, s := range p {
		if !s.Custom {
			if s.Values != (fimcshot.Controls{}) {
				return fmt.Errorf("invalid default controls slot %d", i)
			}
			continue
		}
		// Persistence is parsed without powering/probing sensors. A strict outer
		// envelope rejects malformed state; actual hardware limits apply later.
		c := s.Values
		l := fimcshot.Limits{Crop: fimcshot.Rect{Width: 16384, Height: 16384}, FrameDurationNS: 1_000_000_000,
			ExposureMinNS: 1, ExposureMaxNS: 1_000_000_000, ISOMin: 1, ISOMax: 1_000_000,
			CompensationMin: -1000, CompensationMax: 1000, FocusModes: 0x3e, WBModeMask: 0x7fc, ISPImageControls: true,
			MaxFocusDioptres: 1000, ManualExposure: true, ManualISO: true, ManualFocus: true, AELock: true, AWBLock: true}
		if c.Trigger != fimcshot.FocusIdle || c.AELocked || c.AWBLocked {
			return fmt.Errorf("transient lock/trigger in persistent controls")
		}
		if e := c.Validate(l); e != nil {
			return fmt.Errorf("control slot %d: %w", i, e)
		}
	}
	return nil
}

type ControlDescriptor struct {
	Key      CaptureKey        `json:"key"`
	ModuleID uint32            `json:"module_id"`
	Limits   fimcshot.Limits   `json:"limits"`
	Defaults fimcshot.Controls `json:"defaults"`
	EVStep   float32           `json:"ev_step"`
}

func (d ControlDescriptor) Validate(s Settings) error {
	if e := s.Validate(); e != nil {
		return e
	}
	if d.Key != Key(s) || d.ModuleID == 0 || d.EVStep <= 0 || d.EVStep > 10 || math.IsNaN(float64(d.EVStep)) || math.IsInf(float64(d.EVStep), 0) ||
		d.Limits.FrameDurationNS != 1_000_000_000/uint64(s.Mode.FPS) {
		return fmt.Errorf("wrong camera control descriptor")
	}
	return d.Defaults.Validate(d.Limits)
}

type ControlDescriptorProvider interface {
	DescribeControls(Settings) (ControlDescriptor, error)
}
type ControlSource interface{ UpdateControls(fimcshot.Controls) error }

type LiveControlState struct {
	Sensor              media.SensorResult `json:"sensor_result"`
	Descriptor          ControlDescriptor  `json:"descriptor"`
	Selection           ControlSelection   `json:"selection"`
	Effective           fimcshot.Controls  `json:"effective_request"`
	Requested, Accepted uint64
	Status              string `json:"status"`
	Error               string `json:"error,omitempty"`
	Active              bool   `json:"active"`
}

type controlEntry struct {
	sensor                        media.SensorResult
	sensorAt                      time.Time
	selection                     ControlSelection
	revision, accepted, attempted uint64
	trigger                       fimcshot.FocusTrigger
	lastError                     string
}

func (p *SharedProvider) DescribeControls(s Settings) (ControlDescriptor, error) {
	if e := s.Validate(); e != nil {
		return ControlDescriptor{}, e
	}
	dp, ok := p.backend.(ControlDescriptorProvider)
	if !ok {
		return ControlDescriptor{}, ErrControlsUnsupported
	}
	d, e := dp.DescribeControls(s)
	if e == nil {
		e = d.Validate(s)
	}
	return d, e
}
func (p *SharedProvider) controlLocked(key CaptureKey) *controlEntry {
	if p.controls == nil {
		p.controls = make(map[CaptureKey]*controlEntry)
	}
	entry := p.controls[key]
	if entry == nil {
		entry = &controlEntry{revision: 1}
		p.controls[key] = entry
	}
	return entry
}
func (p *SharedProvider) ControlState(s Settings) (LiveControlState, error) {
	d, e := p.DescribeControls(s)
	if e != nil {
		return LiveControlState{}, e
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return LiveControlState{}, os.ErrClosed
	}
	entry := p.controlLocked(Key(s))
	c, e := entry.selection.resolve(d)
	v := LiveControlState{Descriptor: d, Selection: entry.selection, Effective: c, Requested: entry.revision, Accepted: entry.accepted, Error: entry.lastError}
	v.Active = p.session != nil && !p.session.stopping && Key(p.session.settings) == Key(s)
	if v.Active && time.Since(entry.sensorAt) < time.Second {
		v.Sensor = entry.sensor
	}
	switch {
	case e != nil:
		v.Status = "SAVED VALUES NOT VALID FOR THIS HARDWARE"
		v.Error = e.Error()
	case entry.lastError != "":
		v.Status = "CONTROL UPDATE REJECTED"
	case !v.Active:
		v.Status = "SAVED / WAITING FOR CAPTURE"
	case entry.accepted != entry.revision:
		v.Status = "PENDING CAPTURE WORKER"
	default:
		v.Status = "ACCEPTED FOR NEXT ISP REQUEST / NOT MEASURED"
	}
	return v, e
}

// Only a completely stopped session with no reader leases may be retired.
// Called under p.mu; no waits, device calls or I/O.
func (p *SharedProvider) retireControlsSessionLocked() error {
	if cs := p.session; cs != nil {
		select {
		case <-cs.done:
			if captureOwnershipUncertain(cs.err) {
				p.poisoned = true
				return ErrOwnership
			}
			if cs.readers[0] != nil || cs.readers[1] != nil {
				return ErrCaptureBusy
			}
			p.session = nil
		default:
		}
	}
	return nil
}

// SetControls is bounded, nonblocking and all-or-nothing. The capture worker is
// the only caller of Source.UpdateControls; UI/USB threads never do camera I/O.
func (p *SharedProvider) SetControls(s Settings, selection ControlSelection) error {
	d, e := p.DescribeControls(s)
	if e != nil {
		return e
	}
	if _, e = selection.resolve(d); e != nil {
		return e
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return os.ErrClosed
	}
	if p.poisoned {
		return ErrOwnership
	}
	key := Key(s)
	entry := p.controlLocked(key)
	if entry.trigger != fimcshot.FocusIdle {
		if p.session != nil && !p.session.stopping && Key(p.session.settings) == key {
			return ErrFocusPending
		}
		entry.trigger = fimcshot.FocusIdle
	}
	// Preparing the inactive endpoint changes only its preference bank. The
	// active session reads its own key and never applies another sensor's controls.
	if entry.selection == selection {
		return nil
	}
	if entry.revision == ^uint64(0) {
		return fmt.Errorf("control generation exhausted")
	}
	entry.selection = selection
	entry.revision++
	entry.lastError = ""
	return nil
}
func (p *SharedProvider) TriggerFocus(s Settings, trigger fimcshot.FocusTrigger) error {
	if trigger != fimcshot.FocusStart && trigger != fimcshot.FocusCancel {
		return fmt.Errorf("invalid focus edge")
	}
	d, e := p.DescribeControls(s)
	if e != nil {
		return e
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return os.ErrClosed
	}
	if p.poisoned {
		return ErrOwnership
	}
	if e = p.retireControlsSessionLocked(); e != nil {
		return e
	}
	if p.session == nil || p.session.stopping || Key(p.session.settings) != Key(s) {
		return fmt.Errorf("focus action needs an active matching camera")
	}
	entry := p.controlLocked(Key(s))
	if entry.trigger != fimcshot.FocusIdle {
		return ErrFocusPending
	}
	c, e := entry.selection.resolve(d)
	if e != nil {
		return e
	}
	c.Trigger = trigger
	if e = c.Validate(d.Limits); e != nil {
		return e
	}
	if entry.revision == ^uint64(0) {
		return fmt.Errorf("control generation exhausted")
	}
	entry.trigger = trigger
	entry.revision++
	entry.lastError = ""
	return nil
}
func (p *SharedProvider) ControlPreferences() ControlPreferences {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out ControlPreferences
	for key, e := range p.controls {
		if i := ControlSlot(key); i >= 0 {
			out[i] = e.selection.Persistent()
		}
	}
	return out
}

// Restore only while idle. No sensor is touched; limits are rechecked at open.
func (p *SharedProvider) RestoreControls(pref ControlPreferences) error {
	if e := pref.Validate(); e != nil {
		return e
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return os.ErrClosed
	}
	if p.poisoned {
		return ErrOwnership
	}
	if e := p.retireControlsSessionLocked(); e != nil {
		return e
	}
	if p.session != nil {
		return ErrCaptureBusy
	}
	for _, sensor := range []Sensor{Rear, Front} {
		for _, mode := range Modes(sensor) {
			key := CaptureKey{sensor, mode}
			p.controlsInit(key, pref[ControlSlot(key)])
		}
	}
	return nil
}
func (p *SharedProvider) controlsInit(key CaptureKey, s ControlSelection) {
	e := p.controlLocked(key)
	e.selection = s
	e.revision++
	e.accepted = 0
	e.attempted = 0
	e.trigger = 0
	e.lastError = ""
}

// Called in the capture loop, outside the SharedProvider mutex. Existing sources
// without a control descriptor are left unchanged (e.g. an unrelated test source).
func (s *captureSession) applyControls(source Source, d *ControlDescriptor) error {
	if d == nil {
		return nil
	}
	p := s.owner
	p.mu.Lock()
	entry := p.controlLocked(Key(s.settings))
	rev, sel, edge := entry.revision, entry.selection, entry.trigger
	if entry.attempted == rev {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	c, err := sel.resolve(*d)
	if err == nil {
		c.Trigger = edge
		err = c.Validate(d.Limits)
	}
	if err == nil {
		if target, ok := source.(ControlSource); ok {
			err = target.UpdateControls(c)
		} else {
			err = ErrControlsUnsupported
		}
	}
	// A pending focus edge cannot be replaced before its QBUF. Let Drain run
	// and retry next iteration; transient errors are not final rejections.
	if wouldBlock(err) && !captureOwnershipUncertain(err) && !errors.Is(err, os.ErrClosed) {
		return nil
	}
	p.mu.Lock()
	entry = p.controlLocked(Key(s.settings))
	entry.attempted = rev
	if err == nil {
		entry.accepted = rev
		entry.lastError = ""
	} else {
		entry.lastError = err.Error()
	}
	// Clear an edge only for the exact generation handed to the source; a new
	// command may have arrived during UpdateControls. Never replay it on reopen.
	if entry.revision == rev {
		entry.trigger = fimcshot.FocusIdle
	}
	p.mu.Unlock()
	// Rejecting a setting is local to 3A, not grounds for tearing down the video.
	// Unknown DMA ownership/closed source is different and aborts capture.
	if errors.Is(err, ErrOwnership) || errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

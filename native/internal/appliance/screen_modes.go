package appliance

import (
	"encoding/binary"
	"perimode/native/internal/fb"
	"perimode/native/pkg/hid"
)

type ScreenView uint8

const (
	ViewMonitor ScreenView = iota
	ViewCamera
	ViewSniper
)

func (v ScreenView) String() string {
	switch v {
	case ViewCamera:
		return "CAMERA"
	case ViewSniper:
		return "SNIPER"
	}
	return "MONITOR"
}
func (s *State) viewAllowedLocked(v ScreenView) bool {
	switch v {
	case ViewMonitor:
		return true
	case ViewCamera:
		return s.Camera.Enabled && s.Preview.Enabled && s.Preview.Options.Fullscreen
	case ViewSniper:
		return s.Settings.Enabled && s.Settings.SniperEnabled
	}
	return false
}
func (s *State) activeViewLocked() ScreenView {
	if s.viewAllowedLocked(s.View) {
		return s.View
	}
	return ViewMonitor
}
func (s *State) ActiveView() ScreenView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeViewLocked()
}
func (s *State) setViewLocked(v ScreenView) {
	if !s.viewAllowedLocked(v) {
		v = ViewMonitor
	}
	if s.View == v {
		return
	}
	s.View = v
	s.sniperInput = sniperInputQueue{}
	s.invalidateMonitorLocked()
	s.Generation++
	if s.Generation == 0 {
		s.Generation = 1
	}
	s.Ack, s.previous, s.pts = 0, 0, 0
	s.resyncLocked()
}
func (s *State) NextView() ScreenView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.activeViewLocked()
	for i := 0; i < 3; i++ {
		v = (v + 1) % 3
		if s.viewAllowedLocked(v) {
			s.setViewLocked(v)
			break
		}
	}
	return v
}
func (s *State) PreviewForDisplay() PreviewRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.Preview
	if p.Options.Fullscreen && s.activeViewLocked() != ViewCamera {
		p.Enabled = false
	}
	if s.activeViewLocked() == ViewSniper {
		p.Enabled = false
	}
	return p
}
func (u *UI) cycleView() {
	u.sniperGesture = sniperGesture{}
	u.sniperPress = sniperPress{}
	v := u.state.NextView()
	u.cameraHUDSelection = ""
	u.cameraHUDDragging = false
	u.previewGesture = false
	u.pad.Reset()
	if u.transport != nil && u.transport.touch != nil {
		u.transport.touch.blockInput()
		if u.lastInputCount == 0 {
			_ = u.transport.touch.submit(hid.TouchFrame{})
		} else {
			u.inputAwaitAllUp = true
		}
	}
	u.notify(v.String())
	u.draw()
}
func (u *UI) setSniperZoom(value int) {
	value = max(100, min(1600, value))
	u.state.mu.Lock()
	u.state.Settings.SniperZoom = uint16(value)
	u.state.mu.Unlock()
	u.menuNeedsDraw = true
}

func (u *UI) setSniperValue(value int) {
	if u.cameraHUDSelection == "X SCALE" || u.cameraHUDSelection == "Y SCALE" {
		u.setSniperScale(u.cameraHUDSelection, value)
		return
	}
	if u.cameraHUDSelection != "X" && u.cameraHUDSelection != "Y" {
		u.setSniperZoom(value)
		return
	}
	u.state.mu.Lock()
	if u.cameraHUDSelection == "X" {
		u.state.Settings.SniperX = int16(max(0, min(10000, value)) - 5000)
	} else {
		u.state.Settings.SniperY = int16(max(0, min(10000, value)) - 5000)
	}
	u.state.mu.Unlock()
}
func (u *UI) setSniperScale(axis string, value int) {
	value = max(25, min(400, (value+2)/5*5))
	u.state.mu.Lock()
	if axis == "X SCALE" {
		u.state.Settings.SniperScaleX = uint16(value)
	} else if axis == "Y SCALE" {
		u.state.Settings.SniperScaleY = uint16(value)
	}
	u.state.mu.Unlock()
	u.menuNeedsDraw = true
}

func (u *UI) sniperHUD() *fb.CameraHUD {
	if u.state.ActiveView() != ViewSniper || u.noticeText == "" {
		return nil
	}
	return &fb.CameraHUD{Selected: -1, OptionSelected: -1, Notice: u.noticeText}
}

type sniperContact struct {
	sequence, x, y uint16
	down           bool
}
type sniperInputQueue struct {
	queue         []sniperContact
	sequence      uint16
	down, blocked bool
}

func (q *sniperInputQueue) submit(contacts []hid.Contact) {
	if len(contacts) > 1 {
		q.submit(nil)
		q.blocked = true
		return
	}
	down := len(contacts) == 1
	if q.blocked {
		if len(contacts) == 0 {
			q.blocked = false
		}
		return
	}
	if !down && !q.down {
		return
	}
	q.sequence++
	if q.sequence == 0 {
		q.sequence = 1
	}
	c := sniperContact{sequence: q.sequence, down: down}
	if down {
		c.x, c.y = contacts[0].X, contacts[0].Y
	}
	// Preserve every edge. Only replace motion within the same held contact.
	if n := len(q.queue); down && q.down && n > 1 && q.queue[n-1].down {
		q.queue[n-1] = c
	} else if n >= 16 {
		q.queue = []sniperContact{{sequence: q.sequence}}
		q.blocked, down = true, false
	} else {
		q.queue = append(q.queue, c)
	}
	q.down = down
}
func (s *State) SniperInput(contacts []hid.Contact) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeViewLocked() == ViewSniper {
		if s.TouchKind == 0 {
			contacts = nil
		}
		s.sniperInput.submit(contacts)
	}
}
func (s *State) SniperControl(ack uint16) [32]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := &s.sniperInput
	if len(q.queue) > 0 && q.queue[0].sequence == ack {
		q.queue = q.queue[1:]
	}
	var b [32]byte
	copy(b[:], "S7S1")
	le := binary.LittleEndian
	le.PutUint16(b[4:], 3)
	le.PutUint16(b[6:], 32)
	le.PutUint32(b[8:], s.Generation)
	le.PutUint16(b[12:], uint16(s.Settings.Zoom()))
	le.PutUint16(b[14:], uint16(s.Settings.SniperX+5000))
	le.PutUint16(b[16:], uint16(s.Settings.SniperY+5000))
	le.PutUint16(b[28:], uint16(s.Settings.ScaleX()))
	le.PutUint16(b[30:], uint16(s.Settings.ScaleY()))
	flags := s.Settings.SniperRotation << 3
	if s.activeViewLocked() == ViewSniper {
		flags |= 1
	}
	if s.Settings.SniperStretch {
		flags |= 2
	}
	if s.Settings.SniperMirror {
		flags |= 4
	}
	le.PutUint16(b[18:], flags)
	if len(q.queue) > 0 {
		c := q.queue[0]
		le.PutUint16(b[20:], c.sequence)
		if c.down {
			b[22] = 1
		}
		le.PutUint16(b[24:], c.x)
		le.PutUint16(b[26:], c.y)
	}
	return b
}

package appliance

import (
	"math"
	"time"

	"perimode/native/pkg/hid"
	"perimode/native/pkg/monitor"
)

type sniperGesture struct {
	active, blocked             bool
	ids                         [2]byte
	x, y, distance, angle, turn float64
	start                       monitor.Settings
}

const sniperPressDelay = 90 * time.Millisecond

type sniperPress struct {
	holding, pending bool
	first, last      hid.Contact
	deadline         time.Time
}

// Coordinates have already been mapped into the displayed 16:9 video area.
func (g *sniperGesture) update(s *monitor.Settings, contacts []hid.Contact) bool {
	if len(contacts) == 0 {
		*g = sniperGesture{}
		return false
	}
	if len(contacts) != 2 {
		if len(contacts) > 2 || g.active {
			g.blocked = true
			g.active = false
		}
		return g.blocked
	}
	if g.blocked && !g.active {
		return true
	}
	a, b := contacts[0], contacts[1]
	if a.ID > b.ID {
		a, b = b, a
	}
	if a.ID == b.ID {
		*g = sniperGesture{blocked: true}
		return true
	}
	const aspect = 16.0 / 9.0
	x, y := float64(int(a.X)+int(b.X))/(2*32767), float64(int(a.Y)+int(b.Y))/(2*32767)
	dx, dy := float64(int(b.X)-int(a.X))/32767*aspect, float64(int(b.Y)-int(a.Y))/32767
	distance, angle := math.Hypot(dx, dy), math.Atan2(dy, dx)
	ids := [2]byte{a.ID, b.ID}
	if !g.active || g.ids != ids {
		if distance < .02 {
			return true
		}
		*g = sniperGesture{active: true, blocked: true, ids: ids, x: x, y: y, distance: distance, angle: angle, start: *s}
		return true
	}
	if distance < .02 {
		return true
	}
	ratio := distance / g.distance
	if math.Abs(ratio-1) < .01 {
		ratio = 1
	}
	s.SniperZoom = uint16(max(100, min(1600, int(math.Round(float64(g.start.Zoom())*ratio)))))
	delta := math.Atan2(math.Sin(angle-g.angle), math.Cos(angle-g.angle))
	g.angle = angle
	if g.start.SniperMirror {
		delta = -delta
	}
	g.turn += delta
	// Truncate, not round: fingers must complete a full 15-degree detent.
	// Unwrapped accumulation preserves full turns across atan2's +/-180 edge.
	steps := int(math.Trunc(g.turn/(math.Pi/12) + math.Copysign(1e-9, g.turn)))
	base := (int(g.start.SniperRotation) + 75) / 150 * 150
	rotation := (base + steps*150) % 3600
	if rotation < 0 {
		rotation += 3600
	}
	if steps == 0 {
		s.SniperRotation = g.start.SniperRotation
	} else {
		s.SniperRotation = uint16(rotation)
	}
	// Drag the canvas with the fingers. Magnification reduces desktop travel.
	px, py := (x-g.x)*aspect, y-g.y
	if g.start.SniperMirror {
		px = -px
	}
	radians := float64(s.SniperRotation) * math.Pi / 1800
	sine, cosine := math.Sincos(radians)
	gain := 100 / float64(s.Zoom())
	s.SniperX = int16(max(-5000, min(5000, int(g.start.SniperX)-int(math.Round((cosine*px+sine*py)/aspect*10000*gain)))))
	s.SniperY = int16(max(-5000, min(5000, int(g.start.SniperY)-int(math.Round((-sine*px+cosine*py)*10000*gain)))))
	return true
}

func (u *UI) sniperInput(contacts []hid.Contact) {
	u.sniperInputAt(contacts, time.Now())
}
func (u *UI) sniperInputAt(contacts []hid.Contact, now time.Time) {
	u.state.mu.Lock()
	consumed := false
	if u.state.TouchKind == 0 {
		u.sniperGesture = sniperGesture{blocked: len(contacts) > 0}
		consumed = true
	} else if u.sniperGesture.update(&u.state.Settings, contacts) {
		consumed = true
	}
	u.state.mu.Unlock()
	if consumed {
		u.sniperPress = sniperPress{}
		u.state.SniperInput(nil)
		return
	}
	if len(contacts) == 0 {
		// A short single-finger tap commits on release; a second finger cancels it.
		u.commitSniperPress()
		u.sniperPress = sniperPress{}
		u.state.SniperInput(nil)
		return
	}
	c := contacts[0]
	p := &u.sniperPress
	if !p.holding {
		*p = sniperPress{holding: true, pending: true, first: c, last: c, deadline: now.Add(sniperPressDelay)}
		return
	}
	if c.ID != p.first.ID {
		u.sniperPress = sniperPress{}
		u.sniperGesture = sniperGesture{blocked: true}
		u.state.SniperInput(nil)
		return
	}
	p.last = c
	if p.pending {
		u.flushSniperPress(now)
	} else {
		u.state.SniperInput(contacts)
	}
}
func (u *UI) flushSniperPress(now time.Time) {
	if u.sniperPress.pending && !now.Before(u.sniperPress.deadline) {
		u.commitSniperPress()
	}
}
func (u *UI) commitSniperPress() {
	p := &u.sniperPress
	if !p.pending {
		return
	}
	_, _, page := u.state.Current()
	if page != "" || u.state.ActiveView() != ViewSniper {
		u.sniperPress = sniperPress{}
		return
	}
	u.state.SniperInput([]hid.Contact{p.first})
	if p.last != p.first {
		u.state.SniperInput([]hid.Contact{p.last})
	}
	p.pending = false
}

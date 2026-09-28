package cameractl

import "time"

const IdleLease = 5 * time.Second

// Environment is an immutable snapshot supplied while the appliance lock is held.
// Masks MUST be the intersection of provider admission and the current USB table.
type Environment struct {
	Sensor     byte
	Mode       Mode
	Generation uint64
	Masks      [2]byte
	Flags      uint16
}
type Session struct {
	owner    uint64
	sequence uint32
	lastSeen time.Time
	ack      Message
}

func (s *Session) Reset() { *s = Session{} }
func (s *Session) expire(e Environment, now time.Time) {
	// Never transfer ownership while an old UVC reader can still receive frames.
	if s.owner != 0 && e.Flags&Streaming == 0 && (now.Before(s.lastSeen) || now.Sub(s.lastSeen) > IdleLease) {
		s.owner = 0
		s.sequence = 0
	}
}
func (s *Session) Owner(e Environment, now time.Time) uint64 { s.expire(e, now); return s.owner }

// Fresh does not release a streaming owner. The private transport must first
// cancel its USB requests and confirm completion before clearing Streaming.
func (s *Session) Fresh(token uint64, now time.Time) bool {
	return token != 0 && token == s.owner && !now.Before(s.lastSeen) && now.Sub(s.lastSeen) <= IdleLease
}
func (s *Session) Snapshot(e Environment, now time.Time) Message {
	s.expire(e, now)
	m := s.ack
	m.Kind = Status
	m.Sensor = e.Sensor
	m.Mode = e.Mode
	m.Generation = e.Generation
	m.Masks = e.Masks
	m.Flags = e.Flags
	m.Owner = s.owner
	return m
}

// Apply returns true only for an accepted Acquire. The caller then commits the
// sensor/mode under the SAME lock. No sensor power, USB rebind or media I/O here.
func (s *Session) Apply(c Message, e Environment, now time.Time) (bool, byte) {
	s.expire(e, now)
	result := OK
	change := false
	switch {
	case c.Kind == Status || c.Kind > Keepalive || c.Token == 0 || c.Sequence == 0 || c.Sensor > 1:
		result = Invalid
	case c.Kind == Acquire:
		switch {
		case e.Flags&OwnershipFault != 0:
			result = Unsafe
		case e.Flags&Enabled == 0:
			result = Disabled
		case e.Flags&Paused != 0:
			result = Thermal
		case e.Flags&USBReady == 0 || !Allowed(c.Sensor, c.Mode, e.Masks):
			result = Unavailable
		case (s.owner != 0 && s.owner != c.Token) || (e.Flags&Streaming != 0 && s.owner != c.Token):
			result = Busy
		case s.owner == c.Token && c.Sequence <= s.sequence:
			result = Stale
		case e.Flags&Streaming != 0 && (e.Sensor != c.Sensor || e.Mode != c.Mode):
			result = Busy
		default:
			s.owner = c.Token
			s.sequence = c.Sequence
			s.lastSeen = now
			change = true
		}
	case c.Kind == Keepalive || c.Kind == Release:
		switch {
		case s.owner != c.Token || c.Sequence <= s.sequence:
			result = Stale
		default:
			// The phone may switch the sensor within the negotiated USB format.
			// The token/sequence, not the last observed sensor, owns this stream.
			s.sequence = c.Sequence
			s.lastSeen = now
			if c.Kind == Release {
				s.owner = 0
				s.sequence = 0
			}
		}
	}
	s.ack = Message{Sequence: c.Sequence, Token: c.Token, Result: result}
	return change, result
}

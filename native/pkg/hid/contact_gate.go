package hid

// ContactGate prevents a host feature/mode switch from introducing contacts
// already held down. Only an actual all-up frame opens the new collection.
// It does not synthesize mouse events or Windows gestures.
type ContactGate struct{ waiting bool }

func (g *ContactGate) BlockUntilRelease() { g.waiting = true }
func (g *ContactGate) Accept(count byte) bool {
	if count == 0 {
		g.waiting = false
		return true
	}
	return !g.waiting
}

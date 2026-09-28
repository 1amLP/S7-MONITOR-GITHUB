package appliance

import (
	"fmt"
	"perimode/native/pkg/hid"
	"math"
)

// Port of the supplied TouchMapping.java: scaled contacts share one group
// translation. Extra acceleration applies only to one-contact motion.
type PadMapping struct {
	mask                                   uint32
	offsetX, offsetY, previousX, previousY float64
	previousMS                             int64
}

func (p *PadMapping) Reset() { *p = PadMapping{} }
func (p *PadMapping) Map(in []hid.Contact, ms int64, percent int, acceleration bool) ([]hid.Contact, error) {
	if percent < 25 || percent > 300 || len(in) > hid.MaxContacts {
		return nil, fmt.Errorf("invalid pad sensitivity/contact count")
	}
	if len(in) == 0 {
		p.Reset()
		return nil, nil
	}
	out := append([]hid.Contact(nil), in...)
	mask := uint32(0)
	cx, cy := 0.0, 0.0
	for _, c := range in {
		if c.ID > 31 || c.X > 32767 || c.Y > 32767 || mask&(1<<c.ID) != 0 {
			return nil, fmt.Errorf("invalid pad contact")
		}
		mask |= 1 << c.ID
		cx += float64(c.X)
		cy += float64(c.Y)
	}
	cx /= float64(len(in))
	cy /= float64(len(in))
	gain := float64(percent) / 100
	if p.mask == 0 || p.mask&mask == 0 {
		p.offsetX, p.offsetY = 0, 0
	} else if mask == p.mask {
		dx, dy := cx-p.previousX, cy-p.previousY
		p.offsetX += dx * (gain - 1)
		p.offsetY += dy * (gain - 1)
		if acceleration && len(in) == 1 && ms > p.previousMS {
			speed := math.Hypot(dx, dy) / float64(ms-p.previousMS)
			extra := gain * math.Min(1.5, math.Max(0, (speed-8)/40))
			p.offsetX += dx * extra
			p.offsetY += dy * extra
		}
	}
	bound := func(v float64) uint16 { return uint16(math.Max(0, math.Min(32767, math.Round(v)))) }
	for i := range out {
		out[i].X = bound(float64(in[i].X) + p.offsetX)
		out[i].Y = bound(float64(in[i].Y) + p.offsetY)
	}
	p.mask, p.previousX, p.previousY, p.previousMS = mask, cx, cy, ms
	return out, nil
}

// FitContact maps only the displayed 16:9 region, not letterbox bars. A contact
// beginning on a bar must be suppressed until its physical liftoff by the caller.
func FitContact(c hid.Contact, width, height int) (hid.Contact, bool) {
	if width <= 0 || height <= 0 || c.X > 32767 || c.Y > 32767 {
		return c, false
	}
	dw, dh := width, width*720/1280
	if dh > height {
		dh = height
		dw = height * 1280 / 720
	}
	ox, oy := (width-dw)/2, (height-dh)/2
	x, y := int64(c.X)*int64(width-1)/32767, int64(c.Y)*int64(height-1)/32767
	if x < int64(ox) || y < int64(oy) || x >= int64(ox+dw) || y >= int64(oy+dh) || dw < 2 || dh < 2 {
		return c, false
	}
	c.X = uint16((x - int64(ox)) * 32767 / int64(dw-1))
	c.Y = uint16((y - int64(oy)) * 32767 / int64(dh-1))
	return c, true
}

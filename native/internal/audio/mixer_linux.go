//go:build linux && (amd64 || arm64)

package audio

import (
	"bytes"
	_ "embed"
	"encoding/xml"
	"errors"
	"fmt"
	"perimode/native/internal/linuxio"
	"os"
	"strconv"
	"strings"
	"unsafe"
)

//go:embed mixer_paths_0.xml
var VendorMixer []byte

type RouteElement struct {
	XMLName xml.Name
	Name    string `xml:"name,attr"`
	Value   string `xml:"value,attr"`
}
type Route struct {
	Name     string         `xml:"name,attr"`
	Elements []RouteElement `xml:",any"`
}
type MixerDocument struct {
	XMLName  xml.Name       `xml:"mixer"`
	Defaults []RouteElement `xml:"ctl"`
	Paths    []Route        `xml:"path"`
}

// ExpandRoute preserves the vendor XML order and validates every nested reference.
func ExpandRoute(data []byte, names []string, defaults bool) ([]RouteElement, error) {
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("mixer document too large")
	}
	var doc MixerDocument
	if e := xml.Unmarshal(data, &doc); e != nil {
		return nil, e
	}
	routes := map[string]Route{}
	for _, r := range doc.Paths {
		if r.Name == "" {
			return nil, fmt.Errorf("empty route name")
		}
		if _, ok := routes[r.Name]; ok {
			return nil, fmt.Errorf("duplicate route")
		}
		routes[r.Name] = r
	}
	out := []RouteElement{}
	if defaults {
		out = append(out, doc.Defaults...)
	}
	visiting := map[string]bool{}
	var expand func(string, int) error
	expand = func(name string, depth int) error {
		if depth > 16 || visiting[name] {
			return fmt.Errorf("mixer route cycle/depth")
		}
		r, ok := routes[name]
		if !ok {
			return fmt.Errorf("missing vendor route %s", name)
		}
		visiting[name] = true
		defer delete(visiting, name)
		for _, el := range r.Elements {
			switch el.XMLName.Local {
			case "ctl":
				if len(out) >= 512 {
					return fmt.Errorf("mixer route too long")
				}
				out = append(out, el)
			case "path":
				if e := expand(el.Name, depth+1); e != nil {
					return e
				}
			default:
				return fmt.Errorf("unsupported mixer element %s", el.XMLName.Local)
			}
		}
		return nil
	}
	for _, n := range names {
		if e := expand(n, 0); e != nil {
			return nil, e
		}
	}
	for _, el := range out {
		if el.Name == "" || len(el.Name) >= 44 || el.Value == "" {
			return nil, fmt.Errorf("invalid mixer control")
		}
	}
	return out, nil
}

type ElemInfo [34]uint64
type ElemValue [153]uint64

func (p *ElemInfo) bytes() []byte  { return unsafe.Slice((*byte)(unsafe.Pointer(p)), 272) }
func (p *ElemValue) bytes() []byte { return unsafe.Slice((*byte)(unsafe.Pointer(p)), 1224) }

type Mixer struct{ file *os.File }

func OpenMixer(card int) (*Mixer, error) {
	if card < 0 || card > 15 {
		return nil, fmt.Errorf("invalid ALSA card")
	}
	f, e := os.OpenFile(fmt.Sprintf("/dev/snd/controlC%d", card), os.O_RDWR, 0)
	if e != nil {
		return nil, e
	}
	return &Mixer{file: f}, nil
}
func (m *Mixer) Close() error {
	if m == nil || m.file == nil {
		return nil
	}
	return m.file.Close()
}
func (m *Mixer) info(name string, item uint32) (ElemInfo, error) {
	var info ElemInfo
	if name == "" || len(name) >= 44 {
		return info, fmt.Errorf("invalid control name")
	}
	p := info.bytes()
	le.PutUint32(p[4:], 2)
	copy(p[16:60], name)
	le.PutUint32(p[84:], item)
	e := linuxio.Ioctl(int(m.file.Fd()), 0xc1105511, unsafe.Pointer(&info))
	return info, e
}
func (m *Mixer) Read(name string) (ElemInfo, ElemValue, error) {
	i, e := m.info(name, 0)
	var v ElemValue
	if e != nil {
		return i, v, e
	}
	if le.Uint32(i.bytes()[68:])&3 != 3 {
		return i, v, fmt.Errorf("control %s is not readable+writable", name)
	}
	copy(v.bytes()[:64], i.bytes()[:64])
	e = linuxio.Ioctl(int(m.file.Fd()), 0xc4c85512, unsafe.Pointer(&v))
	return i, v, e
}
func (m *Mixer) Write(v *ElemValue) error {
	return linuxio.Ioctl(int(m.file.Fd()), 0xc4c85513, unsafe.Pointer(v))
}

// MakeValue rejects out-of-range or unresolved enum values. It never guesses an
// enum's numeric value from its position in another card/firmware.
func MakeValue(i *ElemInfo, current ElemValue, text string, enumName func(uint32) (string, error)) (ElemValue, error) {
	p := i.bytes()
	kind, count := le.Uint32(p[64:]), le.Uint32(p[72:])
	v := current
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return v, fmt.Errorf("empty mixer value")
	}
	dst := v.bytes()[72:1096]
	switch kind {
	case 1, 2:
		if count == 0 || count > 128 || (len(parts) != 1 && len(parts) != int(count)) {
			return v, fmt.Errorf("invalid integer control count")
		}
		lo, hi, step := int64(le.Uint64(p[80:])), int64(le.Uint64(p[88:])), int64(le.Uint64(p[96:]))
		for j := uint32(0); j < count; j++ {
			s := parts[0]
			if len(parts) > 1 {
				s = parts[j]
			}
			n, e := strconv.ParseInt(s, 10, 64)
			if e != nil || n < lo || n > hi || (step > 1 && (n-lo)%step != 0) {
				return v, fmt.Errorf("integer mixer value out of range")
			}
			le.PutUint64(dst[j*8:], uint64(n))
		}
	case 3:
		items := le.Uint32(p[80:])
		if count == 0 || count > 128 || items == 0 || items > 512 {
			return v, fmt.Errorf("invalid enum geometry")
		}
		found := false
		for j := uint32(0); j < items; j++ {
			s, e := enumName(j)
			if e != nil {
				return v, e
			}
			if s == text {
				for k := uint32(0); k < count; k++ {
					le.PutUint32(dst[k*4:], j)
				}
				found = true
				break
			}
		}
		if !found {
			return v, fmt.Errorf("unavailable mixer enum %q", text)
		}
	case 4:
		if count == 0 || count > 512 || int(count) != len(parts) {
			return v, fmt.Errorf("byte control count mismatch")
		}
		for j, s := range parts {
			n, e := strconv.ParseUint(s, 10, 8)
			if e != nil {
				return v, e
			}
			dst[j] = byte(n)
		}
	default:
		return v, fmt.Errorf("unsupported mixer value type %d", kind)
	}
	return v, nil
}

type Transaction struct {
	m    *Mixer
	undo []ElemValue
}

// Apply preflights the entire route before any write. Rollback is reverse-order,
// including the last attempted write, because a failed ioctl may have effects.
func (m *Mixer) Apply(route []RouteElement) (tx *Transaction, err error) {
	type operation struct {
		before, after ElemValue
		name          string
		bytes         int
	}
	ops := []operation{}
	// The same control may occur more than once. Simulate prior writes in preflight.
	staged := map[string]ElemValue{}
	for _, el := range route {
		i, v, e := m.Read(el.Name)
		if e != nil {
			return nil, fmt.Errorf("mixer %s: %w", el.Name, e)
		}
		if prior, ok := staged[el.Name]; ok {
			v = prior
		}
		value, e := MakeValue(&i, v, el.Value, func(item uint32) (string, error) {
			x, e := m.info(el.Name, item)
			if e != nil {
				return "", e
			}
			return linuxio.CString(x.bytes()[88:152]), nil
		})
		if e != nil {
			return nil, fmt.Errorf("mixer %s: %w", el.Name, e)
		}
		size := int(le.Uint32(i.bytes()[72:]))
		kind := le.Uint32(i.bytes()[64:])
		switch kind {
		case 1, 2:
			size *= 8
		case 3:
			size *= 4
		case 4:
		default:
			return nil, fmt.Errorf("unsupported mixer type")
		}
		ops = append(ops, operation{v, value, el.Name, size})
		staged[el.Name] = value
	}
	tx = &Transaction{m: m}
	owner := tx
	defer func() {
		if err != nil {
			err = errors.Join(err, owner.Restore())
		}
	}()
	for _, op := range ops {
		if bytes.Equal(op.before.bytes()[72:72+op.bytes], op.after.bytes()[72:72+op.bytes]) {
			continue
		}
		tx.undo = append(tx.undo, op.before)
		v := op.after
		if err = m.Write(&v); err != nil {
			return nil, fmt.Errorf("mixer %s write: %w", op.name, err)
		}
		_, got, e := m.Read(op.name)
		if e != nil {
			return nil, fmt.Errorf("mixer %s readback: %w", op.name, e)
		}
		if !bytes.Equal(got.bytes()[72:72+op.bytes], v.bytes()[72:72+op.bytes]) {
			return nil, fmt.Errorf("mixer %s readback mismatch", op.name)
		}
	}
	return tx, nil
}
func (t *Transaction) Restore() error {
	if t == nil {
		return nil
	}
	var e error
	for i := len(t.undo) - 1; i >= 0; i-- {
		e = errors.Join(e, t.m.Write(&t.undo[i]))
	}
	t.undo = nil
	return e
}

//go:build linux && (amd64 || arm64)

package media

import (
	"fmt"
	"perimode/native/internal/linuxio"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

type Device struct {
	Path               string   `json:"path"`
	Driver             string   `json:"driver"`
	Card               string   `json:"card"`
	Capabilities       uint32   `json:"capabilities"`
	OutputFormats      []string `json:"output_formats"`
	CaptureFormats     []string `json:"capture_formats"`
	OutputEnumeration  uint32   `json:"output_enumeration,omitempty"`
	CaptureEnumeration uint32   `json:"capture_enumeration,omitempty"`
	Error              string   `json:"error,omitempty"`
}

func Probe(path string) Device {
	d := Device{Path: path}
	f, e := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if e != nil {
		d.Error = e.Error()
		return d
	}
	defer f.Close()
	var cap Capability
	if e = ioctl(int(f.Fd()), QueryCap, unsafe.Pointer(&cap)); e != nil {
		d.Error = e.Error()
		return d
	}
	d.Driver = linuxio.CString(cap.Driver[:])
	d.Card = linuxio.CString(cap.Card[:])
	d.Capabilities = cap.Capabilities
	if cap.Capabilities&0x80000000 != 0 {
		d.Capabilities = cap.DeviceCaps
	}
	legacy := d.Driver == "MFC" && (d.Card == "decoder" || d.Card == "encoder")
	for _, t := range []uint32{Output, Capture} {
		formats, used, err := enumerateFormats(t, legacy, func(q *FormatDescription) error { return ioctl(int(f.Fd()), EnumFmt, unsafe.Pointer(q)) })
		if err != nil {
			d.Error = fmt.Sprintf("enum queue%d: %v", t, err)
			return d
		}
		if t == Output {
			d.OutputFormats, d.OutputEnumeration = formats, used
		} else {
			d.CaptureFormats, d.CaptureEnumeration = formats, used
		}
	}
	return d
}

// Samsung 3.18 ENUM_FMT filters by mem_planes, unlike its buffer queue API.
// Compressed one-plane formats are listed via single-plane enumeration only.
// This is discovery, not a switch of S_FMT/REQBUFS/QBUF away from MPLANE.
func enumerateFormats(queue uint32, legacy bool, query func(*FormatDescription) error) ([]string, uint32, error) {
	read := func(kind uint32) ([]string, error) {
		var formats []string
		for i := uint32(0); i < 128; i++ {
			q := FormatDescription{Type: kind, Index: i}
			if err := query(&q); err == syscall.EINVAL {
				return formats, nil
			} else if err != nil {
				return nil, err
			}
			formats = append(formats, FourCC(q.PixelFormat))
		}
		return nil, fmt.Errorf("format enumeration exceeds bound")
	}
	formats, err := read(queue)
	if err != nil || len(formats) > 0 || !legacy {
		return formats, queue, err
	}
	var single uint32
	switch queue {
	case Output:
		single = 2
	case Capture:
		single = 1
	default:
		return nil, queue, fmt.Errorf("unsupported MFC enumeration queue")
	}
	formats, err = read(single)
	return formats, single, err
}
func Discover() []Device {
	p, _ := filepath.Glob("/dev/video*")
	out := make([]Device, 0, len(p))
	for _, x := range p {
		name, e := linuxio.ReadText("/sys/class/video4linux/" + filepath.Base(x) + "/name")
		if e != nil || !strings.Contains(strings.ToLower(name), "mfc") || strings.Contains(strings.ToLower(name), "secure") || strings.Contains(strings.ToLower(name), "drm") {
			// Opening a FIMC-IS node may power the camera. It is deliberately
			// not probed by ioctl while the native camera backend is missing.
			out = append(out, Device{Path: x, Card: strings.TrimSpace(name), Error: "metadata only; non-MFC/unidentified node left unopened"})
			continue
		}
		out = append(out, Probe(x))
	}
	return out
}
func Has(v []string, want string) bool {
	for _, x := range v {
		if x == want {
			return true
		}
	}
	return false
}
func DecoderPath(ds []Device) (string, error) {
	for _, d := range ds {
		if d.Error == "" && !strings.Contains(strings.ToLower(d.Driver+" "+d.Card), "secure") && !strings.Contains(strings.ToLower(d.Driver+" "+d.Card), "drm") && strings.Contains(strings.ToLower(d.Driver+" "+d.Card), "mfc") && Has(d.OutputFormats, "H264") && (Has(d.CaptureFormats, "NM12") || Has(d.CaptureFormats, "NV12")) {
			return d.Path, nil
		}
	}
	return "", fmt.Errorf("no MFC decoder with H264 OUTPUT and linear NV12 CAPTURE found; tiled/protected formats are not guessed")
}

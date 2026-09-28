//go:build linux && (amd64 || arm64)

// Package sspio owns the verified, packed SSP IIO channels. It never runs a
// sensor HAL/lhd, resets the MCU, loads arbitrary firmware, or touches EFS/GPS.
package sspio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"perimode/native/internal/linuxio"
)

type Attributes interface {
	Read(string) (string, error)
	Write(string, string) error
}
type NativeAttributes struct{}

func (NativeAttributes) Read(p string) (string, error) {
	b, e := os.ReadFile(p)
	return strings.TrimSpace(string(b)), e
}
func (NativeAttributes) Write(p, v string) error { return linuxio.WriteAttr(p, v+"\n") }

// A read/modify/write of the shared mask is not atomic in sysfs. Both native
// consumers MUST use this one lock, including acquisition rollback and close.
var controlMu sync.Mutex

type change struct {
	path, before, after string
	mask                uint64
}
type Lease struct {
	io      Attributes
	changes []change
}

func (l *Lease) change(path, value string, mask uint64) error {
	before, e := l.io.Read(path)
	if e != nil {
		return e
	}
	l.changes = append(l.changes, change{path, before, value, mask})
	if e = l.io.Write(path, value); e != nil {
		return e
	}
	got, e := l.io.Read(path)
	if e != nil {
		return e
	}
	if got != value {
		return fmt.Errorf("SSP readback mismatch at %s", path)
	}
	return nil
}
func (l *Lease) closeLocked() error {
	for len(l.changes) > 0 {
		i := len(l.changes) - 1
		c := l.changes[i]
		now, e := l.io.Read(c.path)
		if e != nil {
			return e
		}
		target := c.before
		if c.mask != 0 {
			v, e := strconv.ParseUint(now, 10, 64)
			if e != nil {
				return e
			}
			target = strconv.FormatUint(v&^c.mask, 10)
		} else if now != c.after && now != c.before {
			return fmt.Errorf("SSP ownership changed at %s; not overwritten", c.path)
		}
		if now != target {
			if e = l.io.Write(c.path, target); e != nil {
				return e
			}
			got, e := l.io.Read(c.path)
			if e != nil {
				return e
			}
			if got != target {
				return fmt.Errorf("SSP restore readback failed at %s", c.path)
			}
		}
		// Remove only a confirmed restoration. Failed operations are retryable.
		l.changes = l.changes[:i]
	}
	return nil
}
func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	controlMu.Lock()
	defer controlMu.Unlock()
	return l.closeLocked()
}

// Acquire changes only one verified sensor bit and its own IIO queue. Originals
// are checked first. Partial writes use the same reverse-order cleanup path.
func Acquire(io Attributes, dir, enable string, bit uint64, delayNS uint64) (lease *Lease, err error) {
	if io == nil || (bit != 1 && bit != 1<<9) || delayNS < 50_000_000 || delayNS > 1_000_000_000 {
		return nil, fmt.Errorf("unsupported native SSP lease")
	}
	controlMu.Lock()
	defer controlMu.Unlock()
	enabled, e := io.Read(dir + "/buffer/enable")
	if e != nil {
		return nil, e
	}
	text, e := io.Read(enable)
	if e != nil {
		return nil, e
	}
	mask, e := strconv.ParseUint(text, 10, 64)
	if e != nil {
		return nil, e
	}
	if enabled != "0" || mask&bit != 0 {
		return nil, fmt.Errorf("SSP channel already owned; refusing shared queue mutation")
	}
	length, e := io.Read(dir + "/buffer/length")
	if e != nil {
		return nil, e
	}
	if n, e := strconv.ParseUint(length, 10, 32); e != nil || n == 0 || n > 65536 {
		return nil, fmt.Errorf("SSP original buffer length invalid")
	}
	// Samsung's generic IIO poll_delay show/store callbacks return 0 bytes on
	// this kernel. The per-sensor SSP aliases return the real delay and the
	// write count, so only the pinned native sysfs layout uses those aliases.
	delayPath := dir + "/poll_delay"
	if strings.HasPrefix(dir, "/sys/bus/iio/devices/iio:device") && enable == "/sys/class/sensors/ssp_sensor/enable" {
		if bit == 1 {
			delayPath = "/sys/class/sensors/ssp_sensor/accel_poll_delay"
		} else {
			delayPath = "/sys/class/sensors/ssp_sensor/light_poll_delay"
		}
	}
	delay, e := io.Read(delayPath)
	if e != nil {
		return nil, e
	}
	if n, e := strconv.ParseUint(delay, 10, 64); e != nil || n == 0 || n > 60_000_000_000 {
		return nil, fmt.Errorf("SSP original poll delay invalid")
	}
	l := &Lease{io: io}
	defer func() {
		if err != nil {
			restoreErr := l.closeLocked()
			err = errors.Join(err, restoreErr)
			if restoreErr != nil {
				lease = l
			}
		}
	}()
	for _, x := range []struct {
		p, v string
		mask uint64
	}{
		{dir + "/buffer/length", "16", 0}, {delayPath, strconv.FormatUint(delayNS, 10), 0},
		{enable, strconv.FormatUint(mask|bit, 10), bit}, {dir + "/buffer/enable", "1", 0},
	} {
		if err = l.change(x.p, x.v, x.mask); err != nil {
			return nil, err
		}
	}
	return l, nil
}

type Profile byte

const (
	Accelerometer Profile = iota
	Light
)

func (p Profile) config() (name string, identities []string, bit, period uint64, err error) {
	switch p {
	case Accelerometer:
		return "accelerometer_sensor", []string{"K6DS3TR"}, 1, 50_000_000, nil
	case Light:
		return "light_sensor", []string{"TMD4903", "TMD4904"}, 1 << 9, 200_000_000, nil
	default:
		return "", nil, 0, 0, fmt.Errorf("unknown SSP profile")
	}
}
func Discover(sysRoot string, p Profile) (string, error) {
	name, ids, _, _, e := p.config()
	if e != nil {
		return "", e
	}
	raw, e := os.ReadFile(filepath.Join(sysRoot, "class/sensors", name, "name"))
	if e != nil {
		return "", fmt.Errorf("SSP %s identity unavailable: %w", name, e)
	}
	identity := strings.TrimSpace(string(raw))
	verified := false
	for _, s := range ids {
		verified = verified || identity == s
	}
	if !verified {
		return "", fmt.Errorf("unverified SSP %s identity %q", name, identity)
	}
	paths, e := filepath.Glob(filepath.Join(sysRoot, "bus/iio/devices/iio:device*/name"))
	if e != nil {
		return "", e
	}
	found := ""
	for _, path := range paths {
		b, e := os.ReadFile(path)
		if e != nil || strings.TrimSpace(string(b)) != name {
			continue
		}
		dir := filepath.Dir(path)
		n, e := strconv.Atoi(strings.TrimPrefix(filepath.Base(dir), "iio:device"))
		if e != nil || n < 0 || n > 4096 {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("multiple SSP %s channels; refusing to guess", name)
		}
		found = dir
	}
	if found == "" {
		return "", fmt.Errorf("SSP %s IIO channel missing; native sensor-hub startup required", name)
	}
	return found, nil
}

type Channel struct {
	File  *os.File
	FD    int
	Lease *Lease
	Label string
	Clock *SampleClock
}

func (c *Channel) Close() error {
	if c == nil {
		return nil
	}
	if e := c.Lease.Close(); e != nil {
		return e
	}
	if c.File != nil {
		f := c.File
		c.File = nil
		return f.Close()
	}
	return nil
}
func Open(p Profile) (ch *Channel, err error) {
	dir, e := Discover("/sys", p)
	if e != nil {
		return nil, e
	}
	path := "/dev/" + filepath.Base(dir)
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	defer func() {
		if err != nil && ch == nil {
			f.Close()
		}
	}()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if st.Mode()&os.ModeCharDevice == 0 {
		return nil, fmt.Errorf("SSP IIO path is not a character device")
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("IIO device stat unavailable")
	}
	raw, e := os.ReadFile(dir + "/dev")
	if e != nil {
		return nil, e
	}
	major := ((stat.Rdev >> 8) & 0xfff) | ((stat.Rdev >> 32) & 0xfffff000)
	minor := (stat.Rdev & 0xff) | ((stat.Rdev >> 12) & 0xffffff00)
	if strings.TrimSpace(string(raw)) != fmt.Sprintf("%d:%d", major, minor) {
		return nil, fmt.Errorf("IIO sysfs/device identity mismatch")
	}
	fd := int(f.Fd())
	if e = syscall.SetNonblock(fd, true); e != nil {
		return nil, e
	}
	name, _, bit, period, _ := p.config()
	maxAge := 750 * time.Millisecond
	if p == Light {
		maxAge = 2 * time.Second
	}
	// Establish the lower timestamp bound BEFORE enabling the sensor. Failure
	// does not mutate the shared SSP mask or queue; old records cannot seed it.
	clock, e := NewSampleClock(nil, maxAge)
	if e != nil {
		return nil, e
	}
	lease, e := Acquire(NativeAttributes{}, dir, "/sys/class/sensors/ssp_sensor/enable", bit, period)
	if e != nil && lease == nil {
		return nil, e
	}
	return &Channel{File: f, FD: fd, Lease: lease, Label: path + " / " + name, Clock: clock}, e
}

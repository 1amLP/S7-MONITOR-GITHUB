//go:build linux && (amd64 || arm64)

// Package torch drives only the normal rear-torch switch from the supplied
// herolte HAL. It does not expose factory brightness values or strobe mode.
package torch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const RearPath = "/sys/class/camera/flash/rear_torch_flash"
const sysfsMagic = 0x62656572

type Driver interface {
	Set(bool) error
	Close() error
}
type Sysfs struct{ file *os.File }

// OpenRear validates the real filesystem and opens an existing sysfs attribute.
// Host files with the same spelling cannot pass this check. The descriptor is
// held for this session, so attribute replacement cannot redirect writes.
func OpenRear() (Driver, error) {
	p, e := filepath.EvalSymlinks(RearPath)
	if e != nil {
		return nil, e
	}
	if !strings.HasPrefix(p, "/sys/devices/") {
		return nil, fmt.Errorf("torch attribute outside sysfs devices")
	}
	fd, e := syscall.Open(p, syscall.O_WRONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), p)
	fail := func(e error) (Driver, error) { _ = f.Close(); return nil, e }
	var st syscall.Statfs_t
	if e = syscall.Fstatfs(fd, &st); e != nil {
		return fail(e)
	}
	if uint64(st.Type) != sysfsMagic {
		return fail(fmt.Errorf("torch attribute is not sysfs"))
	}
	fi, e := f.Stat()
	if e != nil {
		return fail(e)
	}
	if !fi.Mode().IsRegular() {
		return fail(fmt.Errorf("torch attribute is not regular sysfs attribute"))
	}
	d := &Sysfs{file: f}
	if e = d.Set(false); e != nil {
		return fail(fmt.Errorf("initial torch OFF: %w", e))
	}
	return d, nil
}
func (d *Sysfs) Set(on bool) error {
	if d == nil || d.file == nil {
		return fmt.Errorf("torch driver closed")
	}
	b := []byte{'0'}
	if on {
		b[0] = '1'
	}
	// Match the existing firmware's one-byte command. No O_TRUNC, creation,
	// level numbers, HBM, flash mode, or unrelated sysfs node is written.
	if _, e := d.file.Seek(0, io.SeekStart); e != nil {
		return e
	}
	n, e := d.file.Write(b)
	if e == nil && n != len(b) {
		e = io.ErrShortWrite
	}
	return e
}
func (d *Sysfs) Close() error {
	if d == nil || d.file == nil {
		return nil
	}
	e := d.Set(false)
	ce := d.file.Close()
	d.file = nil
	return errors.Join(e, ce)
}

//go:build linux && (amd64 || arm64)

package mediaruntime

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

var initHardening = [...]struct {
	path string
	want int
}{
	{"/proc/sys/kernel/kptr_restrict", 4},
	{"/proc/sys/vm/mmap_rnd_bits", 24},
	{"/proc/sys/vm/mmap_rnd_compat_bits", 16},
}

// Original init writes and reads back these three hardening sysctls even if
// already set. Retain their real kernel inodes across the read-only /proc/sys
// bind; no fake success files, global subtree exposure, or thermal controls.
func isolateProcControls(root string) (err error) {
	if root != Root {
		return fmt.Errorf("unexpected private media root")
	}
	var refs []*os.File
	defer func() {
		for _, f := range refs {
			if e := f.Close(); err == nil {
				err = e
			}
		}
	}()
	var sources [3]string
	for i, c := range initHardening {
		f, openErr := os.Open(root + c.path)
		if openErr != nil {
			return openErr
		}
		refs = append(refs, f)
		read := func() ([]byte, error) {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return nil, err
			}
			return io.ReadAll(io.LimitReader(f, 33))
		}
		write := func(b []byte) error {
			w, err := os.OpenFile(root+c.path, os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			_, err = w.Write(b)
			closeErr := w.Close()
			if err != nil {
				return err
			}
			return closeErr
		}
		if err := raiseInitHardening(c.path, c.want, read, write); err != nil {
			return err
		}
		sources[i] = fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	}
	return protectProcControls(root, sources, syscall.Mount)
}

// The reduced property set need not advertise the full Android ABI list.
// Establish protection before starting any native library, including ARM32.
// Never lower an existing value or accept an unconfirmed kernel write.
func raiseInitHardening(path string, want int, read func() ([]byte, error), write func([]byte) error) error {
	b, err := read()
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 || n > want {
		return fmt.Errorf("unexpected initial hardening value %s: %q", path, b)
	}
	if n == want {
		return nil
	}
	if err = write([]byte(strconv.Itoa(want) + "\n")); err != nil {
		return fmt.Errorf("raise hardening %s: %w", path, err)
	}
	b, err = read()
	if err != nil {
		return err
	}
	n, err = strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n != want {
		return fmt.Errorf("hardening write unconfirmed %s: %q", path, b)
	}
	return nil
}

func protectProcControls(root string, sources [3]string, mount func(string, string, string, uintptr, string) error) error {
	if root != Root {
		return fmt.Errorf("unexpected private media root")
	}
	for _, p := range []string{"/proc/sys", "/proc/sysrq-trigger", "/proc/irq"} {
		if err := mount(root+p, root+p, "", syscall.MS_BIND, ""); err != nil {
			return err
		}
		if err := mount("", root+p, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID, ""); err != nil {
			return err
		}
	}
	for i, c := range initHardening {
		if err := mount(sources[i], root+c.path, "", syscall.MS_BIND, ""); err != nil {
			return err
		}
	}
	return nil
}

func verifyInitHardening(read func(string) ([]byte, error)) error {
	for _, c := range initHardening {
		b, err := read(c.path)
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || n != c.want {
			return fmt.Errorf("native init hardening not confirmed %s: %q", c.path, b)
		}
	}
	return nil
}

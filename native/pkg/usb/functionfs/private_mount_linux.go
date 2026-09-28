package functionfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// MountPrivate never reuses or overmounts an existing directory, including ADB.
func MountPrivate(name, path, fileSystem string) (func() error, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00 \t\n") {
		return nil, errors.New("invalid private FunctionFS instance name")
	}
	if filepath.Clean(path) != path || filepath.Dir(path) != "/dev" || !strings.HasPrefix(filepath.Base(path), "s7-") {
		return nil, errors.New("private FunctionFS mount must be a new /dev/s7-* directory")
	}
	if fileSystem != "functionfs_s7" && fileSystem != "functionfs" {
		return nil, errors.New("unsupported FunctionFS filesystem")
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, err
	}
	if err := syscall.Mount(name, path, fileSystem, syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "uid=0,gid=0,rmode=0700,fmode=0600"); err != nil {
		return nil, errors.Join(err, os.Remove(path))
	}
	mounted, removed := true, false
	cleanup := func() error {
		if mounted {
			if err := syscall.Unmount(path, 0); err != nil {
				return fmt.Errorf("unmount private FunctionFS: %w", err)
			}
			mounted = false
		}
		if !removed {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			removed = true
		}
		return nil
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, errors.Join(err, cleanup())
	}
	if stat.Type != 0x0a647361 {
		return nil, errors.Join(errors.New("mounted filesystem is not FunctionFS"), cleanup())
	}
	return cleanup, nil
}

//go:build linux && (amd64 || arm64)

package mediaruntime

import (
	"fmt"
	"os"
	"syscall"
)

// ChildMain has already created a private mount/PID namespace. A chroot alone
// is lost when Android init calls setns: Linux 3.18 resets fs->root to the
// namespace root. Overmount that root with the prepared media mount first.
// MS_MOVE also carries its private /dev, /proc and read-only SELinux mounts.
func enterMediaRoot() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("media root transition requires namespace PID1")
	}
	if err := os.Chdir(Root); err != nil {
		return err
	}
	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move private media mount to namespace root: %w", err)
	}
	if err := syscall.Chroot("."); err != nil {
		return err
	}
	return os.Chdir("/")
}

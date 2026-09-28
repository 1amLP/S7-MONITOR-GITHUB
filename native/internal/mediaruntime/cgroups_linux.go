//go:build linux && (amd64 || arm64)

package mediaruntime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The supplied Android 14 libprocessgroup creates uid_*/pid_* groups in the
// unified hierarchy, even on the pinned S7 3.18 kernel. That kernel exposes its
// early unified ABI as cgroup + __DEVEL__sane_behavior, not a cgroup2 fs type.
// Only the mount API is adapted. cgroup.procs remains a real kernel file.
const groupsBase = "/run/s7-media-groups"
const groupsMount = groupsBase + "/kernel"
const groupsLeaf = groupsMount + "/s7-native-media"
const groupsInside = "/dev/s7-cgroup"

type groupOps interface {
	Mkdir(string, os.FileMode) error
	ReadDir(string) ([]os.DirEntry, error)
	ReadFile(string) ([]byte, error)
	WriteFile(string, []byte, os.FileMode) error
	Remove(string) error
	Mount(string, string, string, uintptr, string) error
	Unmount(string, int) error
}

type realGroups struct{}

func (realGroups) Mkdir(p string, m os.FileMode) error     { return os.Mkdir(p, m) }
func (realGroups) ReadDir(p string) ([]os.DirEntry, error) { return os.ReadDir(p) }
func (realGroups) ReadFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Control files are small; never consume an unbounded list of foreign tasks.
	b, err := io.ReadAll(io.LimitReader(f, 32769))
	if len(b) > 32768 {
		return nil, errors.New("cgroup file exceeds bound")
	}
	return b, err
}
func (realGroups) WriteFile(p string, b []byte, m os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, m)
	if err != nil {
		return err
	}
	_, we := f.Write(b)
	return errors.Join(we, f.Close())
}
func (realGroups) Remove(p string) error { return os.Remove(p) }
func (realGroups) Mount(s, t, f string, flags uintptr, data string) error {
	return syscall.Mount(s, t, f, flags, data)
}
func (realGroups) Unmount(p string, flags int) error { return syscall.Unmount(p, flags) }

type groupOwner struct {
	ops                           groupOps
	base, mountDir, mounted, leaf bool
}

// Provision only a fresh owned subtree. It neither moves the outer PID1 nor
// writes CPU/memory/freezer/thermal policy. No resource controllers are enabled.
func prepareGroups(ops groupOps) (_ *groupOwner, err error) {
	g := &groupOwner{ops: ops}
	defer func() {
		if err != nil {
			err = errors.Join(err, g.Close())
		}
	}()
	if err = ops.Mkdir(groupsBase, 0700); err != nil {
		return nil, err
	}
	g.base = true
	if err = ops.Mkdir(groupsMount, 0700); err != nil {
		return nil, err
	}
	g.mountDir = true
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
	err = ops.Mount("s7-media-cgroups", groupsMount, "cgroup2", flags, "")
	// Permission, busy, and malformed mount errors are not a reason to change API.
	if errors.Is(err, syscall.ENODEV) {
		err = ops.Mount("s7-media-cgroups", groupsMount, "cgroup", flags, "__DEVEL__sane_behavior")
	}
	if err != nil {
		return nil, fmt.Errorf("mount real unified process groups: %w", err)
	}
	g.mounted = true
	for _, file := range []string{"cgroup.controllers", "cgroup.subtree_control", "cgroup.procs"} {
		if _, err = ops.ReadFile(groupsMount + "/" + file); err != nil {
			return nil, fmt.Errorf("unified kernel ABI %s: %w", file, err)
		}
	}
	if err = ops.Mkdir(groupsLeaf, 0755); err != nil {
		return nil, err
	}
	g.leaf = true
	if b, e := ops.ReadFile(groupsLeaf + "/cgroup.procs"); e != nil || len(strings.TrimSpace(string(b))) != 0 {
		return nil, fmt.Errorf("new process group is not empty: %w", errors.Join(e, syscall.EBUSY))
	}
	// Verify the real leaf has the same hierarchy interface before exporting it.
	if _, err = ops.ReadFile(groupsLeaf + "/cgroup.subtree_control"); err != nil {
		return nil, err
	}
	return g, nil
}

// Called only after namespace exit is confirmed. Original init may leave
// uid_*/pid_* directories after a kill. Remove empty directories, never files or
// tasks. A busy group retains its owner and mount for a later cleanup attempt.
func (g *groupOwner) Close() error {
	if g == nil {
		return nil
	}
	if g.leaf {
		budget := 4096
		if e := pruneEmptyGroups(g.ops, groupsLeaf, 0, &budget); e != nil {
			return e
		}
		g.leaf = false
	}
	if g.mounted {
		if e := g.ops.Unmount(groupsMount, 0); e != nil {
			return e
		}
		g.mounted = false
	}
	if g.mountDir {
		if e := g.ops.Remove(groupsMount); e != nil {
			return e
		}
		g.mountDir = false
	}
	if g.base {
		if e := g.ops.Remove(groupsBase); e != nil {
			return e
		}
		g.base = false
	}
	return nil
}
func pruneEmptyGroups(ops groupOps, p string, depth int, budget *int) error {
	if depth > 8 || *budget <= 0 {
		return errors.New("owned cgroup cleanup limit reached")
	}
	*budget--
	b, e := ops.ReadFile(p + "/cgroup.procs")
	if e != nil {
		return e
	}
	if strings.TrimSpace(string(b)) != "" {
		return fmt.Errorf("owned process group still has tasks: %w", syscall.EBUSY)
	}
	entries, e := ops.ReadDir(p)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected link in kernel cgroup")
		}
		if !entry.IsDir() {
			continue
		} // kernel control files must never be unlinked
		name := entry.Name()
		if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
			return errors.New("unexpected cgroup directory name")
		}
		if e := pruneEmptyGroups(ops, filepath.Join(p, name), depth+1, budget); e != nil {
			return e
		}
	}
	return ops.Remove(p)
}

// ABI of the PINNED libcgrouprc.so (both ARM32 and ARM64):
// header {u32 version=1, u32 count}; entries of exactly 56 bytes:
// {u32 hierarchyVersion, u32 flags, char name[16], char path[32]}.
// Mounted flag is 1. This is published only AFTER real mounts have succeeded.
func groupTable() []byte {
	b := make([]byte, 8+56)
	binary.LittleEndian.PutUint32(b[0:4], 1)
	binary.LittleEndian.PutUint32(b[4:8], 1)
	binary.LittleEndian.PutUint32(b[8:12], 2)
	binary.LittleEndian.PutUint32(b[12:16], 1)
	copy(b[16:32], "cgroup2")
	copy(b[32:64], groupsInside)
	return b
}
func installGroups(root string, ops groupOps) error {
	// The parent has mounted and owns groupsLeaf. The child exports only that
	// subtree, never the hierarchy root containing the outer system's processes.
	if e := ops.Mkdir(root+groupsInside, 0755); e != nil {
		return e
	}
	if e := ops.Mount(groupsLeaf, root+groupsInside, "", syscall.MS_BIND, ""); e != nil {
		return e
	}
	if e := ops.Mkdir(root+"/dev/cgroup_info", 0755); e != nil {
		return e
	}
	// CgroupSetup in the pinned Android init sees cgroup.rc and consumes it
	// instead of attempting unsupported global cgroup2/controller setup again.
	return ops.WriteFile(root+"/dev/cgroup_info/cgroup.rc", groupTable(), 0444)
}

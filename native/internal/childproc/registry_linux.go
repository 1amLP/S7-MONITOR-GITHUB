//go:build linux && (amd64 || arm64)

// Package childproc shares child ownership between PID1's codec and sensor
// helpers. An orphan reaper must never steal an os/exec-owned child's status.
package childproc

import (
	"errors"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

var children = struct {
	sync.Mutex
	owned map[int]bool
}{owned: make(map[int]bool)}

// Inspection must not wait for a child launch or an orphan scan.
func OwnedPIDs() ([]int, bool) {
	if !children.TryLock() {
		return nil, false
	}
	pids := make([]int, 0, len(children.owned))
	for pid := range children.owned {
		pids = append(pids, pid)
	}
	children.Unlock()
	sort.Ints(pids)
	return pids, true
}

func Start(c *exec.Cmd) error {
	children.Lock()
	defer children.Unlock()
	if err := c.Start(); err != nil {
		return err
	}
	children.owned[c.Process.Pid] = true
	return nil
}

// Wait must be called exactly once for each command successfully started here.
func Wait(c *exec.Cmd) error {
	err := c.Wait()
	children.Lock()
	delete(children.owned, c.Process.Pid)
	children.Unlock()
	return err
}
func ReapOrphans(proc string) error {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return err
	}
	children.Lock()
	defer children.Unlock()
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil || pid <= 1 || children.owned[pid] {
			continue
		}
		b, e := os.ReadFile(proc + "/" + entry.Name() + "/stat")
		if e != nil {
			continue
		}
		cut := strings.LastIndexByte(string(b), ')')
		if cut < 0 {
			continue
		}
		fields := strings.Fields(string(b[cut+1:]))
		if len(fields) < 2 || fields[0] != "Z" {
			continue
		}
		ppid, e := strconv.Atoi(fields[1])
		if e != nil || ppid != os.Getpid() {
			continue
		}
		var status syscall.WaitStatus
		_, e = syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if e != nil && !errors.Is(e, syscall.ECHILD) && !errors.Is(e, syscall.ESRCH) {
			return e
		}
	}
	return nil
}

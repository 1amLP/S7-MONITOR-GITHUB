//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Engineering reports contain logs/counters, never camera or monitor pixels.
// Save before leaving native: this Recovery kernel cannot retain last_kmsg.
func (u *UI) saveExitReport(phase string, stopErr error) error {
	if u.debug == nil || u.cache == nil || u.cache.Settings == nil {
		return nil
	}
	if err := u.cache.Settings.checkDir(); err != nil {
		return err
	}
	errText := ""
	if stopErr != nil {
		errText = stopErr.Error()
	}
	snapshot := u.state.Snapshot()
	if phase == "usb-stall" {
		if link, ok := snapshot["monitor_link"].(MonitorLinkStatus); !ok || link.Phase != "lost" {
			return nil
		}
	}
	report := map[string]any{"schema": "S7-NATIVE-EXIT-1", "at": time.Now().UTC(), "phase": phase, "shutdown_action": u.shutdownAction,
		"stop_error": errText, "runtime": snapshot, "native_messages": RuntimeMessages(), "kernel_messages": KernelMessages()}
	if phase == "usb-stall" {
		stack := make([]byte, 64<<10)
		n := runtime.Stack(stack, true)
		report["goroutines"] = string(stack[:n])
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if len(data) > 256<<10 {
		return fmt.Errorf("exit report exceeds bound")
	}
	dir := u.cache.Settings.Directory
	name := "last-exit.json"
	if phase == "before-worker-stop" {
		name = "last-exit-before-stop.json"
	} else if phase == "usb-stall" {
		name = "last-usb-stall.json"
	}
	path := filepath.Join(dir, name)
	if st, e := os.Lstat(path); e == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("unsafe exit-report destination")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	f, err := os.CreateTemp(dir, ".exit-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// At most three sustained-loss records per engineering boot, one bounded file.
// A short driver update must not consume the only chance to capture a real loss.
// This worker is joined before CACHE closes; disk I/O never runs in the UI loop.
func (u *UI) usbStallReportWorker(ctx context.Context) error {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var lostSince time.Time
	recorded, captures := false, 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-tick.C:
			v := u.state.MonitorLink(now)
			if v.Phase != "lost" || !v.Bound || !v.Enumerated || v.Pending {
				lostSince = time.Time{}
				recorded = false
				continue
			}
			if lostSince.IsZero() {
				lostSince = now
			}
			if !recorded && now.Sub(lostSince) >= 2*time.Second {
				if err := u.saveExitReport("usb-stall", nil); err != nil {
					return err
				}
				recorded = true
				captures++
				if captures >= 3 {
					return nil
				}
			}
		}
	}
}

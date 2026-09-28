//go:build linux && (amd64 || arm64)

package appliance

import (
	"encoding/json"
	"fmt"
	"perimode/native/internal/journal"
	"os"
	"strings"
	"time"
)

// Fault reports a runtime failure without ever waiting for storage. Invalid menu
// selections continue to use Error, not Fault. First-fault collection starts only
// after diagnostic-specific opt-in; the queue holds exactly one event.
func (s *State) Fault(subsystem string, e error) {
	if e == nil {
		return
	}
	s.Error(e)
	s.mu.Lock()
	enabled := s.diagnosticEnabled
	ch := s.faults
	generation := s.faultGeneration
	s.mu.Unlock()
	if !enabled || ch == nil {
		return
	}
	ev := journal.Event{Subsystem: subsystem, Kind: "runtime-error", Message: e.Error(), AtUnixMS: time.Now().UnixMilli()}
	snapshot := s.Snapshot()
	delete(snapshot, "first_fault")
	b, err := json.Marshal(snapshot)
	if err == nil && len(b) <= 16384 {
		ev.Runtime = b
	}
	select {
	case ch <- faultEnvelope{generation, ev}:
	default:
	}
}
func (u *UI) diagnosticStatus(status string) {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	u.state.DiagnosticStatus = status
	if u.diagnosticStore != nil {
		u.state.FirstFault = u.diagnosticStore.First()
	}
}
func (u *UI) openDiagnostics(explicit bool) error {
	if u.cache == nil || u.cache.Settings == nil {
		return fmt.Errorf("enable CACHE persistence before diagnostics")
	}
	if u.diagnosticStore != nil {
		u.state.mu.Lock()
		on := u.state.diagnosticEnabled
		u.state.mu.Unlock()
		if on {
			return nil
		}
	}
	b, e := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if e != nil {
		return e
	}
	d, e := journal.Open(u.cache.Settings.Directory, PinnedSerial, strings.TrimSpace(string(b)), explicit)
	if e != nil {
		return e
	}
	u.diagnosticStore = d // Preserve this handle so a corrupt record can be explicitly cleared.
	e = d.Begin(time.Now())
	u.state.mu.Lock()
	u.state.diagnosticEnabled = e == nil
	u.state.mu.Unlock()
	if e != nil {
		u.diagnosticStatus("READ/WRITE ERROR: " + e.Error())
		return e
	}
	u.diagnosticStatus("ENABLED / FIRST FAULT ONLY")
	return nil
}
func (u *UI) writeFault(pending faultEnvelope) {
	u.state.mu.Lock()
	valid := u.state.diagnosticEnabled && pending.Generation == u.state.faultGeneration
	u.state.mu.Unlock()
	if !valid || u.diagnosticStore == nil {
		return
	}
	_, e := u.diagnosticStore.RecordFirst(pending.Event)
	if e != nil {
		u.diagnosticStatus("FIRST-FAULT WRITE ERROR: " + e.Error())
		return
	}
	u.diagnosticStatus("FIRST FAULT SAVED")
}
func (u *UI) flushFault() {
	if u.state.faults == nil {
		return
	}
	select {
	case ev := <-u.state.faults:
		u.writeFault(ev)
	default:
	}
}
func (u *UI) diagnosticLines() []string {
	u.state.mu.Lock()
	status, first := u.state.DiagnosticStatus, u.state.FirstFault
	u.state.mu.Unlock()
	detail := "NO SAVED FAULT"
	if first != nil {
		detail = first.Event.Subsystem + " / " + first.Event.Kind + ": " + first.Event.Message
	}
	return []string{"DEVICE / FIRST-FAULT LOG", "ENABLE PERSISTENT DIAGNOSTICS", "CLEAR SAVED FIRST FAULT", "STATUS: " + status, detail, "FIRST FAULT IS NOT REPLACED BY LATER ERRORS", "NO VIDEO / AUDIO / CONTINUOUS DISK LOG", "DISABLE PERSISTENT DIAGNOSTICS", "BACK: STORAGE"}
}

func (u *UI) resetFaultQueue() {
	u.state.mu.Lock()
	u.state.diagnosticEnabled = false
	u.state.faultGeneration++
	u.state.mu.Unlock()
	for {
		select {
		case <-u.state.faults:
		default:
			return
		}
	}
}
func (u *UI) clearDiagnostics() error {
	if u.diagnosticStore == nil {
		return fmt.Errorf("persistent diagnostics are not open")
	}
	u.resetFaultQueue()
	if e := u.diagnosticStore.Clear(); e != nil {
		u.diagnosticStatus("CLEAR ERROR: " + e.Error())
		return e
	}
	if e := u.diagnosticStore.Begin(time.Now()); e != nil {
		u.diagnosticStatus("BEGIN ERROR: " + e.Error())
		return e
	}
	u.state.mu.Lock()
	u.state.diagnosticEnabled = true
	u.state.mu.Unlock()
	u.diagnosticStatus("ENABLED / FIRST FAULT CLEARED")
	return nil
}
func (u *UI) disableDiagnostics() error {
	if u.diagnosticStore == nil {
		return fmt.Errorf("persistent diagnostics are not open")
	}
	u.resetFaultQueue()
	if e := u.diagnosticStore.Disable(); e != nil {
		u.diagnosticStatus("DISABLE ERROR: " + e.Error())
		return e
	}
	u.diagnosticStatus("DISABLED / OLD EVIDENCE RETAINED")
	u.diagnosticStore = nil
	return nil
}

package appliance

import (
	"fmt"
	"time"
)

const linkHeartbeatLimit = 3 * time.Second
const linkReplyLimit = 8 * time.Second

// All fields are guarded by State.mu, never Transport.mu. No filesystem/USB
// operation is performed while holding this lock. Binding is NOT driver readiness.
type monitorLinkRuntime struct {
	Available, Bound, Enumerated, Suspended bool
	Pending, Connecting, HadReply           bool
	Started, EnumeratedAt                   time.Time
	Error                                   string
}

type MonitorLinkStatus struct {
	Phase         string `json:"phase"`
	Title         string `json:"title"`
	Detail        string `json:"detail"`
	Available     bool   `json:"available"`
	Bound         bool   `json:"usb_bound"`
	Enumerated    bool   `json:"usb_enumerated"`
	Pending       bool   `json:"operation_pending"`
	DriverReplied bool   `json:"monitor_driver_replied"`
	VideoRecent   bool   `json:"video_recent"`
	Error         string `json:"error,omitempty"`
}

func recentLink(now, then time.Time, limit time.Duration) bool {
	return !then.IsZero() && !now.Before(then) && now.Sub(then) < limit
}

// Called only with State.mu held. No claim about camera/audio driver installation.
func (s *State) monitorLinkLocked(now time.Time) MonitorLinkStatus {
	l := s.link
	v := MonitorLinkStatus{Available: l.Available, Bound: l.Bound,
		Enumerated: l.Enumerated, Pending: l.Pending, Error: l.Error}
	phase := func(p, t, d string) MonitorLinkStatus {
		v.Phase, v.Title, v.Detail = p, t, d
		return v
	}
	if l.Pending {
		if l.Connecting {
			return phase("connecting", "Connecting S7 Monitor", "Home request / Opening USB")
		}
		return phase("disconnecting", "Disconnecting S7 Monitor", "Releasing USB / Please wait")
	}
	if l.Error != "" {
		return phase("error", "Connection failed", "USB connection error")
	}
	if !l.Available {
		return phase("unavailable", "USB unavailable", "Open Device / Link details")
	}
	if !l.Bound {
		return phase("disconnected", "S7 Monitor disconnected", "USB session disconnected")
	}
	if l.Suspended {
		return phase("suspended", "PC connection suspended", "Waiting for the PC to resume")
	}
	if !l.Enumerated {
		if l.HadReply {
			return phase("lost", "PC connection lost", "USB is armed / Waiting for reconnection")
		}
		return phase("waiting_usb", "Waiting for USB", "Connect a data cable to the PC")
	}
	poll := recentLink(now, s.LastPoll, linkHeartbeatLimit)
	v.DriverReplied = poll && s.Ack == s.Generation && s.HostState <= 2 && s.HostError == 0
	v.VideoRecent = poll && s.Settings.Enabled && s.Consumer &&
		recentLink(now, s.LastBlit, 2*time.Second)
	if s.Ack == s.Generation && poll && (s.HostState == 3 || s.HostError != 0) {
		v.Error = fmt.Sprintf("Monitor driver status %d / 0x%08X", s.HostState, s.HostError)
		return phase("driver_error", "Monitor driver reported an error", "Windows driver error")
	}
	if s.ThermalPaused {
		return phase("thermal", "Monitor paused for temperature", "Thermal protections remain active")
	}
	if v.DriverReplied || v.VideoRecent {
		// One stable phase: changes between active/idle must not replay the wave.
		return phase("ready", "S7 Monitor connected", "Windows monitor driver replied")
	}
	if !s.LastPoll.IsZero() && !poll && l.HadReply {
		return phase("lost", "Monitor link lost", "No fresh driver heartbeat")
	}
	if recentLink(now, l.EnumeratedAt, linkReplyLimit) {
		return phase("checking_driver", "Checking monitor link", "Waiting for the Windows monitor driver")
	}
	return phase("no_reply", "No monitor driver response", "Driver absent, blocked or not responding")
}

func (s *State) MonitorLink(now time.Time) MonitorLinkStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.monitorLinkLocked(now)
}
func (s *State) setLinkAvailable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.link.Available = true
}
func (s *State) setLinkBound(bound bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.link.Bound = bound
	if !bound {
		s.link.Enumerated = false
		s.link.Suspended = false
	}
}
func (s *State) linkUSBEvent(enabled, suspended bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if enabled && (!s.link.Enumerated || s.link.Suspended) {
		s.link.EnumeratedAt = now
	}
	s.link.Enumerated = enabled
	s.link.Suspended = suspended
	if !enabled || suspended {
		s.Ack, s.HostState, s.HostError = 0, 0, 0
		s.LastPoll, s.LastBlit = time.Time{}, time.Time{}
	}
}
func (s *State) beginLinkOperation(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.link.Pending {
		return false
	}
	s.link.Pending = true
	s.link.Connecting = !s.link.Bound
	s.link.Started = now
	s.link.Error = ""
	if s.link.Connecting {
		s.link.HadReply = false
	}
	return true
}
func (s *State) endLinkOperation(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.link.Pending = false
	if err != nil {
		s.link.Error = err.Error()
	}
}

// At most one Home command is queued/running. A long driver syscall never holds
// a UI lock and is NOT declared cancelled merely because the animation ended.
func (u *UI) queueMonitorToggle(fn func() error) {
	if !u.state.beginLinkOperation(time.Now()) {
		return
	}
	if u.ops == nil {
		u.state.endLinkOperation(fmt.Errorf("control worker unavailable"))
		return
	}
	select {
	case u.ops <- func() error {
		err := fn()
		u.state.endLinkOperation(err)
		return err
	}:
	default:
		u.state.endLinkOperation(fmt.Errorf("control operation already pending"))
	}
}
func (u *UI) toggleMonitor() {
	// Normal boot is already attached. Repeated Home must not drop every USB
	// function while the user expects a driver/readiness check.
	if u.usbTrialFPS == 0 && u.transport != nil && u.transport.Bound() {
		return
	}
	u.queueMonitorToggle(func() error {
		if u.transport == nil {
			return fmt.Errorf("USB unavailable: inspect diagnostics")
		}
		if !u.transport.Bound() {
			if _, e := Thermal(); e != nil {
				return e
			}
		}
		return u.transport.Toggle()
	})
}

func (u *UI) monitorLinkSummary(now time.Time) (summary, detail string) {
	v := u.state.MonitorLink(now)
	return v.Title, v.Detail
}
func (u *UI) monitorLinkRows() []string {
	v := u.state.MonitorLink(time.Now())
	video := "Waiting / paused"
	if v.VideoRecent {
		video = "Recent decoded frame"
	}
	rows := []string{"Monitor / Home link", "STATUS: " + v.Title, v.Detail,
		fmt.Sprintf("USB BOUND: %t / ENUMERATED: %t", v.Bound, v.Enumerated),
		fmt.Sprintf("MONITOR DRIVER REPLIED: %t", v.DriverReplied),
		"VIDEO: " + video,
		"Home connects / disconnects. No companion app.",
		"Required Windows drivers must already be installed.",
		"This checks the monitor protocol, not every PC driver."}
	if v.Error != "" {
		rows = append(rows, "ERROR: "+v.Error)
	}
	return append(rows, "BACK: DEVICE")
}

// Link changes are shown in the menu header. Polling must not repaint a video
// frame, so UI input and monitor presentation do not compete for scanout.
func (u *UI) refreshMonitorLink(now time.Time) {
	v := u.state.MonitorLink(now)
	if u.linkSeen && u.linkPhase != v.Phase {
		switch v.Phase {
		case "ready":
			u.notify("USB CONNECTED")
		case "disconnected", "waiting_usb":
			u.notify("USB DISCONNECTED")
		case "lost":
			u.notify("USB LINK LOST")
		}
	}
	u.linkSeen = true
	u.linkPhase = v.Phase
}

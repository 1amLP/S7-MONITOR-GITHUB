//go:build linux && (amd64 || arm64)

package appliance

import (
	"fmt"
	"perimode/native/internal/fb"
	"strconv"
	"strings"
	"time"
)

func activity(enabled, active bool) fb.Activity {
	if !enabled {
		return fb.Off
	}
	if active {
		return fb.Active
	}
	return fb.Idle
}
func fresh(now, last time.Time) bool {
	return !last.IsZero() && !now.Before(last) && now.Sub(last) < 2*time.Second
}
func (s *State) IndicatorValues(now time.Time, bound, touchActive bool) []fb.Indicator {
	s.mu.Lock()
	defer s.mu.Unlock()
	running := bound && !s.ThermalPaused
	battery, thermal, cpu := "?", "?", "?"
	if s.BatteryPercent >= 0 {
		battery = fmt.Sprintf("B%d%%", s.BatteryPercent)
		if s.BatteryMilliC >= 0 {
			battery += fmt.Sprintf(" %dC", s.BatteryMilliC/1000)
		}
	}
	if s.CPUMilliC >= 0 {
		thermal = fmt.Sprintf("CPU%dC", s.CPUMilliC/1000)
	}
	if s.CPULoad >= 0 {
		cpu = fmt.Sprintf("L%d%%", s.CPULoad)
	}
	meter := func(valid bool) fb.Activity {
		if valid {
			return fb.Idle
		}
		return fb.Unknown
	}
	torchActivity := activity(s.Torch.Wanted || s.Torch.Accepted, s.Torch.Accepted)
	if s.Torch.Unknown {
		torchActivity = fb.Unknown
	}
	return []fb.Indicator{
		{Kind: "monitor", State: activity(s.Settings.Enabled, running && s.Consumer && fresh(now, s.LastBlit))},
		{Kind: "camera", State: activity(s.Camera.Enabled || s.Preview.Enabled, !s.ThermalPaused && ((running && s.Camera.LastError == "" && fresh(now, s.Camera.LastActivity)) || (s.Preview.Enabled && s.Preview.LastError == "" && fresh(now, s.Preview.LastFrame))))},
		{Kind: "microphone", State: activity(s.MicrophoneEnabled, running && s.AudioError == "" && fresh(now, s.AudioMicrophoneLast))},
		{Kind: "speaker", State: activity(s.SpeakerEnabled, running && s.AudioError == "" && fresh(now, s.AudioSpeakerLast))},
		{Kind: "touch", State: activity(s.TouchKind != 0, bound && touchActive)},
		{Kind: "torch", State: torchActivity},
		{Kind: "battery", State: meter(s.BatteryPercent >= 0), Text: battery},
		{Kind: "temperature", State: meter(s.CPUMilliC >= 0), Text: thermal},
		{Kind: "cpu", State: meter(s.CPULoad >= 0), Text: cpu},
	}
}
func (u *UI) drawIndicators() error {
	// Status is painted exclusively by the menu header. A closed menu must not
	// touch scanout, which also keeps physical input free of overlay redraws.
	return nil
}
func parseBattery(capacity, status string) (int, string, error) {
	n, e := strconv.Atoi(strings.TrimSpace(capacity))
	if e != nil || n < 0 || n > 100 {
		return -1, "UNKNOWN", fmt.Errorf("invalid battery capacity")
	}
	s := strings.ToUpper(strings.TrimSpace(status))
	switch s {
	case "CHARGING", "DISCHARGING", "FULL", "NOT CHARGING", "UNKNOWN":
	default:
		return -1, "UNKNOWN", fmt.Errorf("invalid battery status")
	}
	return n, s, nil
}
func readBattery() (int, string, error) {
	c, e := read("/sys/class/power_supply/battery/capacity")
	if e != nil {
		return -1, "UNKNOWN", e
	}
	st, e := read("/sys/class/power_supply/battery/status")
	if e != nil {
		return -1, "UNKNOWN", e
	}
	return parseBattery(c, st)
}

func telemetryLabel(load int, cpu, battery int64) string {
	percent := "?"
	if load >= 0 {
		percent = fmt.Sprintf("%d", load)
	}
	temp := func(v int64) string {
		if v < 0 {
			return "?"
		}
		return fmt.Sprintf("%d", v/1000)
	}
	return "CPU: " + percent + " PCT / " + temp(cpu) + " C   BATTERY: " + temp(battery) + " C"
}

func (u *UI) indicatorLines() []string {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	return []string{"Device / Indicators", fmt.Sprintf("SHOW INDICATORS: %t", u.state.Indicators), fmt.Sprintf("INDICATOR SIZE: %d PCT", u.state.IndicatorScale), fmt.Sprintf("POSITION: %s", []string{"Top left", "Top right", "Bottom left", "Bottom right"}[u.state.Corner&3]), "RESET SIZE: 100 PCT", "OFF FUNCTIONS ARE HIDDEN", "BACK: DEVICE"}
}
func (u *UI) selectIndicators(row int) {
	u.state.mu.Lock()
	switch row {
	case 1:
		u.state.Indicators = !u.state.Indicators
	case 3:
		u.state.Corner = (u.state.Corner + 1) % 4
	case 4:
		u.state.IndicatorScale = 100
	}
	u.state.mu.Unlock()
	u.draw()
}

//go:build linux

package appliance

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const cpuLimitNode = "/sys/power/cpuhotplug/max_online_cpu"
const chargeMaxNode = "/sys/module/sec_battery/parameters/store_mode_max"
const chargeMinNode = "/sys/module/sec_battery/parameters/store_mode_min"
const chargeModeNode = "/sys/class/power_supply/battery/store_mode"

type PowerRuntime struct {
	Eco            bool   `json:"eco_requested"`
	CPULimit       int    `json:"cpu_online_limit"`
	Online         string `json:"online_cpus"`
	LittleKHz      int    `json:"little_khz"`
	BigKHz         int    `json:"big_khz"`
	BatteryHealth  string `json:"battery_health"`
	BatteryUV      int    `json:"battery_uv"`
	BatteryCurrent int    `json:"battery_current_raw"`
	ChargeLimit    bool   `json:"charge_80_50_configured"`
	CPUError       string `json:"cpu_error,omitempty"`
	ChargeError    string `json:"charge_error,omitempty"`
}

func (s *State) PowerSnapshot() PowerRuntime { s.mu.Lock(); defer s.mu.Unlock(); return s.Power }

type powerIO struct {
	read  func(string) (string, error)
	write func(string, string) error
}

// Roll back every attempted write, including a write with uncertain progress.
func (p powerIO) transaction(values [][2]string) error {
	before := make([]string, len(values))
	for i, v := range values {
		s, err := p.read(v[0])
		if err != nil {
			return err
		}
		before[i] = s
	}
	for i, v := range values {
		err := p.write(v[0], v[1])
		if err == nil {
			var got string
			got, err = p.read(v[0])
			if err == nil && got != v[1] {
				err = fmt.Errorf("power attribute readback differs: %s", v[0])
			}
		}
		if err != nil {
			for j := i; j >= 0; j-- {
				err = errors.Join(err, p.write(values[j][0], before[j]))
			}
			return err
		}
	}
	return nil
}

func (p powerIO) charge80() error {
	maxText, err := p.read(chargeMaxNode)
	if err != nil {
		return err
	}
	minText, err := p.read(chargeMinNode)
	if err != nil {
		return err
	}
	mode, err := p.read(chargeModeNode)
	if err != nil {
		return err
	}
	upper, e1 := strconv.Atoi(maxText)
	lower, e2 := strconv.Atoi(minText)
	if e1 != nil || e2 != nil || lower < 0 || upper > 100 || lower >= upper || (mode != "0" && mode != "1") {
		return fmt.Errorf("unsupported Samsung charge-control values")
	}
	// Store mode stops battery charging, not USB data or the input supply.
	// No direct voltage/current programming or changes to thermal protection.
	return p.transaction([][2]string{{chargeMaxNode, "80"}, {chargeMinNode, "50"}, {chargeModeNode, "1"}})
}

func cpuUserLimit(text string) (int, error) {
	var current int
	if n, err := fmt.Sscanf(text, "max online cpu : %d", &current); n != 1 || err != nil || current < 0 || current > 8 {
		return 0, fmt.Errorf("invalid CPU user limit: %q", text)
	}
	// e418 leaves user_max zero until the first sysfs write, while its
	// PM_QOS_CPU_ONLINE_MAX request starts at NR_CPUS (8).
	if current == 0 {
		current = 8
	}
	return current, nil
}

func (p powerIO) cpuLimit(eco bool, baseline *int) (int, error) {
	if *baseline == 0 {
		for path, want := range map[string]string{
			"/sys/devices/system/cpu/cpu0/cpufreq/related_cpus": "0 1 2 3",
			"/sys/devices/system/cpu/cpu4/cpufreq/related_cpus": "4 5 6 7",
			"/sys/power/cpuhotplug/enabled":                     "1",
		} {
			v, err := p.read(path)
			if err != nil {
				return 0, err
			}
			if strings.Join(strings.Fields(v), " ") != want {
				return 0, fmt.Errorf("CPU topology/hotplug not confirmed: %s", path)
			}
		}
	}
	// The big policy's sysfs directory may disappear when its last CPU goes
	// offline. Restoring the already validated baseline must not depend on it.
	text, err := p.read(cpuLimitNode)
	if err != nil {
		return 0, err
	}
	current, err := cpuUserLimit(text)
	if err != nil {
		return 0, err
	}
	if *baseline == 0 {
		*baseline = current
	}
	want := *baseline
	if eco && want > 4 {
		want = 4
	}
	if current != want {
		if err = p.write(cpuLimitNode, strconv.Itoa(want)); err != nil {
			return current, errors.Join(err, p.write(cpuLimitNode, strconv.Itoa(current)))
		}
		text, err = p.read(cpuLimitNode)
		if err != nil {
			return current, errors.Join(err, p.write(cpuLimitNode, strconv.Itoa(current)))
		}
		if got, e := cpuUserLimit(text); e != nil || got != want {
			return current, errors.Join(fmt.Errorf("CPU limit was not accepted"), p.write(cpuLimitNode, strconv.Itoa(current)))
		}
	}
	return want, nil
}

func powerNumber(p powerIO, path string) int {
	v, e := p.read(path)
	if e != nil {
		return -1
	}
	n, e := strconv.Atoi(v)
	if e != nil {
		return -1
	}
	return n
}

func (u *UI) powerPolicyWorker(ctx context.Context) {
	p := powerIO{read: read, write: writeAttr}
	var baseline int
	var previous bool
	var attempted bool
	chargeErr := p.charge80()
	var cpuErr error
	limit := 0
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		u.state.mu.Lock()
		eco := u.state.CPUEco
		u.state.mu.Unlock()
		if !attempted || eco != previous {
			limit, cpuErr = p.cpuLimit(eco, &baseline)
			previous = eco
			attempted = true
		}
		v := PowerRuntime{Eco: eco, CPULimit: limit, ChargeLimit: chargeErr == nil}
		v.Online, _ = p.read("/sys/devices/system/cpu/online")
		v.LittleKHz = powerNumber(p, "/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq")
		v.BigKHz = powerNumber(p, "/sys/devices/system/cpu/cpu4/cpufreq/scaling_cur_freq")
		v.BatteryHealth, _ = p.read("/sys/class/power_supply/battery/health")
		v.BatteryUV = powerNumber(p, "/sys/class/power_supply/battery/voltage_now")
		v.BatteryCurrent = powerNumber(p, "/sys/class/power_supply/battery/current_now")
		if chargeErr != nil {
			v.ChargeError = chargeErr.Error()
		}
		if cpuErr != nil {
			v.CPUError = cpuErr.Error()
		}
		u.state.mu.Lock()
		u.state.Power = v
		u.state.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

package appliance

import (
	"fmt"
	"strconv"
	"strings"
)

type CPUTimes struct{ Total, Idle uint64 }

// /proc/stat user/nice/system/idle/iowait/irq/softirq/steal only.
// guest and guest_nice are already counted in user/nice.
func ParseCPU(text string) (CPUTimes, error) {
	f := strings.Fields(strings.SplitN(text, "\n", 2)[0])
	if len(f) < 5 || f[0] != "cpu" {
		return CPUTimes{}, fmt.Errorf("aggregate CPU counters missing")
	}
	var t CPUTimes
	for i := 1; i < len(f) && i <= 8; i++ {
		n, e := strconv.ParseUint(f[i], 10, 64)
		if e != nil {
			return t, e
		}
		if t.Total+n < t.Total {
			return t, fmt.Errorf("CPU counter overflow")
		}
		t.Total += n
		if i == 4 || i == 5 {
			t.Idle += n
		}
	}
	return t, nil
}
func CPUPercent(a, b CPUTimes) (int, bool) {
	if b.Total <= a.Total || b.Idle < a.Idle || b.Idle-a.Idle > b.Total-a.Total {
		return 0, false
	}
	total, idle := b.Total-a.Total, b.Idle-a.Idle
	// Avoid integer overflow even for deliberately malformed counter deltas.
	return int(float64(total-idle) * 100 / float64(total)), true
}

//go:build linux && (amd64 || arm64)

package appliance

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Diagnostic-only. The pinned DECON registers TE and frame-done interrupts
// under its platform name; keep the controller/IRQ identity distinct.
func panelInterruptCounts(data string) (map[string]uint64, error) {
	if len(data) > 128*1024 {
		return nil, fmt.Errorf("interrupt table too large")
	}
	lines := strings.Split(data, "\n")
	cpus := strings.Fields(lines[0])
	if len(cpus) == 0 || len(cpus) > 64 {
		return nil, fmt.Errorf("invalid interrupt CPU header")
	}
	previousCPU := -1
	for _, cpu := range cpus {
		n, err := strconv.Atoi(strings.TrimPrefix(cpu, "CPU"))
		if err != nil || !strings.HasPrefix(cpu, "CPU") || n <= previousCPU || n >= 64 {
			return nil, fmt.Errorf("unexpected interrupt CPU header")
		}
		previousCPU = n
	}
	counts := make(map[string]uint64)
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasSuffix(f[len(f)-1], ".decon_f") {
			continue
		}
		if len(f) < len(cpus)+3 || len(counts) >= 8 {
			return nil, fmt.Errorf("invalid DECON interrupt row")
		}
		var total uint64
		for _, value := range f[1 : 1+len(cpus)] {
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil || n > ^uint64(0)-total {
				return nil, fmt.Errorf("invalid DECON interrupt count")
			}
			total += n
		}
		counts[f[0]+" "+strings.Join(f[1+len(cpus):], " ")] = total
	}
	if len(counts) == 0 {
		return nil, fmt.Errorf("DECON interrupts unavailable")
	}
	return counts, nil
}

func panelInterruptSnapshot() map[string]any {
	data, err := os.ReadFile("/proc/interrupts")
	v := map[string]any{"at": time.Now()}
	if err == nil {
		var counts map[string]uint64
		counts, err = panelInterruptCounts(string(data))
		v["counts"] = counts
	}
	if err != nil {
		v["error"] = err.Error()
	}
	return v
}

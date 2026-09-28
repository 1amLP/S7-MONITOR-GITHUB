//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"perimode/native/internal/childproc"
	"perimode/native/internal/safety"
)

type processMemory struct {
	PID     int               `json:"pid"`
	Name    string            `json:"name"`
	KiB     map[string]uint64 `json:"kib"`
	Threads uint64            `json:"threads"`
	FDs     int               `json:"fds"`
	Error   string            `json:"error,omitempty"`
}

type memorySample struct {
	At        time.Time              `json:"at"`
	MemKiB    map[string]uint64      `json:"meminfo_kib"`
	VM        map[string]uint64      `json:"vm_counters"`
	Go        map[string]uint64      `json:"go_metrics"`
	Processes []processMemory        `json:"processes"`
	Heartbeat safety.HeartbeatStatus `json:"heartbeat"`
	Error     string                 `json:"error,omitempty"`
}

type memoryHistory struct {
	Samples   []memorySample `json:"samples"`
	ThreadCPU []threadCPU    `json:"thread_cpu"`
	ClockHz   uint64         `json:"clock_hz"`
}

type memoryTrace struct{ latest atomic.Pointer[memoryHistory] }

func readMemoryFields(path string, keys ...string) (map[string]uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := make(map[string]uint64, len(keys))
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ":")
		for _, key := range keys {
			if key != name {
				continue
			}
			if value, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				values[key] = value
			}
			break
		}
	}
	return values, nil
}

func readProcessMemory(pid int) processMemory {
	p := processMemory{PID: pid, FDs: -1}
	root := "/proc/" + strconv.Itoa(pid)
	fields, err := readMemoryFields(root+"/status", "VmRSS", "VmHWM", "VmSize", "VmData", "VmSwap", "Threads")
	if err != nil {
		p.Error = err.Error()
		return p
	}
	p.Threads = fields["Threads"]
	delete(fields, "Threads")
	p.KiB = fields
	if name, err := os.ReadFile(root + "/comm"); err == nil {
		p.Name = strings.TrimSpace(string(name))
	}
	if fds, err := os.ReadDir(root + "/fd"); err == nil {
		p.FDs = len(fds)
	} else {
		p.Error = err.Error()
	}
	return p
}

// Independent of framebuffer, codec and UI locks. A renderer stall must not
// hide RAM pressure or stop the small pmsg breadcrumbs before a reset.
func (m *memoryTrace) run(ctx context.Context, health *safety.Liveness) error {
	hz := threadClockHz()
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/heap/free:bytes"},
		{Name: "/memory/classes/heap/released:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/sched/goroutines:goroutines"},
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var previous memorySample
	var logged time.Time
	for {
		s := memorySample{At: time.Now(), Heartbeat: health.Check(), Go: make(map[string]uint64, len(samples))}
		var err error
		s.MemKiB, err = readMemoryFields("/proc/meminfo", "MemTotal", "MemAvailable", "MemFree", "AnonPages", "Cached", "Shmem", "Slab", "SUnreclaim", "PageTables", "KernelStack", "CmaTotal", "CmaFree", "SwapFree")
		if err != nil {
			s.Error = err.Error()
		}
		s.VM, err = readMemoryFields("/proc/vmstat", "oom_kill", "allocstall", "pgmajfault", "pgscan_direct_normal", "pgscan_direct_dma", "pgscan_direct_dma32", "compact_fail")
		if err != nil {
			s.Error = err.Error()
		}
		metrics.Read(samples)
		for _, metric := range samples {
			if metric.Value.Kind() == metrics.KindUint64 {
				s.Go[metric.Name] = metric.Value.Uint64()
			}
		}
		s.Processes = append(s.Processes, readProcessMemory(os.Getpid()))
		if pids, ok := childproc.OwnedPIDs(); ok {
			for _, pid := range pids[:min(len(pids), 16)] {
				s.Processes = append(s.Processes, readProcessMemory(pid))
			}
			if len(pids) > 16 {
				s.Error = "more than 16 owned children; process detail capped"
			}
		} else {
			s.Error = "child registry busy"
		}
		h := &memoryHistory{Samples: make([]memorySample, 0, 24), ClockHz: hz}
		for _, p := range s.Processes {
			h.ThreadCPU = append(h.ThreadCPU, readThreadCPU(p.PID, hz)...)
		}
		if old := m.latest.Load(); old != nil {
			start := max(0, len(old.Samples)-23)
			h.Samples = append(h.Samples, old.Samples[start:]...)
		}
		h.Samples = append(h.Samples, s)
		m.latest.Store(h)
		available, known := s.MemKiB["MemAvailable"]
		pressure := known && (available < 128*1024 || previous.MemKiB["MemAvailable"] > available+64*1024)
		if logged.IsZero() || s.At.Sub(logged) >= 30*time.Second || pressure || s.VM["oom_kill"] > previous.VM["oom_kill"] {
			// log already writes to the bounded persistent RAM ring, never CACHE.
			line := fmt.Sprintf("S7 memory avail_kib=%d anon_kib=%d slab_kib=%d cma_kib=%d heap_bytes=%d goroutines=%d oom=%d ui_age_ms=%d", available, s.MemKiB["AnonPages"], s.MemKiB["Slab"], s.MemKiB["CmaFree"], s.Go[samples[0].Name], s.Go[samples[5].Name], s.VM["oom_kill"], s.Heartbeat.UIAgeMS)
			for _, p := range s.Processes {
				line += fmt.Sprintf(" pid=%d:%s rss_kib=%d fds=%d threads=%d", p.PID, p.Name, p.KiB["VmRSS"], p.FDs, p.Threads)
			}
			log.Print(line)
			logged = s.At
		}
		previous = s
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

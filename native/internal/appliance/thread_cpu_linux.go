//go:build linux && (amd64 || arm64)

package appliance

import (
	"encoding/binary"
	"os"
	"strconv"
	"strings"
)

type threadCPU struct {
	PID, TID                                 int
	Name                                     string
	StartTicks, UserUS, KernelUS, RunqueueNS uint64
}

func threadClockHz() uint64 {
	b, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		return 0
	}
	for len(b) >= 16 {
		if binary.LittleEndian.Uint64(b) == 17 {
			hz := binary.LittleEndian.Uint64(b[8:])
			if hz > 0 && hz <= 10000 {
				return hz
			}
			return 0
		}
		b = b[16:]
	}
	return 0
}
func readThreadCPU(pid int, hz uint64) []threadCPU {
	if hz == 0 {
		return nil
	}
	root := "/proc/" + strconv.Itoa(pid) + "/task/"
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []threadCPU
	for _, entry := range entries[:min(64, len(entries))] {
		tid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(root + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		a, z := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if a < 0 || z <= a {
			continue
		}
		f := strings.Fields(s[z+1:])
		if len(f) <= 19 {
			continue
		}
		u, e1 := strconv.ParseUint(f[11], 10, 64)
		k, e2 := strconv.ParseUint(f[12], 10, 64)
		start, e3 := strconv.ParseUint(f[19], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		v := threadCPU{PID: pid, TID: tid, Name: s[a+1 : z], StartTicks: start, UserUS: u * 1000000 / hz, KernelUS: k * 1000000 / hz}
		if b, err = os.ReadFile(root + entry.Name() + "/schedstat"); err == nil {
			if f = strings.Fields(string(b)); len(f) >= 2 {
				v.RunqueueNS, _ = strconv.ParseUint(f[1], 10, 64)
			}
		}
		out = append(out, v)
	}
	return out
}

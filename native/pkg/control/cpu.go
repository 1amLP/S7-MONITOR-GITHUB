package control

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"
)

type CPUReading struct {
	Percent int `json:"percent"`
	Online  int `json:"online"`
}

type cpuTimes struct{ busy, total uint64 }
type CPUSampler struct{ previous map[string]cpuTimes }

// Per-core deltas avoid treating a returning offline core's old time as load.
func (s *CPUSampler) Sample(input io.Reader) (CPUReading, error) {
	next := make(map[string]cpuTimes)
	scan := bufio.NewScanner(input)
	for scan.Scan() {
		f := strings.Fields(scan.Text())
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") || f[0] == "cpu" {
			continue
		}
		if _, err := strconv.ParseUint(strings.TrimPrefix(f[0], "cpu"), 10, 16); err != nil || len(f) < 5 {
			return CPUReading{}, errors.New("invalid CPU row")
		}
		var times cpuTimes
		for i := 1; i < len(f) && i <= 8; i++ {
			v, err := strconv.ParseUint(f[i], 10, 64)
			if err != nil || ^uint64(0)-times.total < v {
				return CPUReading{}, errors.New("invalid CPU counter")
			}
			times.total += v
			if i != 4 && i != 5 {
				times.busy += v
			}
		}
		next[f[0]] = times
	}
	if err := scan.Err(); err != nil {
		return CPUReading{}, err
	}
	if len(next) == 0 {
		return CPUReading{}, errors.New("CPU counters unavailable")
	}
	var busy, total uint64
	for name, now := range next {
		before, ok := s.previous[name]
		if ok && now.total >= before.total && now.busy >= before.busy {
			dt, db := now.total-before.total, now.busy-before.busy
			if db <= dt {
				total += dt
				busy += db
			}
		}
	}
	s.previous = next
	result := CPUReading{Percent: -1, Online: len(next)}
	if total != 0 {
		result.Percent = int(100*float64(busy)/float64(total) + 0.5)
	}
	return result, nil
}

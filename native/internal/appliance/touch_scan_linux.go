//go:build linux && (amd64 || arm64)

package appliance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"perimode/native/internal/linuxio"
)

var errTouchSensorRateUnsupported = errors.New("touch controller uses its native scan rate")

// FTS7 maps report_rate,0 to FAST_SCAN (90 Hz). Its factory command reports
// OK even if the underlying bus write fails, so the result is only a request.
func requestTouchSensor90At(dir string) error {
	commands, err := os.ReadFile(filepath.Join(dir, "cmd_list"))
	if errors.Is(err, os.ErrNotExist) {
		return errTouchSensorRateUnsupported
	}
	if err != nil {
		return fmt.Errorf("touch sensor command list: %w", err)
	}
	supported := false
	for _, command := range strings.Fields(string(commands)) {
		supported = supported || command == "report_rate"
	}
	if !supported {
		return errTouchSensorRateUnsupported
	}
	if err := linuxio.WriteAttr(filepath.Join(dir, "cmd"), "report_rate,0\n"); err != nil {
		return fmt.Errorf("touch sensor 90 Hz command: %w", err)
	}
	result, err := os.ReadFile(filepath.Join(dir, "cmd_result"))
	if err != nil {
		return fmt.Errorf("touch sensor 90 Hz result: %w", err)
	}
	if strings.TrimSpace(string(result)) != "report_rate,0:OK" {
		if strings.TrimSpace(string(result)) == "report_rate,0:NA" {
			return errTouchSensorRateUnsupported
		}
		return fmt.Errorf("touch sensor 90 Hz request rejected: %q", strings.TrimSpace(string(result)))
	}
	return nil
}

func requestTouchSensor90() error { return requestTouchSensor90At("/sys/class/sec/tsp") }

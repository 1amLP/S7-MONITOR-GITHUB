//go:build linux && (amd64 || arm64)

package appliance

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"perimode/native/internal/linuxio"
)

// FTS7 maps report_rate,0 to FAST_SCAN (90 Hz). Its factory command reports
// OK even if the underlying bus write fails, so the result is only a request.
func requestTouchSensor90At(dir string) error {
	if err := linuxio.WriteAttr(filepath.Join(dir, "cmd"), "report_rate,0\n"); err != nil {
		return fmt.Errorf("touch sensor 90 Hz command: %w", err)
	}
	result, err := os.ReadFile(filepath.Join(dir, "cmd_result"))
	if err != nil {
		return fmt.Errorf("touch sensor 90 Hz result: %w", err)
	}
	if strings.TrimSpace(string(result)) != "report_rate,0:OK" {
		return fmt.Errorf("touch sensor 90 Hz request rejected: %q", strings.TrimSpace(string(result)))
	}
	return nil
}

func requestTouchSensor90() error { return requestTouchSensor90At("/sys/class/sec/tsp") }

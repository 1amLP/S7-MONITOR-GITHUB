//go:build linux && (amd64 || arm64)

package camera

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// InventoryReport never opens /dev/video* or sends ioctls. However, reading the
// pinned rear_sensorid sysfs attribute can power calibration hardware and run
// firmware selection. This is NOT a passive hardware probe or capture evidence.
type InventoryReport struct {
	Schema                      string              `json:"schema"`
	OpenedVideoDevices          bool                `json:"opened_video_devices"`
	HardwareAcceptance          bool                `json:"hardware_acceptance"`
	SensorIdentityMayInitialize bool                `json:"sensor_identity_may_initialize"`
	Nodes                       []InventoryNode     `json:"nodes"`
	SensorLabels                map[string]string   `json:"sensor_labels"`
	SensorModules               map[string]ModuleID `json:"sensor_modules"`
	Errors                      []string            `json:"errors,omitempty"`
}

// ModuleID is the numeric firmware-module selector, NOT a serial number. Its
// presence does not validate calibration, routing or one of the webcam modes.
type ModuleID struct {
	Value uint32 `json:"value"`
	Valid bool   `json:"valid"`
	Error string `json:"error,omitempty"`
}

func parseModuleID(text string) (uint32, error) {
	if len(text) == 0 || len(text) > 3 {
		return 0, fmt.Errorf("sensor module ID is not 1..255 decimal")
	}
	for _, c := range text {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("nondecimal sensor module ID")
		}
	}
	v, e := strconv.ParseUint(text, 10, 8)
	if e != nil || v == 0 {
		return 0, fmt.Errorf("sensor module ID outside packed selector range")
	}
	return uint32(v), nil
}

type InventoryNode struct {
	Class      string `json:"class"`
	Node       string `json:"node"`
	Name       string `json:"name"`
	MajorMinor string `json:"major_minor,omitempty"`
	SystemPath string `json:"system_path,omitempty"`
	RoleHint   string `json:"role_hint"`
}

func boundedSysfs(root, path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return "", fmt.Errorf("sysfs symlink escapes supplied root")
	}
	f, err := os.Open(real)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	if len(data) > 4096 || strings.ContainsRune(string(data), '\x00') {
		return "", fmt.Errorf("oversized/binary sysfs text")
	}
	return strings.TrimSpace(string(data)), nil
}
func roleHint(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "mfc"):
		return "codec; not a sensor"
	case strings.Contains(n, "fimc") || strings.Contains(n, "3aa") || strings.Contains(n, "mcsc") || strings.Contains(n, "isp"):
		return "vendor camera node; topology not inferred"
	case strings.Contains(n, "sensor") || strings.Contains(n, "s5k") || strings.Contains(n, "imx"):
		return "possible sensor; not confirmed"
	case strings.Contains(n, "gadget") || strings.Contains(n, "uvc"):
		return "USB video; not a sensor claim"
	default:
		return "unclassified"
	}
}
func InspectSysfs(sysroot string) InventoryReport {
	r := InventoryReport{Schema: "S7-CAMERA-SYSFS-2", Nodes: []InventoryNode{}, SensorLabels: map[string]string{}, SensorModules: map[string]ModuleID{}}
	root, err := filepath.EvalSymlinks(sysroot)
	if err != nil || !filepath.IsAbs(root) {
		r.Errors = append(r.Errors, "sysfs root unavailable")
		return r
	}
	for _, class := range []string{"video4linux", "media"} {
		paths, err := filepath.Glob(filepath.Join(root, "class", class, "*"))
		if err != nil {
			r.Errors = append(r.Errors, err.Error())
			continue
		}
		for _, p := range paths {
			if len(r.Nodes) >= 128 {
				r.Errors = append(r.Errors, "sysfs inventory capped at 128 nodes")
				break
			}
			name, err := boundedSysfs(root, filepath.Join(p, "name"))
			if err != nil {
				name = ""
			}
			dev, _ := boundedSysfs(root, filepath.Join(p, "dev"))
			real, _ := filepath.EvalSymlinks(p)
			if !strings.HasPrefix(real, root+string(filepath.Separator)) {
				r.Errors = append(r.Errors, "node outside sysfs: "+filepath.Base(p))
				continue
			}
			relative, _ := filepath.Rel(root, real)
			r.Nodes = append(r.Nodes, InventoryNode{Class: class, Node: filepath.Base(p), Name: name, MajorMinor: dev, SystemPath: relative, RoleHint: roleHint(name)})
		}
	}
	// Optional descriptive labels, NOT serial/calibration/EEPROM data. Existence
	// and spelling vary by kernel; a missing label is not proof of missing hardware.
	for _, pair := range [][2]string{{"rear", "class/camera/rear/rear_camtype"}, {"front", "class/camera/front/front_camtype"}} {
		if v, e := boundedSysfs(root, filepath.Join(root, pair[1])); e == nil {
			r.SensorLabels[pair[0]] = v
		}
	}

	// Paths are observed in getSensorIdFromFile from the supplied library. No
	// fallback from "rear/front" to guessed 3AA nodes or default module numbers.
	// Samsung's rear_sensorid handler calls fimc_is_sec_run_fw_sel on read.
	r.SensorIdentityMayInitialize = true
	for _, side := range []string{"rear", "front"} {
		text, e := boundedSysfs(root, filepath.Join(root, "class/camera", side, side+"_sensorid"))
		var value uint32
		if e == nil {
			value, e = parseModuleID(text)
		}
		item := ModuleID{Value: value, Valid: e == nil}
		if e != nil {
			item.Error = e.Error()
		}
		r.SensorModules[side] = item
	}
	return r
}

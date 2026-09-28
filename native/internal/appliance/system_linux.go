//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"perimode/native/internal/camera/fimcgraph"
	"perimode/native/internal/camera/fimcshot"
	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
	"perimode/native/internal/safety"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const PinnedKernel = "3.18.140-ge41817ea9198"
// Set at build time with -ldflags -X; an unset identity never binds USB.
var PinnedSerial string

var previousBootMessages string

func SetupRoot() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("native init must be PID 1")
	}
	// No SYSTEM/USERDATA/EFS mounts, no fsck, no partition writes.
	for _, p := range []string{"/proc", "/sys", "/dev", "/run", "/tmp", "/config"} {
		if e := os.MkdirAll(p, 0755); e != nil {
			return e
		}
	}
	for _, m := range []struct {
		src, target, fs string
		flags           uintptr
		data            string
	}{{"proc", "/proc", "proc", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""}, {"sysfs", "/sys", "sysfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""}, {"devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, "mode=0755"}, {"tmpfs", "/run", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, "mode=0755,size=16m"}, {"tmpfs", "/tmp", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, "mode=1777,size=8m"}, {"configfs", "/config", "configfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""}} {
		if e := syscall.Mount(m.src, m.target, m.fs, m.flags, m.data); e != nil && e != syscall.EBUSY {
			return fmt.Errorf("mount %s: %w", m.target, e)
		}
	}
	if err := os.MkdirAll("/sys/fs/pstore", 0755); err == nil {
		if err = syscall.Mount("pstore", "/sys/fs/pstore", "pstore", syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil && err != syscall.EBUSY {
			log.Printf("previous crash log mount: %v", err)
		}
	}
	return nil
}

// Cache before USB workers start. Pstore is a reserved RAM ring, not USERDATA.
func PreviousBootMessages() string { return previousBootMessages }

func loadPreviousBootMessages() {
	previousBootMessages = ""
	// Samsung extra_info is valid only on the first boot after a panic. Cache
	// it before a later controlled Recovery reboot replaces the reset reason.
	paths := []string{"/proc/reset_reason",
		"/sys/class/sec/sec_hw_param/extra_info", "/sys/class/sec/sec_hw_param/extrb_info",
		"/sys/class/sec/sec_hw_param/extrc_info", "/sys/class/sec/sec_hw_param/extrm_info", "/proc/last_kmsg",
		"/sys/fs/pstore/console-ramoops-0", "/sys/fs/pstore/pmsg-ramoops-0"}
	dumps, _ := filepath.Glob("/sys/fs/pstore/dmesg-ramoops-*")
	paths = append(paths, dumps[:min(4, len(dumps))]...)
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		if strings.HasSuffix(path, ".enc.z") {
			// pstore's suffix means compressed, not text. Preserve exact bytes;
			// UTF-8 conversion would destroy the only surviving panic record.
			data, err := io.ReadAll(io.LimitReader(f, 256<<10))
			_ = f.Close()
			if err == nil {
				previousBootMessages += path + " BASE64:\n" + base64.StdEncoding.EncodeToString(data) + "\n"
			}
			continue
		}
		data, err := previousLogTail(f)
		_ = f.Close()
		if len(data) != 0 {
			previousBootMessages += path + ":\n" + string(data) + "\n"
		}
		if err != nil {
			previousBootMessages += path + " read: " + err.Error() + "\n"
		}
	}
}

func Identity() (string, error) {
	k, e := read("/proc/sys/kernel/osrelease")
	if e != nil || k != PinnedKernel {
		return "", fmt.Errorf("kernel identity mismatch %q: %v", k, e)
	}
	return physicalSerial()
}

func physicalSerial() (string, error) {
	cmd, e := read("/proc/cmdline")
	if e != nil {
		return "", e
	}
	serial := ""
	for _, v := range strings.Fields(cmd) {
		if strings.HasPrefix(v, "androidboot.serialno=") {
			serial = strings.TrimPrefix(v, "androidboot.serialno=")
		}
	}
	if serial == "" {
		serial, _ = read("/sys/devices/soc0/serial_number")
	}
	if PinnedSerial == "" || serial != PinnedSerial {
		return serial, fmt.Errorf("physical serial not confirmed; USB remains unbound")
	}
	return serial, nil
}

type ThermalReading struct {
	Name, Path string
	MilliC     int64
	Role       string
}

func (r ThermalReading) IsSoC() bool { return r.Role == "CPU" || r.Role == "GPU" || r.Role == "ISP" }

func Thermal() ([]ThermalReading, error) {
	return thermalAt("/sys")
}

func thermalAt(root string) ([]ThermalReading, error) {
	out := []ThermalReading{}
	haveCPU := false
	enabledCPUZones := 0
	expected := map[string]bool{"therm_zone0": false, "therm_zone1": false, "therm_zone2": false, "therm_zone3": false, "therm_zone4": false}
	ps, _ := filepath.Glob(filepath.Join(root, "class/thermal/thermal_zone*"))
	for _, p := range ps {
		name, e := read(p + "/type")
		if e != nil {
			continue
		}
		_, known := expected[name]
		str, e := read(p + "/temp")
		if e != nil {
			if known {
				return out, fmt.Errorf("required CPU temperature %s unreadable: %w", name, e)
			}
			continue
		}
		v, e := strconv.ParseInt(str, 10, 64)
		if e != nil || v < -40000 || v > 200000 {
			if known {
				return out, fmt.Errorf("required CPU temperature %s invalid: %q", name, str)
			}
			continue
		}
		role := "OTHER"
		if known {
			expected[name] = true
			role = "CPU"
			if name == "therm_zone2" {
				role = "GPU"
			}
			if name == "therm_zone3" {
				role = "ISP"
			}
			haveCPU = true
			if v < 5000 {
				return out, fmt.Errorf("CPU temperature units/value not credible for this native workload guard")
			}
		}
		out = append(out, ThermalReading{name, p + "/temp", v, role})
		if mode, e := read(p + "/mode"); known {
			if e != nil || (mode != "enabled" && mode != "disabled") {
				return out, fmt.Errorf("kernel CPU thermal zone %s has invalid mode: %q (%v)", name, mode, e)
			}
			// e418 dynamically disables a zone when its CPU cluster is hotplugged
			// out. Temperatures remain readable, and this independent guard still
			// enforces unchanged 71/45 C cutoffs without writing thermal sysfs.
			if mode == "enabled" {
				enabledCPUZones++
			}
		}
		// 71 C is the lowest CPU hot threshold in the supplied thermal_info_config.
		// The native guard stops load here, instead of removing kernel throttling.
		if known && v >= 71000 {
			return out, fmt.Errorf("%s reached conservative 71 C threshold", role)
		}
	}
	batteryPath := filepath.Join(root, "class/power_supply/battery/temp")
	b, e := read(batteryPath)
	if e != nil {
		return out, fmt.Errorf("battery temperature unavailable")
	}
	v, e := strconv.ParseInt(b, 10, 64)
	if e != nil || v < 0 || v > 800 {
		return out, fmt.Errorf("battery temperature not credible")
	}
	out = append(out, ThermalReading{"battery", batteryPath, v * 100, "BATTERY"})
	// Extra conservative workload cutoff, not a claimed Samsung thermal limit.
	if v >= 450 {
		return out, fmt.Errorf("battery reached conservative native-workload cutoff 45 C")
	}
	for name, found := range expected {
		if !found {
			return out, fmt.Errorf("required supplied-firmware CPU sensor %s unavailable", name)
		}
	}
	if !haveCPU {
		return out, fmt.Errorf("base-firmware therm_zone CPU sensors not found")
	}
	if enabledCPUZones == 0 {
		return out, fmt.Errorf("all kernel CPU thermal zones are disabled")
	}
	return out, nil
}
func ProbeReport() map[string]any {
	r := map[string]any{"schema": "S7-NATIVE-DIAGNOSTICS-1", "hardware_acceptance": false, "android_framework": false, "android_services": []string{}, "sensor_hub_uses_private_bionic": true, "protected_partitions_written": false, "cache_policy": "optional settings only after explicit consent", "camera_backend": "native graph/provider and 17 source profiles (9 normal30fps, 4 normal60fps, 2 diagnostic120fps, 2 diagnostic240fps); bounded local NV12 trial only for high FPS; MFC/USB/Windows unverified", "audio_backend": "native ALSA available; explicit opt-in; hardware not accepted"}
	for k, p := range map[string]string{"kernel": "/proc/sys/kernel/osrelease", "model": "/proc/device-tree/model", "cmdline": "/proc/cmdline", "asound_cards": "/proc/asound/cards", "filesystems": "/proc/filesystems"} {
		if v, e := linuxio.ReadText(p); e == nil {
			r[k] = v
		} else {
			r[k+"_error"] = e.Error()
		}
	}
	r["v4l2"] = media.Discover()
	// Samsung sensor-ID reads can initialize calibration hardware. Delay them
	// until an explicit camera request, rather than waking cameras for a report.
	r["camera_sysfs"] = "deferred until camera request"
	r["camera_metadata_contract"] = fimcshot.Contract()
	r["camera_graph_contract"] = fimcgraph.Contract()
	tr, e := Thermal()
	r["thermal"] = tr
	if e != nil {
		r["thermal_gate_error"] = e.Error()
	}
	for k, p := range map[string]string{"framebuffers": "/sys/class/graphics/fb*", "input": "/sys/class/input/event*", "udc": "/sys/class/udc/*", "cooling": "/sys/class/thermal/cooling_device*"} {
		v, _ := filepath.Glob(p)
		r[k] = v
	}
	return r
}
func SaveReport(p string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(p, append(b, '\n'), 0600)
}

// FirmwareFallback services only firmware files placed in the immutable ramdisk.
// It never loads Android ELF libraries, APKs, or user-supplied executable paths.
func FirmwareFallback(env map[string]string) error {
	name := env["FIRMWARE"]
	dev := env["DEVPATH"]
	if env["SUBSYSTEM"] != "firmware" || env["ACTION"] != "add" {
		return nil
	}
	if name == "" || filepath.Clean(name) != name || filepath.IsAbs(name) || strings.HasPrefix(name, ".") || strings.Contains(name, "\\") || !strings.HasPrefix(dev, "/devices/") || strings.Contains(dev, "..") || strings.ContainsRune(dev, 0) {
		return fmt.Errorf("unsafe firmware request")
	}
	sys := "/sys" + dev
	data, e := os.ReadFile("/lib/firmware/" + name)
	if e != nil {
		_ = linuxio.WriteAttr(sys+"/loading", "-1\n")
		return e
	}
	if len(data) > 8<<20 {
		_ = linuxio.WriteAttr(sys+"/loading", "-1\n")
		return fmt.Errorf("firmware oversized")
	}
	if e = linuxio.WriteAttr(sys+"/loading", "1\n"); e != nil {
		return e
	}
	f, e := os.OpenFile(sys+"/data", os.O_WRONLY, 0)
	if e != nil {
		_ = linuxio.WriteAttr(sys+"/loading", "-1\n")
		return e
	}
	e = writeFirmware(f, data)
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		_ = linuxio.WriteAttr(sys+"/loading", "-1\n")
		return e
	}
	return linuxio.WriteAttr(sys+"/loading", "0\n")
}

var watchdogFiles []*os.File // Retain ownership until kernel poweroff; no magic-close disarm.

func StartWatchdog(ctx context.Context, health *safety.Liveness, trip context.CancelFunc) (func(), error) {
	if health == nil || trip == nil {
		return nil, fmt.Errorf("watchdog needs liveness guard")
	}
	f, e := os.OpenFile("/dev/watchdog", os.O_WRONLY, 0)
	if e != nil {
		return nil, fmt.Errorf("hardware watchdog unavailable: %w", e)
	}
	watchdogFiles = append(watchdogFiles, f)
	timeout := int32(30)
	if e = linuxio.Ioctl(int(f.Fd()), 0xc0045706, unsafe.Pointer(&timeout)); e != nil || timeout < 10 || timeout > 60 {
		return nil, fmt.Errorf("watchdog timeout not confirmed (%d): %v", timeout, e)
	}
	if _, e = f.Write([]byte{0}); e != nil {
		return nil, e
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if evidence := health.Check(); !evidence.Healthy {
					health.RecordFailure(evidence)
					log.Printf("liveness lost; no more watchdog feeds: %+v", evidence)
					trip()
					return
				}
				if _, e := f.Write([]byte{0}); e != nil {
					log.Printf("watchdog feed failed: %v", e)
					trip()
					return
				}
			}
		}
	}()
	return func() { <-done }, nil
}

// StopMachine is callable only by the actual PID1 and never writes a boot image.
func StopMachine() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("poweroff refused outside PID1")
	}
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
}

// Laboratory images use recovery as the only automatic exit while USB supplies
// power. Samsung's normal poweroff path reboots into charging in that state.
func ReturnToRecovery() error {
	if os.Getpid() != 1 {
		return fmt.Errorf("recovery request refused outside PID1")
	}
	command, err := syscall.BytePtrFromString("recovery")
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall6(syscall.SYS_REBOOT, syscall.LINUX_REBOOT_MAGIC1,
		syscall.LINUX_REBOOT_MAGIC2, syscall.LINUX_REBOOT_CMD_RESTART2,
		uintptr(unsafe.Pointer(command)), 0, 0)
	runtime.KeepAlive(command)
	if errno != 0 {
		return errno
	}
	return nil
}

// Sysfs binary attributes on older kernels may cap a write to PAGE_SIZE.
// Advance across short writes, do not abort a valid multi-page firmware request.
func writeFirmware(w io.Writer, data []byte) error {
	for pos := 0; pos < len(data); {
		end := min(pos+4096, len(data))
		n, e := w.Write(data[pos:end])
		if n < 0 || n > end-pos {
			return fmt.Errorf("invalid firmware writer count")
		}
		pos += n
		if e != nil && e != io.ErrShortWrite {
			return e
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

// READ_ALL is non-destructive. No printk buffer clear or partition log file.
func KernelMessages() string {
	b := make([]byte, 64<<10)
	n, _, err := syscall.Syscall(syscall.SYS_SYSLOG, 3, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	runtime.KeepAlive(b)
	if err != 0 {
		return "kernel log unavailable: " + err.Error()
	}
	if n > uintptr(len(b)) {
		return "kernel log length rejected"
	}
	return string(b[:n])
}

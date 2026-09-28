//go:build linux && (amd64 || arm64)

package camera

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"perimode/native/internal/camera/fimcdma"
	"perimode/native/internal/camera/fimcshot"
	"perimode/native/internal/media"
)

// These tables translate the pinned libexynoscamera3.so and the matched kernel,
// not a different S7's specification. Provenance with addresses and bytes is in
// hardware/source-config/camera-{profile,rate}-evidence.json. Boot admission checks
// the generated JSON against this generator. No sensor probing writes occur here.
// All modes remain hardware-unaccepted; these plans enable the first real start
// attempt instead of returning a permanent ErrHardwareProfile.
type modeGeometry struct {
	sensorW, sensorH, activeW, activeH uint32
	setfile                            uint32
}

func nativeModeGeometry(sensor Sensor, mode Mode) (modeGeometry, error) {
	if !mode.Valid(sensor) {
		return modeGeometry{}, fmt.Errorf("unsupported camera mode")
	}
	if sensor == Front { // preview LUT 0x1550e8; normal companion path, recording setfile 1
		return modeGeometry{2608, 1960, 2592, 1944, 1}, nil
	}
	if mode.FPS == 60 {
		// Both pinned module tables (29/109) have 4032x2268 at 60 FPS.
		// HAL plain recording branch: min=max=60 -> setfile 6.
		// Source bytes and module-to-table pointer chain are audited in
		// hardware/source-config/camera-rate-evidence.json. Do not use the
		// 2016x1134 120-FPS LUT or the 4032x3024 30-FPS sensor mode here.
		return modeGeometry{4032, 2268, 4032, 2268, 6}, nil
	}
	if mode.FPS == 120 { // high-speed LUT 0x15abe8; setfile branch 0xd05f4
		return modeGeometry{2016, 1134, 2016, 1134, 4}, nil
	}
	if mode.FPS == 240 {
		// Pinned module29/109 row 5: 2016x1134 at 240 FPS. The reviewed
		// plain-recording HAL branch selects setfile 27, not the 120-FPS 4.
		// The same source-derived profile is used by capture and USB.
		return modeGeometry{2016, 1134, 2016, 1134, 27}, nil
	}
	if mode.FPS != 30 {
		return modeGeometry{}, fmt.Errorf("native %d fps profile missing: source-derived sensor geometry/setfile not yet reviewed", mode.FPS)
	}
	setfile := uint32(1)
	// Normal full-frame preview LUT 0x15a8d0. Output aspect crop is downstream
	// of Bayer processing. QHD uses a native center crop, never a 720p upscale.
	return modeGeometry{4032, 3024, 4032, 3024, setfile}, nil
}
func profileNode(video uint32, name string) NativeNode {
	// QUERYCAP is a stub on some 481 nodes; these labels are descriptive ONLY.
	// The owner uses exact sysfs name + dev_t, and pins the kernel before open.
	return NativeNode{Video: video, Name: name, Driver: "exynos-fimc-is", Card: name}
}
func bayerFormat(kind, w, h uint32) media.Format {
	f := media.Format{Type: kind}
	f.Set(w, h, 0x32525942, 2)
	stride := (w*2 + 15) &^ uint32(15)
	f.SetPlaneSize(0, stride*h)
	f.SetPlaneSize(1, 32768)
	binary.LittleEndian.PutUint32(f.Raw[24:], stride)
	return f
}
func outputFormat(w, h uint32) media.Format {
	f := media.Format{Type: media.Capture}
	f.Set(w, h, media.NV12M, 3)
	stride := (w + 15) &^ uint32(15)
	f.SetPlaneSize(0, stride*h)
	f.SetPlaneSize(1, stride*(h/2))
	f.SetPlaneSize(2, 32768)
	binary.LittleEndian.PutUint32(f.Raw[24:], stride)
	binary.LittleEndian.PutUint32(f.Raw[44:], stride)
	return f
}
func centeredAspect(w, h, ow, oh uint32) fimcshot.Rect {
	cw, ch := w, h
	if uint64(w)*uint64(oh) > uint64(h)*uint64(ow) {
		cw = uint32(uint64(h)*uint64(ow)/uint64(oh)) &^ 1
	} else {
		ch = uint32(uint64(w)*uint64(oh)/uint64(ow)) &^ 1
	}
	return fimcshot.Rect{X: ((w - cw) / 2) &^ 1, Y: ((h - ch) / 2) &^ 1, Width: cw, Height: ch}
}
func knownModule(sensor Sensor, module uint32) bool {
	return (sensor == Rear && (module == 29 || module == 109)) || (sensor == Front && module == 21)
}
func makeHeroltePlan(sensor Sensor, module uint32, mode Mode, pins []NativeFirmware) (NativePlan, error) {
	if !knownModule(sensor, module) {
		return NativePlan{}, fmt.Errorf("unknown physical camera module %d", module)
	}
	g, e := nativeModeGeometry(sensor, mode)
	if e != nil {
		return NativePlan{}, e
	}
	physical := profileNode(101, "exynos-fimc-is-ss0")
	companion := profileNode(109, "exynos-fimc-is-pre0")
	// isCompanion() 0xaf650 with all CC disable flags clear returns true for
	// BOTH sensors. m_getFliteNodenum() then uses 101 even for the front camera.
	// No fallback to another physical node is attempted after an ioctl failure.
	sensorRect := fimcshot.Rect{Width: g.sensorW, Height: g.sensorH}
	active := fimcshot.Rect{X: ((g.sensorW - g.activeW) / 2) &^ 1, Y: ((g.sensorH - g.activeH) / 2) &^ 1, Width: g.activeW, Height: g.activeH}
	bayerRect := fimcshot.Rect{Width: g.activeW, Height: g.activeH}
	target := fimcshot.Rect{Width: mode.Width, Height: mode.Height}
	opticA, opticF := float32(1.7), float32(4.2) // sensor info +0x214/+0x21c <- VA 0xe1f90
	if sensor == Front {
		opticA, opticF = 1.9, 2.2
	} // VA 0xe1e50; use binary, not marketing spec
	controls := fimcshot.AutoControls()
	if sensor == Rear {
		controls.Focus = fimcshot.FocusContinuousVideo
	}
	limits, e := controlLimits(sensor, module, mode, active)
	if e != nil {
		return NativePlan{}, e
	}
	// Kernel groupmgr reserves 3/4/5 OTF shots at 30/60/120+ FPS.
	// RAW also stays leased by the following ISP stage (three shots at 240).
	// FLITE pixels are unused on this OTF route; its two recycled slots are
	// independent of the deeper processing queues and their shared RAW memory.
	buffers := uint32(4)
	if mode.FPS == 60 {
		buffers = 6
	} else if mode.FPS > 60 {
		buffers = 8
	}
	p := NativePlan{KernelABI: fimcdma.Herolte481ABI, ID: fmt.Sprintf("herolte481-%s-module%d-%dx%d-%d", strings.ToLower(sensor.String()), module, mode.Width, mode.Height, mode.FPS), Sensor: sensor, Mode: mode, ModuleID: module, SensorTo3AAOTF: true,
		Buffers: buffers, PhysicalBuffers: 2, Physical: physical, Companion: &companion,
		SensorConfig: fimcdma.SensorConfig{KernelABI: fimcdma.Herolte481ABI, Driver: physical.Driver, Card: physical.Card, ModuleID: module, FPS: mode.FPS, Format: bayerFormat(media.Capture, g.sensorW, g.sensorH), Role: fimcdma.ShotMetadata},
		Profile:      fimcshot.Profile{Setfile: g.setfile, YUVRange: 1, Crop: active, FPSMin: mode.FPS, FPSMax: mode.FPS, Aperture: opticA, FocalLength: opticF, CompensationStep: .1},
		Limits:       limits, Controls: controls, Firmware: append([]NativeFirmware(nil), pins...)}
	cfg := func(video, upstream uint32, name string, conn uint8, leader bool, format media.Format, role fimcdma.MetadataRole) NativeProcessingNode {
		n := profileNode(video, name)
		return NativeProcessingNode{Node: n, Config: fimcdma.NodeConfig{KernelABI: p.KernelABI, Driver: n.Driver, Card: n.Card, Route: fimcdma.Route{UpstreamNode: upstream, Connection: conn, Leader: leader, ModuleID: module}, Format: format, Role: role}}
	}
	// OTF 3AA leader's image buffer is the HAL's 32x64 dummy, NOT sensor RAW.
	p.Processing[0] = cfg(110, 101, "exynos-fimc-is-30s", 1, true, bayerFormat(media.Output, 32, 64), fimcdma.ShotMetadata)
	p.Processing[1] = cfg(112, 110, "exynos-fimc-is-30p", 1, false, bayerFormat(media.Capture, g.activeW, g.activeH), fimcdma.StreamMetadata)
	p.Processing[2] = cfg(130, 112, "exynos-fimc-is-i0s", 0, false, bayerFormat(media.Output, g.activeW, g.activeH), fimcdma.ShotMetadata)
	p.Processing[3] = cfg(170, 160, "exynos-fimc-is-m0p", 1, false, outputFormat(mode.Width, mode.Height), fimcdma.StreamMetadata)
	p.RoutesOnly = []NativeRouteOnly{{Node: profileNode(160, "exynos-fimc-is-m0s"), Route: fimcdma.Route{UpstreamNode: 130, Connection: 1, ModuleID: module}}}
	p.ConfigureOrder = []uint32{110, 112, 130, 160, 170}
	p.FliteGroup = fimcshot.Group{Leader: fimcshot.Node{VideoID: 1, Requested: true, Input: sensorRect, Output: sensorRect}}
	p.RawGroup = fimcshot.Group{Leader: fimcshot.Node{VideoID: 10, Requested: true, Input: active, Output: bayerRect}}
	p.RawGroup.Capture[0] = fimcshot.Node{VideoID: 12, Requested: true, Input: bayerRect, Output: bayerRect}
	p.ISPGroup = fimcshot.Group{Leader: fimcshot.Node{VideoID: 30, Requested: true, Input: bayerRect, Output: bayerRect}}
	p.ISPGroup.Capture[0] = fimcshot.Node{VideoID: 70, Requested: true, Input: centeredAspect(g.activeW, g.activeH, mode.Width, mode.Height), Output: target}
	if mode.FPS > 30 {
		// Normal (non-reprocessing) e418 streams ignore per-shot setfile changes.
		// The 3AA leader's documented control initializes device->setfile before
		// STREAMON selects the firmware tuning and stock high-speed DVFS policy.
		selector := g.setfile
		p.Processing[0].Setfile = &selector
	}
	if e = p.Validate(); e != nil {
		return NativePlan{}, fmt.Errorf("%s: %w", p.ID, e)
	}
	return p, nil
}

// GenerateHerolteProfiles runs on the build host. Firmware contents must be the
// ones already approved in the project firmware manifest, which the BOOT tool
// independently checks. It never executes a vendor binary or accesses devices.
func GenerateHerolteProfiles(firmwareDir string) (NativeProfiles, error) {
	var pins []NativeFirmware
	entries, e := os.ReadDir(firmwareDir)
	if e != nil {
		return NativeProfiles{}, e
	}
	for _, entry := range entries {
		name := entry.Name()
		if !(strings.HasPrefix(name, "fimc_is_fw2_") || strings.HasPrefix(name, "setfile_") || strings.HasPrefix(name, "companion_")) || !strings.HasSuffix(name, ".bin") {
			continue
		}
		path := filepath.Join(firmwareDir, name)
		st, e := os.Lstat(path)
		if e != nil {
			return NativeProfiles{}, e
		}
		if !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > 8<<20 {
			return NativeProfiles{}, fmt.Errorf("invalid firmware file %s", name)
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return NativeProfiles{}, e
		}
		h := sha256.Sum256(b)
		pins = append(pins, NativeFirmware{Name: name, SHA256: hex.EncodeToString(h[:])})
	}
	if len(pins) != 22 {
		return NativeProfiles{}, fmt.Errorf("expected 22 supplied FIMC/companion/setfile blobs, got %d", len(pins))
	}
	sort.Slice(pins, func(i, j int) bool { return pins[i].Name < pins[j].Name })
	out := NativeProfiles{Schema: "S7_NATIVE_CAMERA_PROFILES_1", SourceSHA256: CameraHALSourceSHA256}
	for _, module := range []uint32{29, 109, 21} {
		sensor := Rear
		modes := []Mode{{Width: 2560, Height: 1440, FPS: 30}, {Width: 1920, Height: 1080, FPS: 60}, {Width: 1920, Height: 1080, FPS: 30}, {Width: 1280, Height: 720, FPS: 240}, {Width: 1280, Height: 720, FPS: 120}, {Width: 1280, Height: 720, FPS: 60}, {Width: 1280, Height: 720, FPS: 30}}
		if module == 21 {
			sensor = Front
			modes = []Mode{{Width: 2560, Height: 1440, FPS: 30}, {Width: 1920, Height: 1080, FPS: 30}, {Width: 1280, Height: 720, FPS: 30}}
		}
		for _, mode := range modes {
			p, e := makeHeroltePlan(sensor, module, mode, pins)
			if e != nil {
				return NativeProfiles{}, e
			}
			out.Plans = append(out.Plans, p)
		}
	}
	return out, nil
}

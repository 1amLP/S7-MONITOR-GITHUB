//go:build linux && (amd64 || arm64)

package camera

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"perimode/native/internal/camera/fimcdma"
	"perimode/native/internal/camera/fimcgraph"
	"perimode/native/internal/camera/fimcshot"
	"perimode/native/internal/media"
)

const CameraHALSourceSHA256 = "9604961bf7e4e8c657b70185f359cb87c368769eb71d24ea6d4b58f5ac8045f3"

var ErrHardwareProfile = errors.New("complete source-derived native camera profile is not installed")

// This is a build-time hardware description, not a menu setting or a claim of
// device acceptance. An empty registry exposes no modes. Do not ship test plans.
type NativeProfiles struct {
	Schema       string       `json:"schema"`
	SourceSHA256 string       `json:"source_sha256"`
	Plans        []NativePlan `json:"plans"`
}
type NativeNode struct {
	Video  uint32 `json:"video"`
	Name   string `json:"sysfs_name"`
	Driver string `json:"driver"`
	Card   string `json:"card"`
}

func (n NativeNode) filename() string { return fmt.Sprintf("video%d", n.Video) }
func (n NativeNode) validate() error {
	if n.Video < 100 || n.Video > 355 || n.Name == "" || len(n.Name) > 64 || n.Driver == "" || len(n.Driver) >= 16 || n.Card == "" || len(n.Card) >= 32 || strings.ContainsAny(n.Name+n.Driver+n.Card, "\x00\r\n/") {
		return fmt.Errorf("bounded exact camera node identity required")
	}
	return nil
}

type NativeProcessingNode struct {
	Node    NativeNode         `json:"node"`
	Config  fimcdma.NodeConfig `json:"config"`
	Setfile *uint32            `json:"setfile_control,omitempty"`
}

// NativeRouteOnly selects an OTF route without inventing a DMA format.
// The HAL opens MCSC leader 160 and sets its input, but skips S_FMT when ISP-MCSC is OTF.
type NativeRouteOnly struct {
	Node  NativeNode    `json:"node"`
	Route fimcdma.Route `json:"route"`
}

type NativeFirmware struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}
type NativePlan struct {
	KernelABI      string   `json:"kernel_abi,omitempty"`
	ConfigureOrder []uint32 `json:"configure_order"`
	ID             string   `json:"id"`
	Sensor         Sensor   `json:"sensor"`
	Mode           Mode     `json:"mode"`
	ModuleID       uint32   `json:"module_id"`
	// Only the existing OTF FLITE -> OTF 3AA -> M2M ISP -> OTF MCSC path is supported.
	SensorTo3AAOTF  bool                 `json:"sensor_to_3aa_otf"`
	Buffers         uint32               `json:"buffers"`
	PhysicalBuffers uint32               `json:"physical_buffers,omitempty"`
	Physical        NativeNode           `json:"physical"`
	Companion       *NativeNode          `json:"companion,omitempty"`
	SensorConfig    fimcdma.SensorConfig `json:"sensor_config"`
	// Ordered configuration-only OTF nodes. They get no fabricated input buffers.
	RoutesOnly    []NativeRouteOnly       `json:"routes_only,omitempty"`
	ConfigureOnly []NativeProcessingNode  `json:"configure_only,omitempty"`
	Processing    [4]NativeProcessingNode `json:"processing"`
	Profile       fimcshot.Profile        `json:"shot_profile"`
	Limits        fimcshot.Limits         `json:"control_limits"`
	Controls      fimcshot.Controls       `json:"initial_controls"`
	FliteGroup    fimcshot.Group          `json:"flite_group"`
	RawGroup      fimcshot.Group          `json:"raw_group"`
	ISPGroup      fimcshot.Group          `json:"isp_group"`
	Firmware      []NativeFirmware        `json:"firmware"`
}

func hexSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
func (p NativePlan) physicalBuffers() uint32 {
	if p.PhysicalBuffers == 0 {
		return p.Buffers
	}
	return p.PhysicalBuffers
}
func (p NativePlan) Validate() error {
	if e := fimcdma.ValidateKernelABI(p.KernelABI); e != nil {
		return e
	}
	if p.SensorConfig.KernelABI != p.KernelABI {
		return fmt.Errorf("sensor/kernel ABI mismatch")
	}
	if p.ID == "" || len(p.ID) > 96 || !p.Mode.Valid(p.Sensor) || p.ModuleID == 0 || p.ModuleID > 255 || !p.SensorTo3AAOTF || p.Buffers < 2 || p.Buffers > 8 || len(p.ConfigureOnly) > 4 || len(p.RoutesOnly) > 4 {
		return fmt.Errorf("incomplete native camera mode/topology")
	}
	if p.physicalBuffers() < 2 || p.physicalBuffers() > 8 {
		return fmt.Errorf("physical camera queue requires 2..8 buffers")
	}
	if e := p.Physical.validate(); e != nil {
		return e
	}
	if e := p.SensorConfig.Validate(); e != nil {
		return e
	}
	if p.SensorConfig.Driver != p.Physical.Driver || p.SensorConfig.Card != p.Physical.Card || p.SensorConfig.ModuleID != p.ModuleID || p.SensorConfig.FPS != p.Mode.FPS {
		return fmt.Errorf("physical sensor profile mismatch")
	}
	seen := map[uint32]bool{p.Physical.Video: true}
	if p.Companion != nil {
		if e := p.Companion.validate(); e != nil {
			return e
		}
		if seen[p.Companion.Video] {
			return fmt.Errorf("duplicate companion node")
		}
		seen[p.Companion.Video] = true
	}
	for _, n := range p.RoutesOnly {
		if e := n.Node.validate(); e != nil {
			return e
		}
		if seen[n.Node.Video] {
			return fmt.Errorf("duplicate OTF route node")
		}
		seen[n.Node.Video] = true
		if _, e := n.Route.Selector(); e != nil {
			return e
		}
		if n.Route.ModuleID != p.ModuleID || n.Route.Reprocessing || n.Route.Connection != 1 {
			return fmt.Errorf("invalid OTF-only route")
		}
	}
	var allocatedBytes uint64
	sf := p.SensorConfig.Format
	for j := 0; j < int(sf.Planes()); j++ {
		allocatedBytes += uint64((sf.PlaneSize(j)+4095)&^uint32(4095)) * uint64(p.physicalBuffers())
	}
	for i, n := range append(append([]NativeProcessingNode{}, p.ConfigureOnly...), p.Processing[:]...) {
		if e := n.Node.validate(); e != nil {
			return e
		}
		if seen[n.Node.Video] {
			return fmt.Errorf("duplicate physical processing node")
		}
		seen[n.Node.Video] = true
		if e := n.Config.Validate(); e != nil {
			return e
		}
		if n.Config.KernelABI != p.KernelABI || n.Config.Driver != n.Node.Driver || n.Config.Card != n.Node.Card || n.Config.Route.ModuleID != p.ModuleID || n.Config.Route.Reprocessing {
			return fmt.Errorf("processing identity/route mismatch")
		}
		if n.Setfile != nil && (*n.Setfile > 0xffff || n.Config.Role != fimcdma.ShotMetadata || n.Config.Format.Type != media.Output) {
			return fmt.Errorf("invalid setfile-control target")
		}
		if i >= len(p.ConfigureOnly) {
			j := i - len(p.ConfigureOnly)
			typ, role := uint32(media.Output), fimcdma.ShotMetadata
			if j%2 == 1 {
				typ, role = media.Capture, fimcdma.StreamMetadata
			}
			if n.Config.Format.Type != typ || n.Config.Role != role {
				return fmt.Errorf("graph queue role/type mismatch")
			}
			f := n.Config.Format
			count := p.Buffers
			if j == 3 {
				count = max(count, fimcgraph.ConsumerCaptureBuffers)
			}
			for k := 0; k < int(f.Planes()); k++ {
				// ISP imports the RAW capture's pixel allocation; only its shot
				// metadata is private. MCSC reserves slots for both consumers.
				if j == 2 && k < int(f.Planes())-1 {
					continue
				}
				allocatedBytes += uint64((f.PlaneSize(k)+4095)&^uint32(4095)) * uint64(count)
			}
		}
	}
	// Routing order belongs to the source-derived profile, not a guessed global sequence.
	if len(p.ConfigureOrder) != len(p.ConfigureOnly)+len(p.Processing)+len(p.RoutesOnly) {
		return fmt.Errorf("complete processing configuration order required")
	}
	ordered := map[uint32]bool{}
	for _, id := range p.ConfigureOrder {
		if !seen[id] || id == p.Physical.Video || (p.Companion != nil && id == p.Companion.Video) || ordered[id] {
			return fmt.Errorf("invalid processing configuration order")
		}
		ordered[id] = true
	}
	if allocatedBytes > fimcdma.MaxAllocatorBytes {
		return fmt.Errorf("camera plan exceeds the native ION budget")
	}
	if e := fimcdma.VerifyRawLink(p.Processing[1].Config.Format, p.Processing[2].Config.Format); e != nil {
		return e
	}
	output := p.Processing[3].Config.Format
	if output.Width() != p.Mode.Width || output.Height() != p.Mode.Height || !(output.PixelFormat() == media.NV12 || output.PixelFormat() == media.NV12M) {
		return fmt.Errorf("output is not exact target-size NV12")
	}
	if p.Profile.FPSMin != p.Mode.FPS || p.Profile.FPSMax != p.Mode.FPS || p.Limits.FrameDurationNS != 1_000_000_000/uint64(p.Mode.FPS) || p.Profile.Crop != p.Limits.Crop {
		return fmt.Errorf("shot profile/FPS/control limits mismatch")
	}
	if p.Mode.FPS > 30 {
		selector := p.Processing[0].Setfile
		if selector == nil || *selector != p.Profile.Setfile || p.Processing[0].Node.Video != 110 {
			return fmt.Errorf("high-speed capture requires matching 3AA setfile control before STREAMON")
		}
		for _, n := range append(append([]NativeProcessingNode{}, p.ConfigureOnly...), p.Processing[1:]...) {
			if n.Setfile != nil {
				return fmt.Errorf("high-speed setfile must have one control owner")
			}
		}
	}
	template, e := fimcshot.NewTemplate(p.Profile)
	if e != nil {
		return e
	}
	var scratch [fimcshot.Size]byte
	v, e := template.Reset(scratch[:])
	if e != nil {
		return e
	}
	if e = v.ApplyControls(p.Controls, p.Limits); e != nil {
		return e
	}
	for _, g := range []fimcshot.Group{p.FliteGroup, p.RawGroup, p.ISPGroup} {
		if !g.Leader.Requested {
			return fmt.Errorf("graph leader request missing")
		}
		if e = v.SetGroup(g); e != nil {
			return e
		}
	}
	if len(p.Firmware) == 0 || len(p.Firmware) > 32 {
		return fmt.Errorf("pinned firmware prerequisites required")
	}
	names := map[string]bool{}
	for _, f := range p.Firmware {
		if filepath.Base(f.Name) != f.Name || !strings.HasSuffix(f.Name, ".bin") || len(f.Name) > 96 || names[f.Name] || !hexSHA(f.SHA256) {
			return fmt.Errorf("invalid firmware pin")
		}
		names[f.Name] = true
	}
	return nil
}
func (p NativePlan) requests() (fimcgraph.Request, fimcgraph.Request, error) {
	t, e := fimcshot.NewTemplate(p.Profile)
	if e != nil {
		return fimcgraph.Request{}, fimcgraph.Request{}, e
	}
	flite := fimcgraph.Request{Template: &t, Group: p.FliteGroup, Controls: p.Controls, Limits: p.Limits}
	raw := flite
	raw.Group = p.RawGroup
	return flite, raw, nil
}
func ParseNativeProfiles(data []byte) (NativeProfiles, error) {
	var p NativeProfiles
	if len(data) == 0 || len(data) > 256<<10 {
		return p, fmt.Errorf("camera profile size limit")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e := d.Decode(&p); e != nil {
		return p, e
	}
	var trailing any
	if e := d.Decode(&trailing); e != io.EOF {
		return p, fmt.Errorf("trailing profile content")
	}
	if p.Schema != "S7_NATIVE_CAMERA_PROFILES_1" || p.SourceSHA256 != CameraHALSourceSHA256 || len(p.Plans) > 32 {
		return p, fmt.Errorf("incompatible camera profile schema/source")
	}
	seen := map[string]bool{}
	for _, v := range p.Plans {
		if e := v.Validate(); e != nil {
			return p, fmt.Errorf("profile %q: %w", v.ID, e)
		}
		k := fmt.Sprintf("%d/%d/%v", v.Sensor, v.ModuleID, v.Mode)
		if seen[k] {
			return p, fmt.Errorf("duplicate camera mode profile")
		}
		seen[k] = true
	}
	return p, nil
}
func ReadNativeProfiles(path string) (NativeProfiles, error) {
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return NativeProfiles{}, e
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return NativeProfiles{}, e
	}
	if !st.Mode().IsRegular() || st.Size() > 256<<10 {
		return NativeProfiles{}, fmt.Errorf("regular bounded camera profile required")
	}
	data, e := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	if e != nil {
		return NativeProfiles{}, e
	}
	return ParseNativeProfiles(data)
}

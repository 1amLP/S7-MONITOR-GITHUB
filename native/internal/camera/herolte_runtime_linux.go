//go:build linux && (amd64 || arm64)

package camera

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"perimode/native/internal/camera/fimcdma"
	"perimode/native/internal/camera/fimcgraph"
	"perimode/native/internal/camera/fimcshot"
	"perimode/native/internal/media"
)

type nativeSession interface {
	Source
	Start() error
	UpdateControls(fimcshot.Controls) error
}

// nativeGraphOwner is the missing boundary between all the borrowed-fd helpers
// and camera.Source. The graph, FLITE, sensor duplicate, allocator and original
// descriptors stay in ONE object through setup, normal operation and failure.
type nativeGraphOwner struct {
	mu                       sync.Mutex
	graph                    nativeSession
	flite                    io.Closer
	sensor                   io.Closer
	allocator                io.Closer
	files                    []io.Closer
	waitNodes                [5]nativePollFD
	started, closing, closed bool
	closeErr                 error
}

func (o *nativeGraphOwner) Start() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.closing || o.graph == nil {
		return os.ErrClosed
	}
	if o.started {
		return nil
	}
	log.Print("S7 camera graph Start begin")
	if e := o.graph.Start(); e != nil {
		log.Printf("S7 camera graph Start failed: %v", e)
		return errors.Join(e, o.closeLocked())
	}
	log.Print("S7 camera graph Start complete")
	o.started = true
	return nil
}
func (o *nativeGraphOwner) Drain(fn func(media.Image) error) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.started || o.closing || o.closed {
		return 0, os.ErrClosed
	}
	if fn == nil {
		return 0, fmt.Errorf("nil native camera consumer")
	}
	return o.graph.Drain(fn)
}
func (o *nativeGraphOwner) Wait(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.started || o.closing || o.closed {
		return os.ErrClosed
	}
	return waitNativeFrames(ctx, o.waitNodes)
}
func (o *nativeGraphOwner) UpdateControls(c fimcshot.Controls) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.started || o.closing || o.closed || o.graph == nil {
		return os.ErrClosed
	}
	return o.graph.UpdateControls(c)
}
func (o *nativeGraphOwner) Close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closeLocked()
}
func (o *nativeGraphOwner) closeLocked() error {
	if o.closed {
		return o.closeErr
	}
	o.closing = true
	o.started = false
	// Do not run downstream cleanup after an unconfirmed upstream stop.
	if o.graph != nil {
		if e := o.graph.Close(); e != nil {
			return errors.Join(ErrOwnership, e)
		}
		o.graph = nil
	}
	if o.flite != nil {
		if e := o.flite.Close(); e != nil {
			return errors.Join(ErrOwnership, e)
		}
		o.flite = nil
	}
	if o.sensor != nil {
		if e := o.sensor.Close(); e != nil {
			return errors.Join(ErrOwnership, e)
		}
		o.sensor = nil
	}
	if o.allocator != nil {
		if e := o.allocator.Close(); e != nil {
			return errors.Join(ErrOwnership, e)
		}
		o.allocator = nil
	}
	var errs []error
	// os.File.Close is never retried after EINTR/EIO: its fd may already be reused.
	for i := len(o.files) - 1; i >= 0; i-- {
		if o.files[i] != nil {
			if file, ok := o.files[i].(*os.File); ok {
				log.Printf("S7 camera close %s", file.Name())
			}
			errs = append(errs, o.files[i].Close())
			o.files[i] = nil
		}
	}
	o.files = nil
	o.closed = true
	o.closeErr = errors.Join(errs...)
	return o.closeErr
}

func verifyFirmware(root string, pins []NativeFirmware) error {
	root, e := filepath.EvalSymlinks(root)
	if e != nil {
		return e
	}
	for _, p := range pins {
		real, e := filepath.EvalSymlinks(filepath.Join(root, p.Name))
		if e != nil {
			return e
		}
		if !strings.HasPrefix(real, root+string(filepath.Separator)) {
			return fmt.Errorf("firmware path escapes native firmware root")
		}
		f, e := os.Open(real)
		if e != nil {
			return e
		}
		st, e := f.Stat()
		if e != nil {
			f.Close()
			return e
		}
		if !st.Mode().IsRegular() || st.Size() > 8<<20 {
			f.Close()
			return fmt.Errorf("invalid camera firmware extent")
		}
		h := sha256.New()
		_, e = io.Copy(h, io.LimitReader(f, (8<<20)+1))
		ce := f.Close()
		if e != nil || ce != nil {
			return errors.Join(e, ce)
		}
		if hex.EncodeToString(h.Sum(nil)) != p.SHA256 {
			return fmt.Errorf("camera firmware checksum mismatch: %s", p.Name)
		}
	}
	return nil
}
func nodeDeviceNumber(text string) (uint64, error) {
	parts := strings.Split(strings.TrimSpace(text), ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid sysfs dev number")
	}
	a, e := strconv.ParseUint(parts[0], 10, 32)
	if e != nil {
		return 0, e
	}
	b, e := strconv.ParseUint(parts[1], 10, 32)
	if e != nil {
		return 0, e
	}
	return (a&0xfff)<<8 | (a&^uint64(0xfff))<<32 | b&0xff | (b&^uint64(0xff))<<12, nil
}
func openNativeVideo(n NativeNode) (*os.File, error) {
	if e := n.validate(); e != nil {
		return nil, e
	}
	base := "/sys/class/video4linux/" + n.filename()
	name, e := os.ReadFile(base + "/name")
	if e != nil {
		return nil, e
	}
	if strings.TrimSpace(string(name)) != n.Name {
		return nil, fmt.Errorf("camera sysfs identity changed: %s", n.filename())
	}
	dev, e := os.ReadFile(base + "/dev")
	if e != nil {
		return nil, e
	}
	expected, e := nodeDeviceNumber(string(dev))
	if e != nil {
		return nil, e
	}
	path := "/dev/" + n.filename()
	fd, e := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	var st syscall.Stat_t
	if e = syscall.Fstat(fd, &st); e != nil || st.Mode&syscall.S_IFMT != syscall.S_IFCHR || st.Rdev != expected {
		f.Close()
		return nil, fmt.Errorf("camera device/sysfs mismatch %s: %v", path, e)
	}
	// This is a fresh open, not dup() of another queue's open-file description.
	if e = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("camera node owned by another process: %w", e)
	}
	return f, nil
}
func openNativePlan(p NativePlan) (result *nativeGraphOwner, err error) {
	log.Printf("S7 camera plan start %s", p.ID)
	defer func() { log.Printf("S7 camera plan return %s error=%v", p.ID, err) }()
	if err = p.Validate(); err != nil {
		return nil, err
	}
	if err = requireRunningCameraKernel(p.KernelABI); err != nil {
		return nil, err
	}
	if err = verifyFirmware("/lib/firmware", p.Firmware); err != nil {
		return nil, fmt.Errorf("camera profile %s firmware verification: %w", p.ID, err)
	}
	owner := &nativeGraphOwner{}
	defer func() {
		if err != nil {
			log.Printf("S7 camera setup failed before cleanup: %v", err)
			ce := owner.Close()
			err = errors.Join(err, ce)
			if ce != nil {
				result = owner
			} else {
				result = nil
			}
		}
	}()
	open := func(n NativeNode) (*os.File, error) {
		log.Printf("S7 camera open /dev/%s", n.filename())
		f, e := openNativeVideo(n)
		if e != nil {
			return nil, fmt.Errorf("open camera node /dev/%s (%s): %w", n.filename(), n.Name, e)
		}
		owner.files = append(owner.files, f)
		return f, nil
	}
	// Open only explicitly listed nodes. Never enumerate by opening every videoN.
	var companionFD int = -1
	var companion *fimcdma.CompanionConfig
	if p.Companion != nil {
		f, e := open(*p.Companion)
		if e != nil {
			return nil, e
		}
		companionFD = int(f.Fd())
		companion = &fimcdma.CompanionConfig{Driver: p.Companion.Driver, Card: p.Companion.Card, KernelABI: p.KernelABI}
	}
	physical, e := open(p.Physical)
	if e != nil {
		return nil, e
	}
	all := append(append([]NativeProcessingNode{}, p.ConfigureOnly...), p.Processing[:]...)
	descriptors := make([]*os.File, len(all))
	for i, n := range all {
		descriptors[i], e = open(n.Node)
		if e != nil {
			return nil, e
		}
	}
	routeDescriptors := make(map[uint32]*os.File)
	for _, n := range p.RoutesOnly {
		f, e := open(n.Node)
		if e != nil {
			return nil, e
		}
		routeDescriptors[n.Node.Video] = f
	}
	// Pair validation checks identities before either companion or physical S_INPUT.
	log.Print("S7 camera input pair configure")
	sensorNode, _, e := fimcdma.ConfigureInputPair(companionFD, int(physical.Fd()), companion, p.SensorConfig)
	if e != nil {
		return nil, fmt.Errorf("camera profile %s companion/sensor input pair: %w", p.ID, e)
	}
	// All group leaders are open. The pinned kernel requires this control to
	// finish ischain_open_wrap before video110's first S_INPUT.
	leader := p.Processing[0].Node
	log.Print("S7 camera complete chain open")
	if e = fimcdma.CompleteISChainOpen(int(descriptors[len(p.ConfigureOnly)].Fd()), leader.Driver, leader.Card, p.KernelABI); e != nil {
		return nil, fmt.Errorf("camera profile %s FIMC chain open: %w", p.ID, e)
	}
	prepared := make([]*fimcdma.PreparedNode, len(all))
	for _, id := range p.ConfigureOrder {
		log.Printf("S7 camera configure video%d", id)
		if f, ok := routeDescriptors[id]; ok {
			for _, n := range p.RoutesOnly {
				if n.Node.Video == id {
					e = fimcdma.ConfigureRouteOnly(int(f.Fd()), n.Node.Driver, n.Node.Card, p.KernelABI, n.Route)
					if e != nil {
						return nil, fmt.Errorf("route %s: %w", n.Node.filename(), e)
					}
				}
			}
			continue
		}
		i := 0
		for all[i].Node.Video != id {
			i++
		}
		n := all[i]
		prepared[i], _, e = fimcdma.ConfigureNode(int(descriptors[i].Fd()), n.Config)
		if e != nil {
			return nil, fmt.Errorf("configure %s: %w", n.Node.filename(), e)
		}
		if n.Setfile != nil {
			if e = prepared[i].Setfile(*n.Setfile); e != nil {
				return nil, e
			}
		}
	}
	ctl, e := fimcgraph.NewSensorControlABI(int(physical.Fd()), p.Physical.Driver, p.Physical.Card, p.KernelABI)
	if e != nil {
		return nil, fmt.Errorf("camera profile %s sensor control: %w", p.ID, e)
	}
	owner.sensor = ctl
	alloc, e := fimcdma.OpenAllocator()
	if e != nil {
		return nil, fmt.Errorf("camera profile %s ION allocator: %w", p.ID, e)
	}
	owner.allocator = alloc
	fliteRequest, rawRequest, e := p.requests()
	if e != nil {
		return nil, e
	}
	log.Print("S7 camera prepare FLITE")
	flite, e := fimcgraph.PrepareOTFFlite(alloc, sensorNode, ctl, fliteRequest, p.physicalBuffers(), p.SensorTo3AAOTF)
	if flite != nil {
		owner.flite = flite
	}
	if e != nil {
		return nil, fmt.Errorf("camera profile %s FLITE preparation: %w", p.ID, e)
	}
	n := prepared[len(p.ConfigureOnly):]
	for i := 0; i < 4; i++ {
		events := int16(4 | 0x100)
		if i%2 == 1 {
			events = 1 | 0x40
		}
		owner.waitNodes[i] = nativePollFD{FD: int32(descriptors[len(p.ConfigureOnly)+i].Fd()), Events: events}
	}
	owner.waitNodes[4] = nativePollFD{FD: int32(physical.Fd()), Events: 1 | 0x40}
	log.Print("S7 camera prepare processing graph")
	graph, e := fimcgraph.PrepareConfiguredGraph(alloc, fimcgraph.PreparedConfig{RawLeader: n[0], RawCapture: n[1], ISPLeader: n[2], ISPCapture: n[3], Sensor: flite, RawRequest: rawRequest, ISPGroup: p.ISPGroup, Buffers: p.Buffers})
	if graph != nil {
		owner.graph = graph
	}
	if e != nil {
		return nil, fmt.Errorf("camera profile %s 3AA/ISP/MCSC graph preparation: %w", p.ID, e)
	}
	return owner, nil
}

type nativeLookup func(Settings) (NativePlan, error)
type nativeOpener func(NativePlan) (nativeSession, error)
type herolteRuntime struct {
	mu      sync.Mutex
	lookup  nativeLookup
	open    nativeOpener
	active  *herolteSource
	opening bool // reserve the one sensor without holding a lock over ioctls
}
type HerolteProvider struct{ runtime *herolteRuntime }

var defaultHerolteRuntime = &herolteRuntime{lookup: lookupInstalledPlan, open: func(p NativePlan) (nativeSession, error) {
	s, e := openNativePlan(p)
	if s == nil {
		return nil, e
	}
	return s, e
}}

func (p HerolteProvider) rt() *herolteRuntime {
	if p.runtime != nil {
		return p.runtime
	}
	return defaultHerolteRuntime
}
func NewHerolteProvider() Provider { return HerolteProvider{} }

// The registry is immutable in the read-only ramdisk. Parse it once, not once
// per menu row/refresh. Successful physical module identity is fixed for a boot.
var installedRegistry struct {
	once        sync.Once
	profiles    NativeProfiles
	err         error
	inventoryMu sync.Mutex
	inventory   *InventoryReport
}

func installedProfiles() (NativeProfiles, error) {
	installedRegistry.once.Do(func() {
		installedRegistry.profiles, installedRegistry.err = ReadNativeProfiles("/etc/s7-camera/profiles.json")
	})
	return installedRegistry.profiles, installedRegistry.err
}
func lookupInstalledPlan(s Settings) (NativePlan, error) {
	registry, e := installedProfiles()
	if e != nil {
		return NativePlan{}, errors.Join(ErrSensorGraph, ErrHardwareProfile, e)
	}
	p, err := lookupNativePlan(s, registry, requireRunningCameraKernel, func() InventoryReport {
		installedRegistry.inventoryMu.Lock()
		defer installedRegistry.inventoryMu.Unlock()
		side := strings.ToLower(s.Sensor.String())
		if cached := installedRegistry.inventory; cached != nil && cached.SensorModules[side].Valid {
			return *cached
		}
		// These are soldered sensor modules. Reading sensorid can load firmware;
		// serialize that side effect and reuse a successful identity for this boot.
		log.Printf("S7 camera identify modules for %s", side)
		inventory := InspectSysfs("/sys")
		log.Printf("S7 camera identify returned %s valid=%t", side, inventory.SensorModules[side].Valid)
		if inventory.SensorModules[side].Valid {
			installedRegistry.inventory = &inventory
		}
		return inventory
	})
	if err == nil {
		// The pinned shot ABI has custom Kelvin, edge strength and tone curves.
		// Request ranges are bounded here; physical effect is checked on S7.
		p.Limits.ISPImageControls = true
		p.Limits.WBModeMask |= 1 << fimcshot.WBCustomK
	}
	return p, err
}

func lookupNativePlan(s Settings, registry NativeProfiles, checkKernel func(string) error, inspect func() InventoryReport) (NativePlan, error) {
	if len(registry.Plans) == 0 {
		return NativePlan{}, errors.Join(ErrSensorGraph, ErrHardwareProfile)
	}
	// Reading rear_sensorid runs firmware/calibration selection in this kernel.
	// Reject unknown kernels BEFORE that read, not merely before opening videoN.
	matched := false
	for _, p := range registry.Plans {
		if p.Sensor != s.Sensor || p.Mode != s.Mode {
			continue
		}
		if e := checkKernel(p.KernelABI); e != nil {
			return NativePlan{}, e
		}
		matched = true
	}
	if !matched {
		return NativePlan{}, errors.Join(ErrSensorGraph, ErrHardwareProfile, fmt.Errorf("no %s mode %s profile", s.Sensor, s.Mode))
	}
	inv := inspect()
	side := strings.ToLower(s.Sensor.String())
	module, ok := inv.SensorModules[side]
	if !ok || !module.Valid {
		return NativePlan{}, fmt.Errorf("camera %s module identity unavailable: %s", side, module.Error)
	}
	for _, p := range registry.Plans {
		if p.Sensor != s.Sensor || p.Mode != s.Mode || p.ModuleID != module.Value {
			continue
		}
		nodes := []NativeNode{p.Physical}
		if p.Companion != nil {
			nodes = append(nodes, *p.Companion)
		}
		for _, n := range p.RoutesOnly {
			nodes = append(nodes, n.Node)
		}
		for _, n := range p.ConfigureOnly {
			nodes = append(nodes, n.Node)
		}
		for _, n := range p.Processing {
			nodes = append(nodes, n.Node)
		}
		for _, want := range nodes {
			found := false
			for _, have := range inv.Nodes {
				if have.Class == "video4linux" && have.Node == want.filename() && have.Name == want.Name {
					found = true
					break
				}
			}
			if !found {
				return NativePlan{}, fmt.Errorf("profile %s: missing/mismatched %s", p.ID, want.filename())
			}
		}
		return p, nil
	}
	return NativePlan{}, errors.Join(ErrSensorGraph, ErrHardwareProfile, fmt.Errorf("no %s module %d mode %s profile", side, module.Value, s.Mode))
}
func (p HerolteProvider) Available(s Settings) error { return p.available(s, false) }
func (p HerolteProvider) available(s Settings, trial bool) error {
	if e := s.Validate(); e != nil {
		return e
	}
	if !s.Mode.WebcamEligible() && !(trial && isHighFPSTrialSettings(s)) {
		return ErrWebcamUnverified
	}
	if _, e := nativeModeGeometry(s.Sensor, s.Mode); e != nil {
		return e
	}
	r := p.rt()
	r.mu.Lock()
	blocked := r.active != nil && r.active.failed.Load()
	opening := r.opening || r.active != nil && r.active.closing.Load()
	r.mu.Unlock()
	if blocked {
		return ErrOwnership
	}
	// UI capability queries must not wait for slow hardware setup or sensor firmware.
	if opening {
		return ErrCaptureBusy
	}
	plan, e := r.lookup(s)
	if e != nil {
		return e
	}
	if plan.Sensor != s.Sensor || plan.Mode != s.Mode {
		return fmt.Errorf("native resolver returned another camera/mode")
	}
	return plan.Validate()
}
func (p HerolteProvider) Open(s Settings) (Source, error) {
	return p.openSession(s, false)
}

// The diagnostic bypass is private and used by the bounded local trial or the
// explicitly selected, time-limited USB engineering build.
// It shares the SAME exclusive owner, kernel/firmware checks and cleanup.
// Ordinary Provider.Open, USB descriptors and camera protocol remain gated.
func (p HerolteProvider) openSession(s Settings, diagnostic bool) (Source, error) {
	if e := s.Validate(); e != nil {
		return nil, e
	}
	if !s.Mode.WebcamEligible() && !(diagnostic && isHighFPSTrialSettings(s)) {
		return nil, ErrWebcamUnverified
	}
	if _, e := nativeModeGeometry(s.Sensor, s.Mode); e != nil {
		return nil, e
	}
	r := p.rt()
	r.mu.Lock()
	if r.active != nil || r.opening {
		blocked := r.active != nil && r.active.failed.Load()
		r.mu.Unlock()
		if blocked {
			return nil, ErrOwnership
		}
		return nil, ErrCaptureBusy
	}
	r.opening = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.opening = false; r.mu.Unlock() }()
	// The reservation prevents concurrent opens; the mutex is NOT held during
	// filesystem access, construction, streaming ioctls, or rollback.
	plan, e := r.lookup(s)
	if e != nil {
		return nil, e
	}
	if plan.Sensor != s.Sensor || plan.Mode != s.Mode {
		return nil, fmt.Errorf("native resolver returned another camera/mode")
	}
	if e = plan.Validate(); e != nil {
		return nil, e
	}
	inner, e := r.open(plan)
	if e != nil {
		e = fmt.Errorf("native camera profile %s open: %w", plan.ID, e)
	}
	if inner == nil {
		if e == nil {
			e = fmt.Errorf("native opener returned nil session")
		}
		return nil, e
	}
	out := &herolteSource{owner: r, inner: inner, identity: CaptureTrialIdentity{ProfileID: plan.ID, ModuleID: plan.ModuleID, KernelABI: plan.KernelABI, HALSHA256: CameraHALSourceSHA256}}
	r.mu.Lock()
	r.active = out
	r.mu.Unlock()
	if e == nil {
		e = inner.Start()
		if e != nil {
			e = fmt.Errorf("native camera profile %s stream start: %w", plan.ID, e)
		}
	}
	if e != nil {
		ce := inner.Close()
		if ce == nil {
			r.mu.Lock()
			r.active = nil
			r.mu.Unlock()
			return nil, e
		}
		out.failed.Store(true)
		return out, errors.Join(e, ce, ErrOwnership)
	}
	return out, nil
}

type herolteSource struct {
	identity CaptureTrialIdentity
	owner    *herolteRuntime
	inner    nativeSession
	mu       sync.Mutex // Never hold owner.mu across a frame callback.
	failed   atomic.Bool
	closing  atomic.Bool
	closed   bool
}

func (s *herolteSource) CaptureIdentity() CaptureTrialIdentity { return s.identity }

func (s *herolteSource) Drain(fn func(media.Image) error) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed.Load() {
		return 0, os.ErrClosed
	}
	return s.inner.Drain(fn)
}
func (s *herolteSource) Wait(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed.Load() {
		return os.ErrClosed
	}
	if source, ok := s.inner.(interface{ Wait(context.Context) error }); ok {
		return source.Wait(ctx)
	}
	// Injected test sources do not own pollable kernel queues.
	return waitCaptureTick(ctx)
}
func (s *herolteSource) UpdateControls(c fimcshot.Controls) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed.Load() {
		return os.ErrClosed
	}
	return s.inner.UpdateControls(c)
}
func (s *herolteSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closing.Store(true)
	if e := s.inner.Close(); e != nil {
		s.failed.Store(true)
		return errors.Join(e, ErrOwnership)
	}
	s.closed = true
	s.owner.mu.Lock()
	if s.owner.active == s {
		s.owner.active = nil
	}
	s.owner.mu.Unlock()
	return nil
}

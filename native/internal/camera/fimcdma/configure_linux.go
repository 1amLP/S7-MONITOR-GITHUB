//go:build linux && (amd64 || arm64)

package fimcdma

import (
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"perimode/native/internal/camera/fimcshot"
	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
)

// SetInput is VIDIOC_S_INPUT, not a guessed FIMC private control. The packed
// value itself follows the supplied HAL's fimcshot.PackInput contract.
const SetInput uintptr = 0xc0045627

type Route struct {
	UpstreamNode         uint32
	Connection           uint8
	Leader, Reprocessing bool
	ModuleID             uint32
}

func (r Route) Selector() (uint32, error) {
	return fimcshot.PackInput(r.UpstreamNode, r.Connection, r.Leader, r.Reprocessing, r.ModuleID)
}

// NodeConfig is a caller-provided, module-specific plan. This package does not
// guess sensor IDs, video-node numbers, optical crops, Bayer order or setfiles.
// The descriptor must be exclusively owned, with no buffers already requested.
// Role and exact geometry are locked for the lifetime of the prepared node.
type NodeConfig struct {
	KernelABI    string
	Driver, Card string
	Route        Route
	Format       media.Format
	Role         MetadataRole
}

func (c NodeConfig) Validate() error {
	if e := ValidateKernelABI(c.KernelABI); e != nil {
		return e
	}
	if c.Driver == "" || c.Card == "" || len(c.Driver) >= 16 || len(c.Card) >= 32 {
		return fmt.Errorf("exact bounded FIMC driver/card identity required")
	}
	if _, e := c.Route.Selector(); e != nil {
		return e
	}
	if e := ValidateTypedLayout(c.Format, c.Role); e != nil {
		return e
	}
	return nil
}

// SameLayout compares semantic DMA layout, not reserved/padding bytes. A plane
// count alone cannot prove that a later G_FMT still describes the allocated
// memory: geometry, FourCC, stride and every sizeimage must also match.
func SameLayout(a, b media.Format) bool {
	if a.Type != b.Type || a.Width() != b.Width() || a.Height() != b.Height() ||
		a.PixelFormat() != b.PixelFormat() || a.Planes() != b.Planes() || a.Planes() > 3 || a.Planes() < 2 {
		return false
	}
	for i := 0; i < int(a.Planes()); i++ {
		if a.PlaneSize(i) != b.PlaneSize(i) || a.Stride(i) != b.Stride(i) {
			return false
		}
	}
	return true
}
func validateNegotiation(requested, actual media.Format, role MetadataRole) error {
	if e := ValidateTypedLayout(actual, role); e != nil {
		return e
	}
	if actual.Type != requested.Type || actual.Width() != requested.Width() ||
		actual.Height() != requested.Height() || actual.PixelFormat() != requested.PixelFormat() ||
		actual.Planes() != requested.Planes() {
		return fmt.Errorf("FIMC changed requested format; no silent resize or format fallback")
	}
	// Padding can increase, but never truncate the minimum requested extent.
	for i := 0; i < int(actual.Planes()); i++ {
		if actual.PlaneSize(i) < requested.PlaneSize(i) || actual.Stride(i) < requested.Stride(i) {
			return fmt.Errorf("FIMC returned short plane %d", i)
		}
	}
	return nil
}

// ConfigureNode executes QUERYCAP -> S_INPUT -> S_FMT -> G_FMT on an exclusively
// owned fd. It never starts a sensor, allocates memory, queues buffers or closes
// the caller's fd. After an error, the caller MUST discard that node instance;
// a failed vendor ioctl may already have changed routing. No fake rollback ioctl.
func ConfigureNode(fd int, c NodeConfig) (*PreparedNode, media.Format, error) {
	if e := c.Validate(); e != nil {
		return nil, media.Format{}, e
	}
	if e := verifyVideoFD(fd); e != nil {
		return nil, media.Format{}, e
	}
	n, format, err := configureNode(fd, c, func(r uintptr, p unsafe.Pointer) error {
		trace := r != media.QueueBuffer && r != media.DequeueBuffer
		if trace {
			log.Printf("S7 camera %s ioctl=%#x begin", c.Card, r)
		}
		if c.Card == "exynos-fimc-is-i0s" && (r == SetInput || r == media.SFormat) {
			traceCameraKernelTail()
		}
		err := linuxio.Ioctl(fd, r, p)
		if trace || err != nil && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			log.Printf("S7 camera %s ioctl=%#x returned %v", c.Card, r, err)
		}
		return err
	})
	if err == nil {
		n.ready = n.completionReady
	}
	return n, format, err
}

func traceCameraKernelTail() {
	buffer := make([]byte, 4096)
	n, _, errno := syscall.Syscall(syscall.SYS_SYSLOG, 3, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	runtime.KeepAlive(buffer)
	if errno != 0 || n > uintptr(len(buffer)) {
		return
	}
	file, err := os.OpenFile("/dev/pmsg0", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	_, _ = file.Write([]byte("S7 camera kernel tail before ISP ioctl:\n"))
	_, _ = file.Write(buffer[:n])
	_ = file.Close()
}
func verifyVideoFD(fd int) error {
	if fd < 0 {
		return fmt.Errorf("invalid video descriptor")
	}
	if e := media.VerifyABI(); e != nil {
		return e
	}
	var st syscall.Stat_t
	if e := syscall.Fstat(fd, &st); e != nil {
		return e
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFCHR {
		return fmt.Errorf("video descriptor is not a character device")
	}
	flags, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFL, 0)
	if e != 0 {
		return e
	}
	if flags&syscall.O_NONBLOCK == 0 || flags&syscall.O_ACCMODE != syscall.O_RDWR {
		return fmt.Errorf("nonblocking read/write video descriptor required")
	}
	return nil
}
func configureNode(fd int, c NodeConfig, call func(uintptr, unsafe.Pointer) error) (*PreparedNode, media.Format, error) {
	fail := func(e error) (*PreparedNode, media.Format, error) { return nil, media.Format{}, e }
	if e := c.Validate(); e != nil {
		return fail(e)
	}
	if call == nil {
		return fail(fmt.Errorf("missing ioctl backend"))
	}
	if e := queryNodeIdentityABI(c.Driver, c.Card, c.Format.Type, c.KernelABI, call); e != nil {
		return fail(e)
	}
	selector, _ := c.Route.Selector()
	sent := selector
	if e := call(SetInput, unsafe.Pointer(&sent)); e != nil {
		return fail(fmt.Errorf("FIMC S_INPUT: %w", e))
	}
	if sent != selector {
		return fail(fmt.Errorf("FIMC altered input selector"))
	}
	n, format, err := negotiateNodeABI(fd, c.Format, c.Role, c.KernelABI, call)
	if err == nil {
		// This kernel requires the M2M ISP group started before the 3AA leader.
		// VIDEO_IXS_READY_BUFFERS and vb2 min_buffers_needed are both zero.
		// Admit an empty STREAMON only for the verified native ISP route.
		pinned := c.KernelABI == Herolte481ABI && c.Driver == "exynos-fimc-is" &&
			c.Format.Type == media.Output && c.Format.PixelFormat() == 0x32525942 && c.Format.Planes() == 2 &&
			c.Role == ShotMetadata && !c.Route.Reprocessing
		isp := c.Card == "exynos-fimc-is-i0s" && c.Route.UpstreamNode == 112 && c.Route.Connection == 0 && !c.Route.Leader
		raw := c.Card == "exynos-fimc-is-30s" && c.Route.UpstreamNode == 101 && c.Route.Connection == 1 && c.Route.Leader && c.Format.Width() == 32 && c.Format.Height() == 64
		// Both VIDEO_IXS_READY_BUFFERS and VIDEO_3XS_READY_BUFFERS are zero.
		// 3AA must start before first leader QBUF: group_start resets rcount.
		n.emptyLeaderStart = pinned && (isp || raw)
	}
	return n, format, err
}

// VerifyRawLink is mandatory BEFORE starting 3AA -> ISP forwarding. Equal byte
// counts do not mean equal Bayer layout. Metadata roles intentionally differ.
func VerifyRawLink(capture, leader media.Format) error {
	if e := errors.Join(ValidateTypedLayout(capture, StreamMetadata), ValidateTypedLayout(leader, ShotMetadata)); e != nil {
		return e
	}
	if capture.Type != media.Capture || leader.Type != media.Output || capture.Planes() != 2 || leader.Planes() != 2 {
		return fmt.Errorf("RAW link requires one image plane plus separate metadata")
	}
	// Bayer FourCCs actually observed in the supplied HAL's m_initFlitePipe.
	bits := uint64(0)
	switch capture.PixelFormat() {
	case 0x30314742:
		bits = 10 // BG10
	case 0x32314742:
		bits = 12 // BG12
	case 0x32525942:
		bits = 16 // BYR2
	default:
		return fmt.Errorf("unverified FIMC RAW FourCC %q", media.FourCC(capture.PixelFormat()))
	}
	if capture.Width() != leader.Width() || capture.Height() != leader.Height() ||
		capture.PixelFormat() != leader.PixelFormat() || uint64(capture.Stride(0)) < (uint64(capture.Width())*bits+7)/8 ||
		capture.Stride(0) != leader.Stride(0) || capture.PlaneSize(0) != leader.PlaneSize(0) ||
		uint64(capture.Stride(0))*uint64(capture.Height()) > uint64(capture.PlaneSize(0)) {
		return fmt.Errorf("incompatible RAW geometry/FourCC/stride/sizeimage")
	}
	return nil
}

// ConfiguredLayout returns the immutable negotiated snapshot. Legacy borrowed
// nodes lacking a ConfigureNode handshake cannot be used by the graph builder.
func (n *PreparedNode) ConfiguredLayout() (media.Format, MetadataRole, error) {
	if n == nil || n.expected == nil || n.released {
		return media.Format{}, 0, fmt.Errorf("configured live node required")
	}
	return *n.expected, n.role, nil
}

// SharesDescriptor detects aliases before graph construction takes ownership of
// queue imports. Duplicated fd aliases remain forbidden by the caller contract.
func (n *PreparedNode) SharesDescriptor(other *PreparedNode) bool {
	return n != nil && other != nil && (n == other || n.fd == other.fd)
}

// queryNodeIdentity is shared by processing nodes, physical sensors and the
// companion. kind==0 is the control-only companion: it need not advertise a
// video streaming queue. Identity is checked before any mutating ioctl.
func queryNodeIdentity(driver, card string, kind uint32, call func(uintptr, unsafe.Pointer) error) error {
	if driver == "" || card == "" || len(driver) >= 16 || len(card) >= 32 || call == nil {
		return fmt.Errorf("exact bounded node identity required")
	}
	var caps media.Capability
	if e := call(media.QueryCap, unsafe.Pointer(&caps)); e != nil {
		return e
	}
	if linuxio.CString(caps.Driver[:]) != driver || linuxio.CString(caps.Card[:]) != card {
		return fmt.Errorf("FIMC driver/card identity mismatch; no writes issued")
	}
	if kind == 0 {
		return nil
	}
	effective := caps.Capabilities
	if effective&0x80000000 != 0 {
		effective = caps.DeviceCaps
	}
	need := uint32(0x04000000)
	if kind == media.Capture {
		need |= 0x1000
	} else if kind == media.Output {
		need |= 0x2000
	} else {
		return fmt.Errorf("invalid FIMC queue type")
	}
	if effective&need != need {
		return fmt.Errorf("FIMC queue lacks streaming/multiplanar capability")
	}
	return nil
}

// negotiateNode assumes input routing is ALREADY selected. A physical FLITE
// sensor uses a bare module ID, unlike the packed route of processing nodes.
// Sharing this helper must not re-send the processing-node S_INPUT to a sensor.
func negotiateNode(fd int, requested media.Format, role MetadataRole, call func(uintptr, unsafe.Pointer) error) (*PreparedNode, media.Format, error) {
	return negotiateNodeABI(fd, requested, role, "", call)
}

func negotiateNodeABI(fd int, requested media.Format, role MetadataRole, abi string, call func(uintptr, unsafe.Pointer) error) (*PreparedNode, media.Format, error) {
	fail := func(e error) (*PreparedNode, media.Format, error) { return nil, media.Format{}, e }
	if e := ValidateKernelABI(abi); e != nil {
		return fail(e)
	}
	format := requested
	if e := call(media.SFormat, unsafe.Pointer(&format)); e != nil {
		return fail(fmt.Errorf("FIMC S_FMT: %w", e))
	}
	if e := validateNegotiation(requested, format, role); e != nil {
		return fail(e)
	}
	if e := ValidateKernelABI(abi); e != nil {
		return fail(e)
	}
	got := format
	if abi == "" {
		got = media.Format{Type: requested.Type}
		if e := call(media.GFormat, unsafe.Pointer(&got)); e != nil {
			return fail(fmt.Errorf("FIMC G_FMT: %w", e))
		}
	}
	// In the pinned 481 kernel these G_FMT callbacks are no-op stubs. The
	// source-derived layout is a request, NOT a measured readback. QBUF/import
	// extent validation and DQBUF validation still apply without exception.
	if !SameLayout(format, got) {
		return fail(fmt.Errorf("FIMC S_FMT/G_FMT layout mismatch"))
	}
	n := &PreparedNode{fd: fd, kind: got.Type, planes: int(got.Planes()), expected: &got, role: role, call: call, kernelABI: abi}
	return n, got, nil
}

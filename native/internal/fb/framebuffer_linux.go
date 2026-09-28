//go:build linux && (amd64 || arm64)

// Package fb uses the Exynos M2M scaler to write completed monitor frames
// directly into the current S7 fb page. CPU conversion remains host-test only.
package fb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"os"
	"perimode/native/internal/decon"
	"perimode/native/internal/fimg2d"
	"perimode/native/internal/gpumenu"
	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
	"perimode/native/internal/orientation"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type BitField = linuxio.FBBitField
type Variable = linuxio.FBVariable
type Fixed = linuxio.FBFixed
type Buffer struct {
	inputView           atomic.Pointer[inputGeometry]
	capturePending      atomic.Bool
	s7BootVisual        bool
	cpuRasterRejected   bool
	videoGeneration     uint32
	inkTarget           *glassInk
	gpuTarget           *gpuDrawList
	gpuCommandStorage   []gpumenu.Command
	menuGPU             *gpumenu.Renderer
	previewGPU          *gpumenu.Renderer
	previewGPUFactory   func() (*gpumenu.Renderer, error)
	liveStats           LiveGlassStats
	menuStatus          MenuStatus
	menuStatusDirty     bool
	menuForegroundDirty bool
	menuCategoryDirty   bool
	menuHeaderDirty     bool
	menuPreviousStatus  MenuStatus

	feedback                           *feedbackLayer
	preview                            *previewLayer
	previewMu                          sync.Mutex
	previewRevision                    uint64
	menuVideo                          *os.File
	glass                              *glassScene
	menuComposer                       menuFrameComposer
	layerMenuFD                        int
	layerMenuRect                      image.Rectangle
	cameraHUD                          *CameraHUD
	bootOrbit                          bool
	menuComposerRect                   image.Rectangle
	hardwareGlassDisabled              bool
	hardwareGlassUnsafe                bool
	hardwareGlassError                 string
	indicators                         []Indicator
	indicatorsOn                       bool
	indicatorCorner                    int
	indicatorScale                     int
	indicatorBackup                    []savedPixel
	mu                                 sync.Mutex
	file                               *os.File
	scanout                            *decon.Presenter
	Data                               []byte
	V                                  Variable
	F                                  Fixed
	Width, Height                      int
	Rotation                           orientation.Degrees
	closed                             bool
	packed                             []uint32
	row                                []byte
	menuStage                          []byte
	scaler                             *media.RGBScaler
	scalerDMAMode                      bool
	directFrameFD                      int
	directFrameReady                   bool
	lastMonitorFrame                   time.Time
	scalerLast                         media.ScalerStats
	scalerTried                        bool
	scalerError                        string
	redPacked, greenPacked, bluePacked [256]uint32
	packFields                         [4]BitField
	packingReady                       bool
	mapKey                             [5]int
	majorMap, minorMap                 []int
}

type menuFrameComposer interface {
	ComposeFrame(context.Context) (fimg2d.FrameResult, error)
	UpdateOverlay([]uint32) error
	Close() error
}

// Menu composition happens in ordinary RAM. The mapped scanout receives only
// the completed visible image, preventing black-clear and half-painted flicker.
func (b *Buffer) beginMenuStage() []byte {
	mapped := b.Data
	if len(b.menuStage) != len(mapped) {
		b.menuStage = make([]byte, len(mapped))
	}
	b.Data = b.menuStage
	return mapped
}
func (b *Buffer) finishMenuStage(mapped []byte) error {
	return b.finishMenuStageRegion(mapped, image.Rect(0, 0, b.Width, b.Height))
}

func (b *Buffer) finishMenuStageRegion(mapped []byte, logical image.Rectangle) error {
	stage := b.Data
	if len(stage) != len(mapped) || len(mapped) < int(b.F.MemoryLength) {
		return fmt.Errorf("invalid menu staging extent")
	}
	b.Data = mapped
	logical = logical.Intersect(image.Rect(0, 0, b.Width, b.Height))
	if logical.Empty() {
		return nil
	}
	minX, minY, maxX, maxY := int(b.V.X), int(b.V.Y), -1, -1
	for _, p := range [4]image.Point{{logical.Min.X, logical.Min.Y}, {logical.Max.X - 1, logical.Min.Y}, {logical.Min.X, logical.Max.Y - 1}, {logical.Max.X - 1, logical.Max.Y - 1}} {
		x, y := b.Rotation.ToPanel(p.X, p.Y, int(b.V.X), int(b.V.Y))
		minX, minY, maxX, maxY = min(minX, x), min(minY, y), max(maxX, x), max(maxY, y)
	}
	bytespp := int(b.V.Bits / 8)
	rowBytes := (maxX - minX + 1) * bytespp
	for y := minY; y <= maxY; y++ {
		off := (y+int(b.V.YOffset))*int(b.F.LineLength) + (minX+int(b.V.XOffset))*bytespp
		copy(mapped[off:off+rowBytes], stage[off:off+rowBytes])
	}
	return nil
}

func Open(path string) (b *Buffer, err error) {
	if unsafe.Sizeof(Variable{}) != 160 || unsafe.Sizeof(Fixed{}) != 80 {
		return nil, fmt.Errorf("fbdev ABI mismatch")
	}
	f, e := os.OpenFile(path, os.O_RDWR, 0)
	if e != nil {
		return nil, e
	}
	b = &Buffer{file: f}
	owner := b
	defer func() {
		if err != nil {
			_ = owner.Close()
		}
	}()
	if err = linuxio.Ioctl(int(f.Fd()), 0x4600, unsafe.Pointer(&b.V)); err != nil {
		return nil, fmt.Errorf("FBIOGET_VSCREENINFO: %w", err)
	}
	if err = linuxio.Ioctl(int(f.Fd()), 0x4602, unsafe.Pointer(&b.F)); err != nil {
		return nil, fmt.Errorf("FBIOGET_FSCREENINFO: %w", err)
	}
	if path == "/dev/fb0" || path == "/dev/graphics/fb0" {
		kernel, readErr := os.ReadFile("/proc/sys/kernel/osrelease")
		b.s7BootVisual = readErr == nil && strings.TrimSpace(string(kernel)) == "3.18.140-ge41817ea9198"
	}
	if err = b.Validate(); err != nil {
		return nil, fmt.Errorf("%w (variable=%+v fixed=%+v)", err, b.V, b.F)
	}
	b.Width, b.Height = int(b.V.X), int(b.V.Y)
	if b.Height > b.Width {
		b.Rotation = 90
		b.Width, b.Height = b.Height, b.Width
	}
	b.Data, err = syscall.Mmap(int(f.Fd()), 0, int(b.F.MemoryLength), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("fbdev mmap unavailable: %w", err)
	}
	// Do not change DSU/panel timings, clocks or firmware. Only standard unblank.
	if err = linuxio.IoctlValue(int(f.Fd()), 0x4611, 0); err != nil {
		return nil, fmt.Errorf("fbdev unblank: %w", err)
	}
	if b.s7BootVisual {
		b.scanout, err = decon.OpenPresenter(f, b.V, b.F)
		if err != nil {
			return nil, fmt.Errorf("DECON presenter: %w", err)
		}
	}
	b.publishInputGeometry()
	return b, nil
}

// e418's initial fb0 leaves fix.visual zero despite this exact packed XRGB
// bootloader layout. Keep the exception behind a kernel/device gate and retain
// all normal memory/bitfield checks. This changes no kernel metadata or timing.
func s7BootLayout(v Variable, f Fixed) bool {
	return f.Visual == 0 && f.Type == 0 && f.TypeAux == 0 && f.ID == ([16]byte{}) &&
		f.MemoryStart == 0x10000000 && f.MemoryLength == 1440*2560*4*2 && f.LineLength == 1440*4 &&
		v.X == 1440 && v.Y == 2560 && v.XVirtual == 1440 && v.YVirtual == 2560 &&
		v.XOffset == 0 && v.YOffset == 0 && v.Bits == 32 && v.Gray == 0 && v.NonStd == 0 &&
		v.Red == (BitField{Offset: 16, Length: 8}) && v.Green == (BitField{Offset: 8, Length: 8}) &&
		v.Blue == (BitField{Length: 8}) && v.Alpha == (BitField{})
}

func (b *Buffer) Validate() error {
	if b.cpuRasterRejected {
		return fmt.Errorf("CPU pixel drawing rejected on native S7")
	}
	if b.hardwareGlassUnsafe {
		return fimg2d.ErrComposerPoisoned
	}
	v, f := b.V, b.F
	if v.X < 320 || v.Y < 320 || v.X > 4096 || v.Y > 4096 || v.XVirtual < v.X || v.YVirtual < v.Y || v.XVirtual > 8192 || v.YVirtual > 8192 || uint64(v.XOffset)+uint64(v.X) > uint64(v.XVirtual) || uint64(v.YOffset)+uint64(v.Y) > uint64(v.YVirtual) {
		return fmt.Errorf("invalid framebuffer geometry")
	}
	if (f.Visual != 2 && !(b.s7BootVisual && s7BootLayout(v, f))) || f.Type != 0 || v.NonStd != 0 || (v.Bits != 16 && v.Bits != 32) {
		return fmt.Errorf("only packed true-color 16/32-bit framebuffer supported")
	}
	var mask uint64
	for _, c := range []BitField{v.Red, v.Green, v.Blue, v.Alpha} {
		if c.MSBRight != 0 || c.Length > 8 || c.Offset > v.Bits || c.Length > v.Bits-c.Offset {
			return fmt.Errorf("invalid framebuffer component")
		}
		if c.Length > 0 {
			m := ((uint64(1) << c.Length) - 1) << c.Offset
			if m&mask != 0 {
				return fmt.Errorf("overlapping channels")
			}
			mask |= m
		}
	}
	if v.Red.Length == 0 || v.Green.Length == 0 || v.Blue.Length == 0 {
		return fmt.Errorf("missing RGB channels")
	}
	end := (uint64(v.YOffset)+uint64(v.Y)-1)*uint64(f.LineLength) + (uint64(v.XOffset)+uint64(v.X))*uint64(v.Bits/8)
	if f.LineLength < v.XVirtual*(v.Bits/8) || f.MemoryLength > 64<<20 || end > uint64(f.MemoryLength) {
		return fmt.Errorf("framebuffer memory shorter than geometry")
	}
	return nil
}
func (b *Buffer) Close() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	b.publishInputGeometry()
	var e error
	if b.menuComposer != nil {
		e = b.menuComposer.Close()
		if e != nil {
			return e
		}
		b.menuComposer = nil
	}
	if closeErr := b.closeScalerLocked(); closeErr != nil {
		return errors.Join(e, closeErr)
	}
	if b.menuVideo != nil {
		e = errors.Join(e, b.menuVideo.Close())
		b.menuVideo = nil
	}
	if b.scanout != nil {
		if err := b.scanout.Close(); err != nil {
			return errors.Join(e, err)
		}
	}
	if b.Data != nil {
		e = errors.Join(e, syscall.Munmap(b.Data))
	}
	if b.file != nil {
		e = errors.Join(e, b.file.Close())
	}
	return e
}

// Geometry is safe to read while the media worker presents a frame.
func (b *Buffer) Geometry() (int, int, orientation.Degrees) {
	g := b.inputGeometry()
	return g.width, g.height, g.rotation
}
func (b *Buffer) SetRotation(d orientation.Degrees) error {
	if !d.Valid() {
		return fmt.Errorf("invalid panel rotation")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return os.ErrClosed
	}
	if e := b.Validate(); e != nil {
		return e
	}
	if len(b.Data) < int(b.F.MemoryLength) {
		return fmt.Errorf("missing framebuffer mapping")
	}
	if b.hardwareGlassUnsafe {
		return fimg2d.ErrComposerPoisoned
	}
	b.discardFeedback()
	if b.menuComposer != nil {
		if e := b.menuComposer.Close(); e != nil {
			return e
		}
		b.menuComposer = nil
		b.menuComposerRect = image.Rectangle{}
	}
	if e := b.closeScalerLocked(); e != nil {
		return e
	}
	b.scalerTried, b.scalerError = false, ""
	oldRotation, oldWidth, oldHeight := b.Rotation, b.Width, b.Height
	b.Rotation = d
	b.Width, b.Height = d.Size(int(b.V.X), int(b.V.Y))
	b.mapKey = [5]int{}
	b.glass = nil
	b.layerMenuFD = 0
	b.previewRevision++
	if b.preview != nil {
		b.preview.blank = true
		b.preview.backup = nil
		b.preview.docked = false
	}
	b.indicatorBackup = b.indicatorBackup[:0]
	// Clear the old orientation before admitting touch in the new one.
	b.clearVisibleLocked()
	b.drawFeedback()
	if e := b.commit(); e != nil {
		b.discardFeedback()
		b.Rotation = oldRotation
		b.Width, b.Height = oldWidth, oldHeight
		b.mapKey = [5]int{}
		return e
	}
	return nil
}

// MenuLayout is shared by rendering and hit testing, including portrait pages.
func MenuLayout(width, height int, lines []string) (scale, margin, lineHeight int) {
	longest := 1
	for _, s := range lines {
		if len(s) > longest {
			longest = len(s)
		}
	}
	scale = max(1, min(width/(longest*6+8), height/(len(lines)*10+8), max(2, width/480)))
	return scale, 4 * scale, 10 * scale
}
func MenuRow(width, height int, lines []string, x, y uint16) int {
	if width <= 0 || height <= 0 || len(lines) == 0 || x > 32767 || y > 32767 {
		return -1
	}
	scale, margin, lineHeight := MenuLayout(width, height, lines)
	px, py := int(x)*(width-1)/32767, int(y)*(height-1)/32767
	if px < margin || px >= width-margin || py < margin {
		return -1
	}
	row := (py - margin) / lineHeight
	if row >= len(lines) || py >= margin+len(lines)*lineHeight || scale < 1 {
		return -1
	}
	return row
}
func clamp(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}
func RGB(y, u, v byte) (byte, byte, byte) {
	c := int(y) - 16
	d := int(u) - 128
	e := int(v) - 128
	return clamp((298*c + 459*e + 128) >> 8), clamp((298*c - 55*d - 136*e + 128) >> 8), clamp((298*c + 541*d + 128) >> 8)
}
func (b *Buffer) pixelRGB(r, g, bl byte) uint32 {
	v := b.V
	p := uint32(r)>>(8-v.Red.Length)<<v.Red.Offset | uint32(g)>>(8-v.Green.Length)<<v.Green.Offset | uint32(bl)>>(8-v.Blue.Length)<<v.Blue.Offset
	if v.Alpha.Length > 0 {
		p |= ((1 << v.Alpha.Length) - 1) << v.Alpha.Offset
	}
	return p
}
func (b *Buffer) point(x, y int, p uint32) {
	if b.s7BootVisual {
		b.cpuRasterRejected = true
		return
	}
	if x < 0 || y < 0 || x >= b.Width || y >= b.Height {
		return
	}
	x, y = b.Rotation.ToPanel(x, y, int(b.V.X), int(b.V.Y))
	off := (y+int(b.V.YOffset))*int(b.F.LineLength) + (x+int(b.V.XOffset))*int(b.V.Bits/8)
	if b.V.Bits == 32 {
		binary.LittleEndian.PutUint32(b.Data[off:], p)
	} else {
		binary.LittleEndian.PutUint16(b.Data[off:], uint16(p))
	}
}
func (b *Buffer) Fill(r, g, bl byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if b.s7BootVisual {
		if b.scanout == nil {
			b.cpuRasterRejected = true
			return
		}
		color := uint32(0xff000000) | uint32(r)<<16 | uint32(g)<<8 | uint32(bl)
		if err := b.scanout.FillRectangle(b.Data[:int(b.V.Y)*int(b.F.LineLength)], false, fimg2d.Rect{W: int(b.V.X), H: int(b.V.Y)}, color); err != nil {
			b.hardwareGlassError = err.Error()
		}
		return
	}
	p := b.pixelRGB(r, g, bl)
	for y := 0; y < b.Height; y++ {
		for x := 0; x < b.Width; x++ {
			b.point(x, y, p)
		}
	}
}
func Fit(w, h, sw, sh int) (x, y, dw, dh int) {
	dw = w
	dh = w * sh / sw
	if dh > h {
		dh = h
		dw = h * sw / sh
	}
	return (w - dw) / 2, (h - dh) / 2, dw, dh
}

// prepareCPU is the memory-framebuffer test fallback. Real S7 scanout never
// enters this conversion path.
func (b *Buffer) prepareCPU() {
	fields := [4]BitField{b.V.Red, b.V.Green, b.V.Blue, b.V.Alpha}
	if !b.packingReady || fields != b.packFields {
		alpha := uint32(0)
		if b.V.Alpha.Length > 0 {
			alpha = ((1 << b.V.Alpha.Length) - 1) << b.V.Alpha.Offset
		}
		for i := uint32(0); i < 256; i++ {
			b.redPacked[i] = (i >> (8 - b.V.Red.Length) << b.V.Red.Offset) | alpha
			b.greenPacked[i] = i >> (8 - b.V.Green.Length) << b.V.Green.Offset
			b.bluePacked[i] = i >> (8 - b.V.Blue.Length) << b.V.Blue.Offset
		}
		b.packingReady = true
		b.packFields = fields
	}
	rot := int(b.Rotation)
	key := [5]int{int(b.V.X), int(b.V.Y), b.Width, b.Height, rot}
	if key == b.mapKey {
		return
	}
	b.mapKey = key
	b.majorMap = make([]int, b.V.Y)
	b.minorMap = make([]int, b.V.X)
	ox, oy, w, h := Fit(b.Width, b.Height, 1280, 720)
	for py := range b.majorMap {
		q := -1
		if b.Rotation.Swapped() {
			lx := py
			if b.Rotation == 270 {
				lx = int(b.V.Y) - 1 - py
			}
			if lx >= ox && lx < ox+w {
				q = (lx - ox) * 1280 / w
			}
		} else {
			ly := py
			if b.Rotation == 180 {
				ly = int(b.V.Y) - 1 - py
			}
			if ly >= oy && ly < oy+h {
				q = (ly - oy) * 720 / h
			}
		}
		b.majorMap[py] = q
	}
	for px := range b.minorMap {
		q := -1
		if b.Rotation.Swapped() {
			ly := px
			if b.Rotation == 90 {
				ly = int(b.V.X) - 1 - px
			}
			if ly >= oy && ly < oy+h {
				q = 719 - (ly-oy)*720/h
			}
		} else {
			lx := px
			if b.Rotation == 180 {
				lx = int(b.V.X) - 1 - px
			}
			if lx >= ox && lx < ox+w {
				q = (lx - ox) * 1280 / w
			}
		}
		b.minorMap[px] = q
	}
}
func (b *Buffer) packLuma(y byte, rv, gv, bv int) uint32 {
	c := 298 * (int(y) - 16)
	return b.redPacked[clamp((c+rv)>>8)] | b.greenPacked[clamp((c+gv)>>8)] | b.bluePacked[clamp((c+bv)>>8)]
}

// A 16x16 tile keeps both the source and transposed destination cache-local.
// Chroma math is shared by each 2x2 NV12 block. Portrait output is packed in
// physical scanline order, avoiding the old full-frame strided column reads.
func (b *Buffer) convertCPU(im media.Image) {
	for ty := 0; ty < 720; ty += 16 {
		for tx := 0; tx < 1280; tx += 16 {
			for y := ty; y < ty+16; y += 2 {
				y0 := im.Y[y*im.StrideY:]
				y1 := im.Y[(y+1)*im.StrideY:]
				uv := im.UV[(y/2)*im.StrideUV:]
				for x := tx; x < tx+16; x += 2 {
					u, v := int(uv[x])-128, int(uv[x+1])-128
					rv, gv, bv := 459*v+128, -55*u-136*v+128, 541*u+128
					p00 := b.packLuma(y0[x], rv, gv, bv)
					p01 := b.packLuma(y0[x+1], rv, gv, bv)
					p10 := b.packLuma(y1[x], rv, gv, bv)
					p11 := b.packLuma(y1[x+1], rv, gv, bv)
					if b.Rotation.Swapped() {
						i := x*720 + 719 - y
						j := i + 720
						b.packed[i] = p00
						b.packed[i-1] = p10
						b.packed[j] = p01
						b.packed[j-1] = p11
					} else {
						i := y*1280 + x
						j := i + 1280
						b.packed[i] = p00
						b.packed[i+1] = p01
						b.packed[j] = p10
						b.packed[j+1] = p11
					}
				}
			}
		}
	}
}

// Present keeps decoded buffers owned until scaler DQBUF. FBIOPAN_DISPLAY is a
// scanout request, not proof of physical panel presentation or vsync.
func (b *Buffer) Present(im media.Image) error {
	_, err := b.PresentFrame(im)
	return err
}

// PresentFrame reports whether this call committed a new video background.
// Menu-rate coalescing must not be counted as successful monitor presentation.
func (b *Buffer) PresentFrame(im media.Image) (bool, error) {
	return b.presentFrameAt(im, time.Now())
}
func validateSourceImage(im media.Image) error {
	if im.PTS < 0 || !(im.Width == 1280 && im.Height == 720 || im.Width == 2560 && im.Height == 1440 && im.Lease.Live()) || im.StrideY < im.Width || im.StrideUV < im.Width || im.StrideY > 16384 || im.StrideUV > 16384 {
		return fmt.Errorf("invalid source image")
	}
	if len(im.Y) < (im.Height-1)*im.StrideY+im.Width || len(im.UV) < (im.Height/2-1)*im.StrideUV+im.Width {
		return fmt.Errorf("short decoded planes")
	}
	return nil
}
func (b *Buffer) presentFrameAt(im media.Image, now time.Time) (bool, error) {
	if err := validateSourceImage(im); err != nil {
		return false, err
	}
	if b.capturePending.Load() {
		return false, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false, os.ErrClosed
	}
	if err := b.Validate(); err != nil {
		return false, err
	}
	expectedW, expectedH := b.Rotation.Size(int(b.V.X), int(b.V.Y))
	if !b.Rotation.Valid() || b.Width != expectedW || b.Height != expectedH {
		return false, fmt.Errorf("inconsistent logical framebuffer geometry")
	}
	if len(b.Data) < int(b.F.MemoryLength) || b.Width <= 0 || b.Height <= 0 {
		return false, fmt.Errorf("missing framebuffer mapping/geometry")
	}
	if b.hardwareGlassUnsafe {
		return false, fimg2d.ErrComposerPoisoned
	}
	if b.s7BootVisual && b.scanout != nil {
		if b.preview != nil && b.preview.options.Fullscreen {
			return false, nil
		}
		started := time.Now()
		if err := b.commitLayersLocked(&im); err != nil {
			return false, err
		}
		b.lastMonitorFrame = time.Now()
		if b.glass != nil {
			b.liveStats.Offered++
			b.liveStats.Committed++
			b.liveStats.BlurUS, b.liveStats.BlendUS = 0, 0
			b.liveStats.LastComposeUS = time.Since(started).Microseconds()
			b.liveStats.MaxComposeUS = max(b.liveStats.MaxComposeUS, b.liveStats.LastComposeUS)
		}
		return true, nil
	}
	if im.Width != 1280 || im.Height != 720 {
		return false, fmt.Errorf("1440p requires native DMA scanout")
	}
	sc := b.glass
	if sc != nil {
		b.liveStats.Offered++
		if sc.sourceBacked && b.menuComposer != nil {
			return b.presentHardwareGlassLocked(im)
		}
		if sc.sourceBacked {
			sc.storePending(im)
			b.liveStats.Coalesced++
			return false, nil
		}
		if now.Before(sc.nextLive) {
			sc.storePending(im)
			b.liveStats.Coalesced++
			return false, nil
		}
	}
	started := time.Now()
	var mapped []byte
	if sc != nil && sc.sourceBacked {
		mapped = b.beginMenuStage()
		defer func() {
			if mapped != nil {
				b.Data = mapped
			}
		}()
	}
	b.discardFeedback()
	b.indicatorBackup = b.indicatorBackup[:0]
	if b.preview != nil {
		b.preview.backup = b.preview.backup[:0]
	}
	if err := b.writeImageLocked(im); err != nil {
		return false, err
	}
	if sc != nil {
		sc.hasPending = false
		if sc.sourceBacked {
			sc.storeBackdrop(im)
			b.sampleImageBackdrop(sc, im)
		} else {
			b.updateGlassSource(sc)
		}
		b.paintGlassBackdrop(sc, sc.layout)
	}
	b.drawPreview()
	b.drawIndicators()
	if sc != nil {
		b.invalidateFeedbackBackdrop()
	}
	b.drawFeedback()
	if mapped != nil {
		if err := b.finishMenuStage(mapped); err != nil {
			return false, err
		}
		mapped = nil
	}
	err := b.commit()
	if err == nil {
		b.lastMonitorFrame = time.Now()
	}
	if sc != nil {
		cost := time.Since(started)
		sc.nextLive = now.Add(cost + max(liveGlassMinimumGap, cost))
		b.liveStats.LastComposeUS = cost.Microseconds()
		b.liveStats.MaxComposeUS = max(b.liveStats.MaxComposeUS, cost.Microseconds())
		if err == nil {
			b.liveStats.Committed++
		}
	}
	return err == nil, err
}

// The scaler writes the hidden page. G2D is the sole writer of the composed
// visible frame; menu pixels are not blurred or blended on the CPU here.
func (b *Buffer) presentHardwareGlassLocked(im media.Image) (bool, error) {
	started := time.Now()
	b.discardFeedback()
	b.indicatorBackup = b.indicatorBackup[:0]
	if b.preview != nil {
		b.preview.backup = b.preview.backup[:0]
	}
	if err := b.writeImageLocked(im); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	result, err := b.menuComposer.ComposeFrame(ctx)
	cancel()
	if err != nil {
		closeErr := b.menuComposer.Close()
		b.menuComposer = nil
		b.menuComposerRect = image.Rectangle{}
		if errors.Is(closeErr, fimg2d.ErrComposerPoisoned) {
			b.hardwareGlassUnsafe = true
		}
		b.hardwareGlassError = errors.Join(err, closeErr).Error()
		return false, errors.Join(err, closeErr)
	}
	b.liveStats.BlurUS, b.liveStats.BlendUS = result.DownscaleUS, result.CompositeUS
	if b.scanout != nil && b.menuVideo != nil {
		b.directFrameFD, err = b.scanout.BackFD()
		if err != nil {
			return false, err
		}
		b.directFrameReady = true
	}
	b.drawPreview()
	if err := b.commit(); err != nil {
		return false, err
	}
	b.lastMonitorFrame = time.Now()
	b.liveStats.Committed++
	cost := time.Since(started).Microseconds()
	b.liveStats.LastComposeUS = cost
	b.liveStats.MaxComposeUS = max(b.liveStats.MaxComposeUS, cost)
	return true, nil
}

// Called under Buffer.mu, with validated NV12 input. No compositor/commit here.
func (b *Buffer) writeImageLocked(im media.Image) error {
	if handled, err := b.writeHardwareLocked(im); handled {
		return err
	}
	if len(b.packed) != 1280*720 {
		b.packed = make([]uint32, 1280*720)
	}
	b.prepareCPU()
	b.convertCPU(im)
	bytespp := int(b.V.Bits / 8)
	rowlen := int(b.V.X) * bytespp
	if len(b.row) != rowlen {
		b.row = make([]byte, rowlen)
	}
	minorSize := 1280
	if b.Rotation.Swapped() {
		minorSize = 720
	}
	last := -2
	black := b.pixelRGB(0, 0, 0)
	for py, key := range b.majorMap {
		if key != last {
			for px, minor := range b.minorMap {
				v := black
				if key >= 0 && minor >= 0 {
					v = b.packed[key*minorSize+minor]
				}
				if bytespp == 4 {
					binary.LittleEndian.PutUint32(b.row[px*4:], v)
				} else {
					binary.LittleEndian.PutUint16(b.row[px*2:], uint16(v))
				}
			}
			last = key
		}
		off := (py+int(b.V.YOffset))*int(b.F.LineLength) + int(b.V.XOffset)*bytespp
		copy(b.Data[off:off+rowlen], b.row)
	}
	return nil
}

// Commit uses the existing framebuffer geometry. No panel timings/clocks or
// vendor-specific register writes. It requests scanout, not proof of presentation.
func (b *Buffer) commit() error {
	if b.hardwareGlassUnsafe {
		return fimg2d.ErrComposerPoisoned
	}
	if b.cpuRasterRejected {
		return fmt.Errorf("CPU pixel drawing rejected on native S7")
	}
	if b.scanout != nil {
		if b.s7BootVisual && b.scanout.UsesLayers() {
			return b.commitLayersLocked(nil)
		}
		if b.directFrameReady {
			fd := b.directFrameFD
			b.directFrameReady = false
			if err := b.scanout.PresentReady(fd); err != nil {
				return err
			}
			b.publishInputGeometry()
			return nil
		}
		if err := b.scanout.Present(b.Data[:int(b.V.Y)*int(b.F.LineLength)]); err != nil {
			return err
		}
		b.publishInputGeometry()
		return nil
	}
	if b.file == nil {
		b.publishInputGeometry()
		return nil
	} // Memory-only unit-test framebuffer.
	v := b.V
	if e := linuxio.Ioctl(int(b.file.Fd()), 0x4606, unsafe.Pointer(&v)); e != nil {
		return fmt.Errorf("FBIOPAN_DISPLAY: %w", e)
	}
	b.publishInputGeometry()
	return nil
}

// Clear only the visible rows without reading uncached framebuffer memory.
func (b *Buffer) Clear() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return os.ErrClosed
	}
	if err := b.Validate(); err != nil {
		return err
	}
	if len(b.Data) < int(b.F.MemoryLength) {
		return fmt.Errorf("missing framebuffer mapping")
	}
	b.discardFeedback()
	b.clearVisibleLocked()
	return b.commit()
}

func (b *Buffer) clearVisibleLocked() {
	if b.scanout != nil {
		if b.s7BootVisual && b.scanout.UsesLayers() {
			if err := b.scanout.DropVideo(); err != nil {
				b.hardwareGlassUnsafe = true
				b.hardwareGlassError = err.Error()
			}
			return
		}
		if err := b.scanout.ClearWorkspace(b.Data[:int(b.V.Y)*int(b.F.LineLength)]); err != nil {
			b.hardwareGlassUnsafe = true
			b.hardwareGlassError = err.Error()
		}
		return
	}
	bytespp := int(b.V.Bits / 8)
	if len(b.row) != int(b.V.X)*bytespp {
		b.row = make([]byte, int(b.V.X)*bytespp)
	}
	row := b.row
	black := b.pixelRGB(0, 0, 0)
	for x := 0; x < int(b.V.X); x++ {
		if bytespp == 4 {
			binary.LittleEndian.PutUint32(row[x*4:], black)
		} else {
			binary.LittleEndian.PutUint16(row[x*2:], uint16(black))
		}
	}
	for y := 0; y < int(b.V.Y); y++ {
		off := (y+int(b.V.YOffset))*int(b.F.LineLength) + int(b.V.XOffset)*bytespp
		copy(b.Data[off:off+len(row)], row)
	}
}

// Status uses an embedded, hand-written 5x7 diagnostic alphabet, not Android or external font assets.
func (b *Buffer) Status(lines []string) error {
	if b.s7BootVisual {
		_, err := b.GlassMenuSource(lines, 0, nil)
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return os.ErrClosed
	}
	b.discardFeedback()
	scale, _, _ := MenuLayout(b.Width, b.Height, lines)
	height := b.Height
	black := b.pixelRGB(0, 0, 0)
	white := b.pixelRGB(225, 225, 225)
	for y := 0; y < height; y++ {
		for x := 0; x < b.Width; x++ {
			b.point(x, y, black)
		}
	}
	for i, s := range lines {
		b.text(4*scale, (4+i*10)*scale, s, scale, white)
	}
	b.drawFeedback()
	return b.commit()
}
func (b *Buffer) text(x, y int, s string, scale int, color uint32) {
	for _, c := range s {
		g, ok := glyph[c]
		if !ok {
			g = glyph['?']
		}
		for row, bits := range g {
			for col := 0; col < 5; col++ {
				if bits&(1<<(4-col)) != 0 {
					for dy := 0; dy < scale; dy++ {
						for dx := 0; dx < scale; dx++ {
							b.point(x+col*scale+dx, y+row*scale+dy, color)
						}
					}
				}
			}
		}
		x += 6 * scale
		if x+5*scale > b.Width {
			return
		}
	}
}

var glyph = map[rune][7]byte{
	' ': {0, 0, 0, 0, 0, 0, 0}, '?': {14, 17, 1, 2, 4, 0, 4}, '-': {0, 0, 0, 31, 0, 0, 0}, ':': {0, 4, 4, 0, 4, 4, 0}, '.': {0, 0, 0, 0, 0, 4, 4}, '/': {1, 2, 2, 4, 8, 8, 16}, '(': {2, 4, 8, 8, 8, 4, 2}, ')': {8, 4, 2, 2, 2, 4, 8}, '+': {0, 4, 4, 31, 4, 4, 0}, '_': {0, 0, 0, 0, 0, 0, 31}, '%': {17, 2, 4, 4, 8, 16, 17},
	'0': {14, 17, 19, 21, 25, 17, 14}, '1': {4, 12, 4, 4, 4, 4, 14}, '2': {14, 17, 1, 2, 4, 8, 31}, '3': {30, 1, 1, 14, 1, 1, 30}, '4': {2, 6, 10, 18, 31, 2, 2}, '5': {31, 16, 16, 30, 1, 1, 30}, '6': {14, 16, 16, 30, 17, 17, 14}, '7': {31, 1, 2, 4, 8, 8, 8}, '8': {14, 17, 17, 14, 17, 17, 14}, '9': {14, 17, 17, 15, 1, 1, 14},
	'A': {14, 17, 17, 31, 17, 17, 17}, 'B': {30, 17, 17, 30, 17, 17, 30}, 'C': {14, 17, 16, 16, 16, 17, 14}, 'D': {30, 17, 17, 17, 17, 17, 30}, 'E': {31, 16, 16, 30, 16, 16, 31}, 'F': {31, 16, 16, 30, 16, 16, 16}, 'G': {14, 17, 16, 23, 17, 17, 15}, 'H': {17, 17, 17, 31, 17, 17, 17}, 'I': {14, 4, 4, 4, 4, 4, 14}, 'J': {7, 2, 2, 2, 2, 18, 12}, 'K': {17, 18, 20, 24, 20, 18, 17}, 'L': {16, 16, 16, 16, 16, 16, 31}, 'M': {17, 27, 21, 21, 17, 17, 17}, 'N': {17, 25, 21, 19, 17, 17, 17}, 'O': {14, 17, 17, 17, 17, 17, 14}, 'P': {30, 17, 17, 30, 16, 16, 16}, 'Q': {14, 17, 17, 17, 21, 18, 13}, 'R': {30, 17, 17, 30, 20, 18, 17}, 'S': {15, 16, 16, 14, 1, 1, 30}, 'T': {31, 4, 4, 4, 4, 4, 4}, 'U': {17, 17, 17, 17, 17, 17, 14}, 'V': {17, 17, 17, 17, 17, 10, 4}, 'W': {17, 17, 17, 21, 21, 21, 10}, 'X': {17, 17, 10, 4, 10, 17, 17}, 'Y': {17, 17, 10, 4, 4, 4, 4}, 'Z': {31, 1, 2, 4, 8, 16, 31},
}

// Badge is a small native status overlay. It is not the former Android glass UI.
// corner: 0 TL, 1 TR, 2 BL, 3 BR. All positions are clamped to the framebuffer.
func (b *Buffer) Badge(text string, corner int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return os.ErrClosed
	}
	b.restoreFeedback()
	scale := max(2, b.Width/640)
	maxchars := max(1, (b.Width-8*scale)/(6*scale))
	if len(text) > maxchars {
		text = text[:maxchars]
	}
	w, h := min(b.Width, (len(text)*6+8)*scale), min(b.Height, 11*scale)
	x, y := 0, 0
	if corner&1 != 0 {
		x = b.Width - w
	}
	if corner&2 != 0 {
		y = b.Height - h
	}
	black, gray := b.pixelRGB(0, 0, 0), b.pixelRGB(190, 190, 190)
	for yy := 0; yy < h; yy++ {
		for xx := 0; xx < w; xx++ {
			b.point(x+xx, y+yy, black)
		}
	}
	b.text(x+4*scale, y+2*scale, text, scale, gray)
	b.drawFeedback()
	return b.commit()
}

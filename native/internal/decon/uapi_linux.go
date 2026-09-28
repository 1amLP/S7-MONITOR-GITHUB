//go:build linux && (amd64 || arm64)

package decon

import (
	"fmt"
	"unsafe"
)

// Pinned e418 Exynos8890 decon.h, LP64 with CONFIG_FB_DSU=y.
const (
	winConfigIOCTL uintptr = 0x463846d1
	winStateBuffer int32   = 2
	idmaG1         int32   = 1
	formatBGRA8888 int32   = 3
	panelWidth             = 1440
	panelHeight            = 2560
	panelBytes             = panelWidth * panelHeight * 4
)

type frame struct {
	X, Y         int32
	W, H, FW, FH uint32
}

type rect struct {
	X, Y int32
	W, H uint32
}

type vppParams struct {
	Addr [3]uint64
	Rot  int32
	CSC  int32
}

type winBuffer struct {
	FD                            [3]int32
	FenceFD, PlaneAlpha, Blending int32
	IDMA, Format                  int32
	VPP                           vppParams
	Block, Transparent, Opaque    rect
	Src                           frame
}

type winConfig struct {
	State       int32
	_           [4]byte
	Buffer      winBuffer
	Dst         frame
	Protection  uint8
	Compression uint8
	_           [6]byte
}

type winConfigData struct {
	Fence, FDODMA int32
	Config        [9]winConfig
}

func verifyABI() error {
	var c winConfig
	var b winBuffer
	var d winConfigData
	if unsafe.Sizeof(frame{}) != 24 || unsafe.Sizeof(rect{}) != 16 || unsafe.Sizeof(vppParams{}) != 32 ||
		unsafe.Sizeof(b) != 136 || unsafe.Offsetof(b.VPP) != 32 || unsafe.Offsetof(b.Src) != 112 ||
		unsafe.Sizeof(c) != 176 || unsafe.Offsetof(c.Buffer) != 8 || unsafe.Offsetof(c.Dst) != 144 ||
		unsafe.Sizeof(d) != 1592 || unsafe.Offsetof(d.Config) != 8 {
		return fmt.Errorf("e418 DECON WIN_CONFIG LP64 ABI mismatch")
	}
	return nil
}

func fullFrame(fd int) (winConfigData, error) {
	if fd < 0 || uint64(fd) > 0x7fffffff {
		return winConfigData{}, fmt.Errorf("invalid DECON DMA-BUF fd")
	}
	if err := verifyABI(); err != nil {
		return winConfigData{}, err
	}
	d := winConfigData{Fence: -1, FDODMA: -1}
	w := &d.Config[0]
	w.State = winStateBuffer
	w.Buffer.FD = [3]int32{int32(fd), -1, -1}
	w.Buffer.FenceFD = -1
	w.Buffer.PlaneAlpha = 255
	w.Buffer.Blending = 0
	w.Buffer.IDMA = idmaG1
	// Mali and G2D write words 0xAARRGGBB (bytes B,G,R,A). e418 reverses
	// WIN_CONFIG names: BGRA -> VPP ARGB. RGBA selected ABGR and swapped R/B.
	w.Buffer.Format = formatBGRA8888
	w.Buffer.Src = frame{W: panelWidth, H: panelHeight, FW: panelWidth, FH: panelHeight}
	w.Dst = w.Buffer.Src
	return d, nil
}

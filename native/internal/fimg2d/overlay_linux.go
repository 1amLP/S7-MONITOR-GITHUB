//go:build linux && (amd64 || arm64)

package fimg2d

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
)

type staticOverlay struct {
	dma    *os.File
	pixels []byte
	length int
	width  int
	height int
}

func newDMAOverlay(fd int) (*staticOverlay, error) {
	if fd < 0 {
		return nil, fmt.Errorf("invalid GPU overlay fd")
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Size > 0 && st.Size < frameBytes {
		return nil, fmt.Errorf("GPU overlay DMA-BUF is too short")
	}
	copyFD, err := syscall.Dup(fd)
	if err != nil {
		return nil, err
	}
	return &staticOverlay{dma: os.NewFile(uintptr(copyFD), "s7-menu-overlay"), length: frameBytes, width: panelWidth, height: panelHeight}, nil
}

func (o *staticOverlay) source(r Rect) image {
	if o.dma == nil {
		return userImage(o.pixels, r.W, r.H, Rect{W: r.W, H: r.H}, r, premultAlpha, blendSourceOver, 0)
	}
	im := image{Flags: premultAlpha, Fence: -1, Memory: bufferDMA, NumPlanes: 1,
		Format: format{Width: panelWidth, Height: panelHeight, PixelFormat: formatARGB32,
			Crop:   rect{Left: int32(r.X), Top: int32(r.Y), Width: uint32(r.W), Height: uint32(r.H)},
			Window: rect{Left: int32(r.X), Top: int32(r.Y), Width: uint32(r.W), Height: uint32(r.H)}},
		Extra: extra{Composite: blendSourceOver, Alpha: 255}}
	im.Plane[0] = buffer{Memory: o.dma.Fd(), Length: frameBytes}
	return im
}

// argb contains packed 0xAARRGGBB premultiplied pixels in physical order.
// This CPU copy happens only when the menu changes, never in ComposeFrame.
func newStaticOverlay(r Rect, argb []uint32) (*staticOverlay, error) {
	if !r.validPhysical() || len(argb) != r.W*r.H {
		return nil, fmt.Errorf("invalid physical overlay geometry")
	}
	for _, p := range argb {
		a := (p >> 24) & 0xff
		if (p>>16)&0xff > a || (p>>8)&0xff > a || p&0xff > a {
			return nil, fmt.Errorf("overlay is not premultiplied ARGB")
		}
	}
	length := len(argb) * 4
	capacity := (length + 4095) &^ 4095
	pixels, err := syscall.Mmap(-1, 0, capacity, syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return nil, fmt.Errorf("map static menu overlay: %w", err)
	}
	for i, p := range argb {
		binary.LittleEndian.PutUint32(pixels[i*4:], p)
	}
	clear(pixels[length:])
	return &staticOverlay{pixels: pixels, length: length, width: r.W, height: r.H}, nil
}

func (o *staticOverlay) close() error {
	if o == nil {
		return nil
	}
	if o.dma != nil {
		err := o.dma.Close()
		o.dma = nil
		return err
	}
	if o.pixels == nil {
		return nil
	}
	err := syscall.Munmap(o.pixels)
	if err == nil {
		o.pixels = nil
	}
	return err
}

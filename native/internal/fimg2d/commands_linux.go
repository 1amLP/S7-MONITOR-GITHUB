//go:build linux && (amd64 || arm64)

package fimg2d

import (
	"fmt"
	"unsafe"
)

const (
	premultAlpha    uint32 = 1 << 4
	blendSourceOver uint16 = 4
	bilinearFilter  uint8  = 3
)

func dmaImage(fd, width, height, length int, crop, window Rect, flags uint32, blend uint16, filter uint8) image {
	im := image{Flags: flags, Fence: -1, Memory: bufferDMA, NumPlanes: 1,
		Format: format{Width: uint32(width), Height: uint32(height), PixelFormat: formatARGB32,
			Crop:   rect{Left: int32(crop.X), Top: int32(crop.Y), Width: uint32(crop.W), Height: uint32(crop.H)},
			Window: rect{Left: int32(window.X), Top: int32(window.Y), Width: uint32(window.W), Height: uint32(window.H)}},
		Extra: extra{Composite: blend, Alpha: 255, ScalerFilter: filter}}
	im.Plane[0] = buffer{Memory: uintptr(fd), Length: uint32(length)}
	return im
}

func downscaleDMACommand(mapped []byte, fd int, r Rect, scratch scratchLayout) (*[g2dMaxSources]image, *task, error) {
	if len(mapped) != 2*frameBytes || fd < 0 || scratch.payload <= 0 || scratch.payload > 1<<20 {
		return nil, nil, fmt.Errorf("invalid Mali blur input")
	}
	sources := new([g2dMaxSources]image)
	small := Rect{W: scratch.width, H: scratch.height}
	sources[0] = userImage(mapped[frameBytes:], panelWidth, panelHeight, r, small, 0, blendSource, bilinearFilter)
	target := dmaImage(fd, scratch.width, scratch.height, 1<<20, small, small, 0, 0, 0)
	return sources, &task{Sources: uintptr(unsafe.Pointer(&sources[0])), Target: target, NumSources: 1}, nil
}

func compositeDMACommand(mapped []byte, fd int, overlay *staticOverlay, r Rect, scratch scratchLayout) (*[g2dMaxSources]image, *task, error) {
	if len(mapped) != 2*frameBytes || fd < 0 || overlay == nil || overlay.dma == nil || scratch.payload <= 0 || scratch.payload > 1<<20 {
		return nil, nil, fmt.Errorf("invalid Mali composition buffers")
	}
	sources := new([g2dMaxSources]image)
	full := Rect{W: panelWidth, H: panelHeight}
	sources[0] = userImage(mapped[frameBytes:], panelWidth, panelHeight, full, full, 0, blendSource, 0)
	sources[1] = dmaImage(fd, scratch.width, scratch.height, 1<<20, Rect{W: scratch.width, H: scratch.height}, r, 0, blendSource, bilinearFilter)
	sources[2] = overlay.source(r)
	target := userImage(mapped[:frameBytes], panelWidth, panelHeight, full, full, 0, 0, 0)
	return sources, &task{Sources: uintptr(unsafe.Pointer(&sources[0])), Target: target, NumSources: 3}, nil
}

func userImage(data []byte, width, height int, crop, window Rect, flags uint32, blend uint16, filter uint8) image {
	im := image{Flags: flags, Fence: -1, Memory: bufferUser, NumPlanes: 1,
		Format: format{Width: uint32(width), Height: uint32(height), PixelFormat: formatARGB32,
			Crop:   rect{Left: int32(crop.X), Top: int32(crop.Y), Width: uint32(crop.W), Height: uint32(crop.H)},
			Window: rect{Left: int32(window.X), Top: int32(window.Y), Width: uint32(window.W), Height: uint32(window.H)}},
		Extra: extra{Composite: blend, Alpha: 255, ScalerFilter: filter}}
	im.Plane[0] = buffer{Memory: uintptrOf(data), Length: uint32(len(data))}
	return im
}

func checkCommandRanges(visible, scratch, overlay []byte) error {
	v, err := rangeOf(visible)
	if err != nil {
		return err
	}
	s, err := rangeOf(scratch)
	if err != nil {
		return err
	}
	if v.overlaps(s) {
		return fmt.Errorf("G2D visible source aliases blur scratch")
	}
	if len(overlay) != 0 {
		o, err := rangeOf(overlay)
		if err != nil {
			return err
		}
		if v.overlaps(o) || s.overlaps(o) {
			return fmt.Errorf("G2D overlay aliases visible or scratch memory")
		}
	}
	return nil
}

func checkFrameRanges(visible, raw, scratch, overlay []byte) error {
	regions := [4][]byte{visible, raw, scratch, overlay}
	var ranges [4]byteRange
	for i, region := range regions {
		current, err := rangeOf(region)
		if err != nil {
			return err
		}
		for j := 0; j < i; j++ {
			if current.overlaps(ranges[j]) {
				return fmt.Errorf("G2D frame regions %d and %d overlap", j, i)
			}
		}
		ranges[i] = current
	}
	return nil
}

func downscaleCommand(mapped, scratchMem []byte, r Rect, scratch scratchLayout) (*[g2dMaxSources]image, *task, error) {
	if len(mapped) != 2*frameBytes || len(scratchMem) != scratch.mapped ||
		scratch.mapped <= 0 || scratch.mapped > frameBytes || scratch.width <= 0 || scratch.height <= 0 {
		return nil, nil, fmt.Errorf("invalid G2D downscale mapping")
	}
	visible := mapped[:frameBytes]
	raw := mapped[frameBytes:]
	if err := checkCommandRanges(raw, scratchMem, visible); err != nil {
		return nil, nil, err
	}
	sources := new([g2dMaxSources]image)
	sources[0] = userImage(raw, panelWidth, panelHeight, r,
		Rect{W: scratch.width, H: scratch.height}, 0, blendSource, bilinearFilter)
	dstRect := Rect{W: scratch.width, H: scratch.height}
	dst := userImage(scratchMem, scratch.width, scratch.height, dstRect, dstRect, 0, 0, 0)
	cmd := &task{Sources: uintptr(unsafe.Pointer(&sources[0])), Target: dst, NumSources: 1}
	return sources, cmd, nil
}

func compositeCommand(mapped, scratchMem []byte, overlay *staticOverlay, r Rect, scratch scratchLayout) (*[g2dMaxSources]image, *task, error) {
	if len(mapped) != 2*frameBytes || overlay == nil ||
		len(scratchMem) != scratch.mapped || scratch.mapped <= 0 || scratch.mapped > frameBytes ||
		scratch.width <= 0 || scratch.height <= 0 {
		return nil, nil, fmt.Errorf("invalid G2D composite mapping")
	}
	visible := mapped[:frameBytes]
	raw := mapped[frameBytes:]
	if overlay.dma == nil && (overlay.pixels == nil || overlay.width != r.W || overlay.height != r.H || overlay.length != r.W*r.H*4) {
		return nil, nil, fmt.Errorf("invalid software overlay extent")
	}
	if overlay.dma != nil && (overlay.width != panelWidth || overlay.height != panelHeight || overlay.length != frameBytes) {
		return nil, nil, fmt.Errorf("invalid DMA overlay extent")
	}
	if err := checkCommandRanges(visible, raw, scratchMem); err != nil {
		return nil, nil, err
	}
	if overlay.dma == nil {
		if err := checkFrameRanges(visible, raw, scratchMem, overlay.pixels); err != nil {
			return nil, nil, err
		}
	}
	sources := new([g2dMaxSources]image)
	full := Rect{W: panelWidth, H: panelHeight}
	sources[0] = userImage(raw, panelWidth, panelHeight, full, full, 0, blendSource, 0)
	sources[1] = userImage(scratchMem, scratch.width, scratch.height,
		Rect{W: scratch.width, H: scratch.height}, r, 0, blendSource, bilinearFilter)
	sources[2] = overlay.source(r)
	dst := userImage(visible, panelWidth, panelHeight, full, full, 0, 0, 0)
	cmd := &task{Sources: uintptr(unsafe.Pointer(&sources[0])), Target: dst, NumSources: g2dMaxSources}
	return sources, cmd, nil
}

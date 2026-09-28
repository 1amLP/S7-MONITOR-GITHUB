//go:build linux && (amd64 || arm64)

package fimg2d

import (
	"fmt"
	"unsafe"

	"perimode/native/internal/orientation"
)

// Rect uses physical panel pixels. The overlay passed to the composer must
// already be rotated into this rectangle's physical orientation.
type Rect struct{ X, Y, W, H int }

func (r Rect) validPhysical() bool {
	return r.X >= 0 && r.Y >= 0 && r.W >= 64 && r.H >= 64 &&
		r.X < panelWidth && r.Y < panelHeight &&
		r.W <= panelWidth-r.X && r.H <= panelHeight-r.Y
}

func PhysicalRect(logical Rect, rotation orientation.Degrees) (Rect, error) {
	if !rotation.Valid() {
		return Rect{}, fmt.Errorf("invalid panel rotation")
	}
	lw, lh := rotation.Size(panelWidth, panelHeight)
	if logical.X < 0 || logical.Y < 0 || logical.W < 64 || logical.H < 64 ||
		logical.X >= lw || logical.Y >= lh || logical.W > lw-logical.X || logical.H > lh-logical.Y {
		return Rect{}, fmt.Errorf("logical menu rect outside panel")
	}
	r := logical
	switch rotation {
	case 90:
		r = Rect{panelWidth - logical.Y - logical.H, logical.X, logical.H, logical.W}
	case 180:
		r = Rect{panelWidth - logical.X - logical.W, panelHeight - logical.Y - logical.H, logical.W, logical.H}
	case 270:
		r = Rect{logical.Y, panelHeight - logical.X - logical.W, logical.H, logical.W}
	}
	if !r.validPhysical() {
		return Rect{}, fmt.Errorf("rotated menu rect outside physical panel")
	}
	return r, nil
}

type scratchLayout struct {
	width, height int
	payload       int
	mapped        int
}

func layoutFor(r Rect) (scratchLayout, error) {
	if !r.validPhysical() {
		return scratchLayout{}, fmt.Errorf("invalid physical menu rect")
	}
	w, h := (r.W+7)/8, (r.H+7)/8
	bytes := w * h * 4
	mapped := (bytes + 4095) &^ 4095
	if mapped <= 0 || mapped > frameBytes {
		return scratchLayout{}, fmt.Errorf("blur scratch exceeds invisible fb0 half")
	}
	return scratchLayout{width: w, height: h, payload: bytes, mapped: mapped}, nil
}

type byteRange struct{ start, end uintptr }

func uintptrOf(data []byte) uintptr { return uintptr(unsafe.Pointer(&data[0])) }

func rangeOf(data []byte) (byteRange, error) {
	if len(data) == 0 {
		return byteRange{}, fmt.Errorf("empty G2D memory range")
	}
	start := uintptrOf(data)
	if uintptr(len(data)) > ^uintptr(0)-start {
		return byteRange{}, fmt.Errorf("G2D address overflow")
	}
	return byteRange{start, start + uintptr(len(data))}, nil
}

func (a byteRange) overlaps(b byteRange) bool { return a.start < b.end && b.start < a.end }

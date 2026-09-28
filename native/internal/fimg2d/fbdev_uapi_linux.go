//go:build linux && (amd64 || arm64)

package fimg2d

import (
	"fmt"
	"unsafe"
)

// The pinned LP64 fbdev ioctl shapes. Kept local so fb can import fimg2d.
type bitField struct{ Offset, Length, MSBRight uint32 }

type variable struct {
	X, Y, XVirtual, YVirtual, XOffset, YOffset, Bits, Gray                                                                          uint32
	Red, Green, Blue, Alpha                                                                                                         bitField
	NonStd, Activate, HeightMM, WidthMM, Accel, PixelClock, Left, Right, Upper, Lower, HSync, VSync, Sync, Mode, Rotate, ColorSpace uint32
	Reserved                                                                                                                        [4]uint32
}

type fixed struct {
	ID                                  [16]byte
	MemoryStart                         uint64
	MemoryLength, Type, TypeAux, Visual uint32
	XPanStep, YPanStep, YWrapStep       uint16
	Pad                                 uint16
	LineLength                          uint32
	Pad2                                uint32
	MMIOStart                           uint64
	MMIOLength, Accel                   uint32
	Capabilities                        uint16
	Reserved                            [2]uint16
	Pad3                                uint16
}

func verifyFBABI() error {
	var v variable
	var f fixed
	if unsafe.Sizeof(v) != 160 || unsafe.Sizeof(f) != 80 ||
		unsafe.Offsetof(v.Red) != 32 || unsafe.Offsetof(v.NonStd) != 80 ||
		unsafe.Offsetof(f.MemoryStart) != 16 || unsafe.Offsetof(f.MemoryLength) != 24 ||
		unsafe.Offsetof(f.LineLength) != 48 || unsafe.Offsetof(f.MMIOStart) != 56 {
		return fmt.Errorf("pinned S7 fbdev LP64 ABI mismatch")
	}
	return nil
}

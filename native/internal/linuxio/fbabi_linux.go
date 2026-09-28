//go:build linux && (amd64 || arm64)

package linuxio

// Linux LP64 fbdev UAPI, shared by the legacy mapping and DECON owner.
type FBBitField struct{ Offset, Length, MSBRight uint32 }
type FBVariable struct {
	X, Y, XVirtual, YVirtual, XOffset, YOffset, Bits, Gray                                                                          uint32
	Red, Green, Blue, Alpha                                                                                                         FBBitField
	NonStd, Activate, HeightMM, WidthMM, Accel, PixelClock, Left, Right, Upper, Lower, HSync, VSync, Sync, Mode, Rotate, ColorSpace uint32
	Reserved                                                                                                                        [4]uint32
}
type FBFixed struct {
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

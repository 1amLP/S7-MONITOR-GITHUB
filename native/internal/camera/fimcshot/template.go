package fimcshot

// The initializer below is a translation of the pinned vendor's initShotData,
// not a fabricated zero-filled request. It requires the sensor-dependent inputs
// explicitly. It initializes METADATA ONLY: firmware, calibration, sensor mode,
// node routing, buffer allocation and STREAMON are separate responsibilities.
import (
	"fmt"
	"math"
)

// Profile values must come from the selected sensor's verified static data.
// No global S7 profile is inferred: different modules and high-speed modes have
// different crops, optics and timings. Synthetic profiles are for tests only.
type Profile struct {
	// setMetaSetfile writes the merged setfile/YUV range word at offset zero.
	// Values come from the selected mode, never from an output-size heuristic.
	Setfile, YUVRange uint32

	Crop                                    Rect
	FPSMin, FPSMax                          uint32
	Aperture, FocalLength, CompensationStep float32
	ThumbnailWidth, ThumbnailHeight         uint32
}

func finite(x float32) bool { return !math.IsNaN(float64(x)) && !math.IsInf(float64(x), 0) }
func (p Profile) Validate() error {
	if p.Setfile > 0xffff || p.YUVRange > 0xffff {
		return fmt.Errorf("setfile and YUV range exceed vendor 16-bit fields")
	}

	if !p.Crop.valid() || p.Crop.X%2 != 0 || p.Crop.Y%2 != 0 || p.Crop.Width%2 != 0 || p.Crop.Height%2 != 0 {
		return fmt.Errorf("invalid aligned sensor crop")
	}
	if p.FPSMin == 0 || p.FPSMin > p.FPSMax || p.FPSMax > 240 {
		return fmt.Errorf("invalid sensor FPS range")
	}
	if !finite(p.Aperture) || !finite(p.FocalLength) || !finite(p.CompensationStep) || p.Aperture <= 0 || p.Aperture > 64 || p.FocalLength <= 0 || p.FocalLength > 1000 || p.CompensationStep <= 0 || p.CompensationStep > 16 {
		return fmt.Errorf("verified positive optical and exposure-step inputs required")
	}
	if p.ThumbnailWidth > 4096 || p.ThumbnailHeight > 4096 || (p.ThumbnailWidth == 0) != (p.ThumbnailHeight == 0) {
		return fmt.Errorf("invalid thumbnail geometry")
	}
	return nil
}

// Template is immutable to users. Its array does not alias a mapped DMA plane.
type Template struct {
	data        [Size]byte
	profile     Profile
	initialized bool
}

func NewTemplate(p Profile) (Template, error) {
	if e := p.Validate(); e != nil {
		return Template{}, e
	}
	t := Template{profile: p, initialized: true}
	b := t.data[:]
	word := func(off int, x uint32) { le.PutUint32(b[off:], x) }
	real := func(off int, x float32) { word(off, math.Float32bits(x)) }
	word(0, p.Setfile|p.YUVRange<<16)
	// All unspecified bytes remain zero, matching memset(..., 0, 0x7df0).
	for off, x := range map[int]uint32{
		0x39c: 1, 0x2b4: 1, 0x310: 1, 0x38c: 1, 0x1a0: 2, 0x700: 2, 0x2f8: 1,
		0x3e8: 1, 0x3ec: 1, 0x3f0: 1, 0x27c: 1, 0x280: 2, 0x284: 2,
		0x21c: 100, 0x230: 1000, 0x218: 1, 0x25c: 1, 0x260: 2, 0x210: 4,
		0x298: 1, 0x254: 1000, 0x240: 1, 0x29c: 1, 0x4734: 1, 0x4738: 1,
		0x473c: 2, 0x4740: 2, 0x4750: 1, 0xf4: 1, 0xf8: 1, 0xfc: 1, 0x100: 1,
		MagicOffset: Magic,
	} {
		word(off, x)
	}
	real(0x378, p.Aperture)
	real(0x380, p.FocalLength)
	real(0x384, -1)
	real(0x28c, p.CompensationStep)
	b[0x390] = 5
	b[0x2fc] = 5
	b[0x36c] = 0x60
	b[0x36d] = 100
	word(0x370, p.ThumbnailWidth)
	word(0x374, p.ThumbnailHeight)
	for i, x := range []uint32{p.Crop.X, p.Crop.Y, p.Crop.Width, p.Crop.Height} {
		word(0x3a4+i*4, x)
	}
	// 3x3 rational identity, one numerator/denominator pair per cell.
	for i := 0; i < 9; i++ {
		if i%4 == 0 {
			word(0x1a4+i*8, 1)
		}
		word(0x1a8+i*8, 1)
	}
	// Pinned initializer repeats (0,0,1,1) sixteen times per tone curve.
	for _, base := range []int{0x3fc, 0x4fc, 0x5fc} {
		for i := 0; i < 16; i++ {
			real(base+i*16+8, 1)
			real(base+i*16+12, 1)
		}
	}
	v, _ := Bind(b)
	_ = v.SetFrameRange(p.FPSMin, p.FPSMax)
	return t, nil
}

// Reset prepares a caller-owned metadata plane. It never operates on a queued
// plane and does not decide DMA ownership; the queue owner must enforce that.
func (t *Template) Reset(dst []byte) (View, error) {
	if t == nil || !t.initialized {
		return View{}, fmt.Errorf("uninitialized shot template")
	}
	if len(dst) != Size {
		return View{}, fmt.Errorf("metadata plane extent mismatch")
	}
	copy(dst, t.data[:])
	return Bind(dst)
}
func (t *Template) Profile() (Profile, error) {
	if t == nil || !t.initialized {
		return Profile{}, fmt.Errorf("uninitialized shot template")
	}
	return t.profile, nil
}

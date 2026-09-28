package fimcshot

import (
	"fmt"
	"math"
)

// Values are the pinned camera2_shot ABI, not UI or V4L2 enum numbers.
type FocusMode uint32

const (
	FocusOff               FocusMode = 1
	FocusSingle            FocusMode = 2
	FocusMacro             FocusMode = 3
	FocusContinuousVideo   FocusMode = 4
	FocusContinuousPicture FocusMode = 5
)

type FocusTrigger uint32

const (
	FocusIdle FocusTrigger = iota
	FocusStart
	FocusCancel
)

type WhiteBalance uint32

const (
	WBAuto            WhiteBalance = 2
	WBIncandescent    WhiteBalance = 3
	WBFluorescent     WhiteBalance = 4
	WBWarmFluorescent WhiteBalance = 5
	WBDaylight        WhiteBalance = 6
	WBCloudy          WhiteBalance = 7
	WBTwilight        WhiteBalance = 8
	WBShade           WhiteBalance = 9
	WBCustomK         WhiteBalance = 10
)

// Region is an absolute sensor rectangle. A zero Region delegates selection to
// ISP. Nonzero regions must fit the ACTIVE sensor crop, not the display surface.
type Region struct {
	Rect   Rect
	Weight uint32
}

func (r Region) valid(crop Rect) bool {
	if r == (Region{}) {
		return true
	}
	return r.Weight >= 1 && r.Weight <= 1000 && r.Rect.valid() && r.Rect.X >= crop.X && r.Rect.Y >= crop.Y &&
		uint64(r.Rect.X)+uint64(r.Rect.Width) <= uint64(crop.X)+uint64(crop.Width) &&
		uint64(r.Rect.Y)+uint64(r.Rect.Height) <= uint64(crop.Y)+uint64(crop.Height)
}

// Limits must be obtained for the actual sensor/mode before enabling controls.
// No minimum focus distance or ISO range is assumed from the phone's model.
type Limits struct {
	ISPImageControls                                        bool
	Crop                                                    Rect
	FrameDurationNS                                         uint64
	ExposureMinNS, ExposureMaxNS                            uint64
	ISOMin, ISOMax                                          uint32
	CompensationMin, CompensationMax                        int32
	FocusModes                                              uint32 // bit (1 << FocusMode)
	WBModeMask                                              uint32 // bit (1 << WhiteBalance)
	MaxFocusDioptres                                        float32
	ManualExposure, ManualISO, ManualFocus, AELock, AWBLock bool
}

func (l Limits) Validate() error {
	if !l.Crop.valid() || l.FrameDurationNS == 0 || l.FrameDurationNS > 1_000_000_000 ||
		l.CompensationMin > l.CompensationMax || l.CompensationMin > 0 || l.CompensationMax < 0 ||
		l.FocusModes == 0 || l.FocusModes & ^uint32(0x3e) != 0 || l.WBModeMask&(1<<WBAuto) == 0 || l.WBModeMask & ^uint32(0x7fc) != 0 ||
		!finite(l.MaxFocusDioptres) || l.MaxFocusDioptres < 0 || l.MaxFocusDioptres > 1000 {
		return fmt.Errorf("incomplete sensor control limits")
	}
	if l.ManualExposure && (l.ExposureMinNS == 0 || l.ExposureMinNS > l.ExposureMaxNS || l.ExposureMaxNS > 1_000_000_000) {
		return fmt.Errorf("invalid exposure limits")
	}
	if l.ManualISO && (l.ISOMin == 0 || l.ISOMin > l.ISOMax || l.ISOMax > 1_000_000) {
		return fmt.Errorf("invalid ISO limits")
	}
	if l.ManualFocus && (l.MaxFocusDioptres <= 0 || l.FocusModes&(1<<FocusOff) == 0) {
		return fmt.Errorf("invalid manual focus limits")
	}
	return nil
}

type Controls struct {
	Brightness                 int32
	Contrast, Gamma, Sharpness uint32
	WBTemperature              uint32
	// Zero preserves legacy automatic anti-banding; 1..4 are the native enum.
	AntiBand uint32
	// Zero exposure selects AE. ManualISO is only supported with manual exposure,
	// matching the sensor metadata translator, not the separate legacy API.
	ExposureNS   uint64
	ISO          uint32
	Compensation int32
	AELocked     bool
	AERegion     Region
	Focus        FocusMode
	Trigger      FocusTrigger
	// -1 leaves the lens under AF control; nonnegative values request manual focus.
	FocusDioptres float32
	AFRegion      Region
	WhiteBalance  WhiteBalance
	AWBLocked     bool
}

func AutoControls() Controls {
	return Controls{Focus: FocusOff, FocusDioptres: -1, WhiteBalance: WBAuto}
}
func (c Controls) Validate(l Limits) error {
	if e := l.Validate(); e != nil {
		return e
	}
	if c.AntiBand > 4 {
		return fmt.Errorf("invalid anti-banding mode")
	}
	if c.Brightness < -100 || c.Brightness > 100 || (c.Contrast != 0 && (c.Contrast < 10 || c.Contrast > 200)) || (c.Gamma != 0 && (c.Gamma < 50 || c.Gamma > 300)) || c.Sharpness > 10 ||
		(!l.ISPImageControls && (c.Brightness != 0 || c.Contrast != 0 || c.Gamma != 0 || c.Sharpness != 0)) {
		return fmt.Errorf("unsupported ISP image controls")
	}
	if c.WBTemperature != 0 && (c.WBTemperature < 2000 || c.WBTemperature > 10000 || c.WBTemperature%100 != 0) {
		return fmt.Errorf("invalid white balance temperature")
	}
	if c.Compensation < l.CompensationMin || c.Compensation > l.CompensationMax || (c.AELocked && !l.AELock) || !c.AERegion.valid(l.Crop) || !c.AFRegion.valid(l.Crop) {
		return fmt.Errorf("unsupported AE/AF region or exposure compensation/lock")
	}
	if c.ExposureNS > 0 {
		if !l.ManualExposure || c.ExposureNS < l.ExposureMinNS || c.ExposureNS > l.ExposureMaxNS || c.ExposureNS > l.FrameDurationNS || c.AELocked || c.Compensation != 0 || c.AERegion != (Region{}) {
			return fmt.Errorf("manual exposure conflicts with limits, FPS or AE controls")
		}
	} else if c.ISO != 0 {
		return fmt.Errorf("manual ISO requires manual exposure")
	}
	if c.ISO != 0 && (!l.ManualISO || c.ISO < l.ISOMin || c.ISO > l.ISOMax) {
		return fmt.Errorf("unsupported manual ISO")
	}
	if c.Focus < FocusOff || c.Focus > FocusContinuousPicture || l.FocusModes&(1<<c.Focus) == 0 || c.Trigger > FocusCancel {
		return fmt.Errorf("unsupported focus mode/trigger")
	}
	if c.Focus == FocusOff && (c.Trigger != FocusIdle || c.AFRegion != (Region{})) {
		return fmt.Errorf("AF command supplied with AF off")
	}
	// The pinned continuous-mode setter discards a requested AF rectangle.
	// Refuse that combination instead of claiming a region was applied.
	if (c.Focus == FocusContinuousVideo || c.Focus == FocusContinuousPicture) && c.AFRegion != (Region{}) {
		return fmt.Errorf("AF region is not supported in continuous focus mode")
	}
	if !finite(c.FocusDioptres) || (c.FocusDioptres != -1 && (c.FocusDioptres < 0 || !l.ManualFocus || c.FocusDioptres > l.MaxFocusDioptres || c.Focus != FocusOff)) {
		return fmt.Errorf("unsupported manual lens distance")
	}
	if c.WhiteBalance < WBAuto || c.WhiteBalance > WBCustomK || l.WBModeMask&(1<<c.WhiteBalance) == 0 || (c.WhiteBalance == WBCustomK && c.WBTemperature == 0) || (c.AWBLocked && (!l.AWBLock || c.WhiteBalance != WBAuto)) {
		return fmt.Errorf("unsupported white balance/lock")
	}
	return nil
}

// ApplyControls is all-or-nothing. It resets stale manual state on transition to
// AE/AF, but preserves node groups, unrelated metadata and dynamic results.
func (v View) ApplyControls(c Controls, l Limits) error {
	if e := v.valid(); e != nil {
		return e
	}
	if e := c.Validate(l); e != nil {
		return e
	}
	// Prevent using a limit from a different mode/request against this plane.
	if le.Uint64(v.data[FrameDurationOffset:]) != l.FrameDurationNS {
		return fmt.Errorf("control limits belong to another frame interval")
	}
	for i, x := range []uint32{l.Crop.X, l.Crop.Y, l.Crop.Width, l.Crop.Height} {
		if le.Uint32(v.data[0x3a4+i*4:]) != x {
			return fmt.Errorf("control limits belong to another sensor crop")
		}
	}
	word := func(off int, x uint32) { le.PutUint32(v.data[off:], x) }
	band := c.AntiBand
	if band == 0 {
		band = 4
	}
	word(0x210, band)
	locked := func(b bool) uint32 {
		if b {
			return 2
		}
		return 1
	}
	region := func(off int, r Region) {
		vals := [5]uint32{0, 0, 0, 0, 1000}
		if r != (Region{}) {
			vals = [5]uint32{r.Rect.X, r.Rect.Y, r.Rect.X + r.Rect.Width, r.Rect.Y + r.Rect.Height, r.Weight}
		}
		for i, x := range vals {
			word(off+4*i, x)
		}
	}
	// Zeroing exposure first is essential: the vendor AE setter otherwise refuses
	// to restore AE when the previous request was manual.
	le.PutUint64(v.data[0x3b8:], c.ExposureNS)
	word(0x21c, 100)
	word(0x218, locked(c.AELocked))
	word(0x214, uint32(c.Compensation))
	region(0x220, c.AERegion)
	if c.ExposureNS != 0 {
		word(0x21c, 1)
		for off := 0x220; off <= 0x230; off += 4 {
			word(off, 0)
		}
	}
	word(0x29c, 1)
	if c.ISO != 0 {
		word(0x29c, 2)
	}
	word(0x2a0, c.ISO)
	word(0x3c8, c.ISO)
	word(0x240, uint32(c.Focus))
	word(0x258, uint32(c.Trigger))
	// Jump table at 0x104b6c in the pinned metadata translator, entries
	// 2..5: single=0, macro=2, continuous-video=1, continuous-picture=0.
	option := uint32(0)
	if c.Focus == FocusMacro {
		option = 2
	}
	if c.Focus == FocusContinuousVideo {
		option = 1
	}
	word(0x290, option)
	region(0x244, c.AFRegion)
	word(0x384, math.Float32bits(c.FocusDioptres))
	word(0x260, uint32(c.WhiteBalance))
	word(0x25c, locked(c.AWBLocked))
	if c.WhiteBalance == WBCustomK {
		word(0x2a4, c.WBTemperature)
	} else {
		word(0x2a4, 0)
	}
	if l.ISPImageControls {
		// These are 32 tone-curve metadata knots, never CPU video pixels.
		contrast, gamma := c.Contrast, c.Gamma
		if contrast == 0 {
			contrast = 100
		}
		if gamma == 0 {
			gamma = 100
		}
		word(0x700, 2)
		if c.Brightness != 0 || contrast != 100 || gamma != 100 {
			word(0x700, 1)
			for i := 0; i < 32; i++ {
				x := float64(i) / 31
				y := max(0, min(1, (math.Pow(x, 100/float64(gamma))-.5)*float64(contrast)/100+.5+float64(c.Brightness)/100))
				for _, offset := range []int{0x3fc, 0x4fc, 0x5fc} {
					word(offset+i*8, math.Float32bits(float32(x)))
					word(offset+i*8+4, math.Float32bits(float32(y)))
				}
			}
		}
		word(0x2f8, 1)
		if c.Sharpness != 0 {
			word(0x2f8, 2)
		}
		v.data[0x2fc] = byte(c.Sharpness)
	}
	return nil
}

// FrameCount is the ISP's dynamic request count, NOT a locally invented FPS.
func (v View) FrameCount() (uint32, error) {
	if e := v.valid(); e != nil {
		return 0, e
	}
	return le.Uint32(v.data[0x29a0:]), nil
}

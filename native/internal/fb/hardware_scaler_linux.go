//go:build linux && (amd64 || arm64)

package fb

import (
	"errors"
	"fmt"
	"perimode/native/internal/decon"

	"perimode/native/internal/media"
)

type HardwarePresenterStatus struct {
	CPURasterRejected bool                  `json:"cpu_raster_rejected"`
	Scanout           *decon.PresenterStats `json:"decon_scanout,omitempty"`
	Required          bool                  `json:"required"`
	Active            bool                  `json:"active"`
	Backend           string                `json:"backend"`
	Device            string                `json:"device"`
	Frames            uint64                `json:"frames"`
	MeanUS            int64                 `json:"mean_us"`
	MaxUS             int64                 `json:"max_us"`
	LastUS            int64                 `json:"last_us"`
	TargetBytes       int                   `json:"target_bytes"`
	LastError         string                `json:"last_error"`
}

func (b *Buffer) HardwarePresenterStatus() HardwarePresenterStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := HardwarePresenterStatus{Required: b.s7BootVisual, Backend: "Exynos M2M scaler / direct fb USERPTR", LastError: b.scalerError}
	v.CPURasterRejected = b.cpuRasterRejected
	if b.scanout != nil && b.scanout.UsesLayers() {
		stats := b.scanout.Stats()
		v.Scanout = &stats
		v.Active = stats.Active
		v.Frames = stats.NV12Frames
		v.LastUS = stats.LastFenceUS
		v.Backend = "MFC NV12 DMA-BUF -> VPP -> DECON hardware layers"
		if _, camera := b.scanout.CameraView(); camera {
			v.Backend = "FIMC NV12 DMA-BUF -> VPP -> DECON hardware layers"
		}
		v.Device = "/dev/fb0 / VGR0"
		v.LastError = stats.Error
		return v
	}
	if b.scanout != nil {
		stats := b.scanout.Stats()
		v.Scanout = &stats
		v.Backend = "Exynos scaler + G2D + DECON fenced double buffer"
		if b.scalerDMAMode {
			v.Backend = "Exynos scaler DMA-BUF -> DECON fenced double buffer"
		}
	}
	s := b.scalerLast
	if b.scaler != nil {
		s = b.scaler.Stats()
		v.Active = true
	}
	v.Device, v.Frames, v.LastUS, v.MeanUS, v.MaxUS, v.TargetBytes = s.Device, s.Frames, s.LastUS, s.MeanUS, s.MaxUS, s.TargetBytes
	return v
}

func (b *Buffer) closeScalerLocked() error {
	if b.scaler == nil {
		return nil
	}
	b.scalerLast = b.scaler.Stats()
	err := b.scaler.Close()
	if err == nil {
		b.scaler = nil
	}
	return err
}

func (b *Buffer) scalerPixelFormat() (uint32, error) {
	if b.V.Bits != 32 || b.V.Green != (BitField{Offset: 8, Length: 8}) || (b.V.Alpha.Length != 0 && b.V.Alpha != (BitField{Offset: 24, Length: 8})) {
		return 0, fmt.Errorf("hardware scaler requires packed 32-bit RGB")
	}
	if b.V.Red == (BitField{Offset: 16, Length: 8}) && b.V.Blue == (BitField{Length: 8}) {
		// Native WIN_CONFIG consumes canonical ARGB words, as do Mali/G2D.
		// Only the legacy fb0 scanout used the opposite byte order.
		if b.scanout != nil {
			return media.BGR32, nil
		}
		return media.RGB32, nil
	}
	if b.V.Red == (BitField{Length: 8}) && b.V.Blue == (BitField{Offset: 16, Length: 8}) {
		return media.BGR32, nil
	}
	return 0, fmt.Errorf("unsupported framebuffer channel order")
}

func directScanoutTarget(data []byte, v Variable, f Fixed, stride int) ([]byte, error) {
	rowBytes := int(v.X) * 4
	frameBytes := rowBytes * int(v.Y)
	if v.Bits != 32 || stride != rowBytes || v.XOffset != 0 || f.LineLength != uint32(rowBytes) {
		return nil, fmt.Errorf("direct hardware scanout requires contiguous fb rows")
	}
	start := int(v.YOffset) * rowBytes
	if start < 0 || start > len(data) || frameBytes > len(data)-start {
		return nil, fmt.Errorf("hardware scaler target extent changed")
	}
	return data[start : start+frameBytes], nil
}

func (b *Buffer) writeHardwareLocked(im media.Image) (bool, error) {
	if !b.s7BootVisual {
		return false, nil
	}
	if !s7BootLayout(b.V, b.F) {
		return true, fmt.Errorf("S7 hardware presenter layout changed")
	}
	direct := b.scanout != nil
	b.directFrameReady = false
	if b.scalerTried && b.scalerDMAMode != direct {
		if err := b.closeScalerLocked(); err != nil {
			return true, err
		}
		b.scalerTried = false
	}
	if !b.scalerTried {
		b.scalerTried = true
		b.scalerDMAMode = direct
		pixel, err := b.scalerPixelFormat()
		if err == nil {
			if direct {
				b.scaler, err = media.OpenRGBScalerDMABuf([]string{"/dev/video50", "/dev/video51"}, int(b.V.X), int(b.V.Y), int(b.Rotation), pixel)
			} else {
				b.scaler, err = media.OpenRGBScaler([]string{"/dev/video50", "/dev/video51"}, int(b.V.X), int(b.V.Y), int(b.Rotation), pixel)
			}
		}
		if err != nil {
			b.scalerError = err.Error()
			return true, fmt.Errorf("hardware presenter unavailable: %w", err)
		}
	}
	if b.scaler == nil {
		return true, fmt.Errorf("hardware presenter unavailable: %s", b.scalerError)
	}
	if direct {
		fd, err := b.scanout.BackFD()
		if err != nil {
			return true, err
		}
		menu := b.menuComposer != nil && b.menuVideo != nil
		if menu {
			fd = int(b.menuVideo.Fd())
		}
		if !b.Rotation.Swapped() {
			if err := b.scanout.ClearBack(); err != nil {
				return true, err
			}
		}
		if err := b.scaler.TransformToDMABuf(im, fd, int(b.V.X*b.V.Y*4)); err != nil {
			b.scalerError = err.Error()
			return true, errors.Join(err, b.closeScalerLocked())
		}
		b.directFrameFD = fd
		b.directFrameReady = !menu
		return true, nil
	}
	stride := b.scaler.TargetStride()
	output := b.Data
	if b.menuComposer != nil {
		frameBytes := int(b.V.Y) * int(b.F.LineLength)
		if frameBytes <= 0 || len(b.Data) != 2*frameBytes {
			return true, fmt.Errorf("G2D menu requires one complete offscreen framebuffer page")
		}
		output = b.Data[frameBytes:]
	}
	target, err := directScanoutTarget(output, b.V, b.F, stride)
	if err != nil {
		return true, err
	}
	if b.scanout != nil && !b.Rotation.Swapped() {
		if err := b.scanout.ClearWorkspace(target); err != nil {
			return true, err
		}
	}
	if err := b.scaler.TransformTo(im, target); err != nil {
		b.scalerError = err.Error()
		return true, errors.Join(fmt.Errorf("hardware scaler transform: %w", err), b.closeScalerLocked())
	}
	return true, nil
}

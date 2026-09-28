package appliance

import (
	"context"
	"fmt"
	"perimode/native/internal/media"
	"perimode/native/internal/mediacodec"
	"perimode/native/internal/mediaruntime"
)

// Both production choices use the phone hardware. MediaCodec calls the original
// Android C API; mfc uses direct V4L2. Selection is explicit, never an error fallback.
type DecoderBackend interface {
	Submit([]byte, uint64) error
	Drain(func(media.Image) error) (int, error)
	Close() error
}

func (u *UI) openMonitorDecoder(ctx context.Context, path string, first []byte, pts uint64, fps uint32) (DecoderBackend, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	settings, _, _ := u.state.Current()
	w, h := settings.Dimensions()
	sw, sh, err := media.MonitorKeyDimensions(first)
	if err != nil {
		return nil, err
	}
	if sw != w || sh != h {
		return nil, fmt.Errorf("monitor SPS differs from requested dimensions")
	}
	if u.codecSelection.Monitor == "mediacodec" {
		d, err := mediacodec.OpenDecoder(ctx, first, pts, fps)
		if d == nil {
			return nil, err
		}
		return d, err
	}
	d, err := media.OpenDecoder(path, first, pts)
	if d == nil {
		return nil, err
	}
	return d, err
}
func (u *UI) selectDecoder(devices []media.Device) (string, error) {
	selected, err := mediacodec.LoadSelection()
	if err != nil {
		return "", err
	}
	u.codecSelection = selected
	if selected.Monitor == "mediacodec" {
		if err = mediacodec.Available(); err != nil {
			return "", err
		}
		return "mediacodec:OMX.Exynos.avc.dec", nil
	}
	return media.DecoderPath(devices)
}

func (u *UI) monitorBackendReady() bool {
	return u.device != "" && u.screen != nil && (u.codecSelection.Monitor != "mediacodec" || mediaruntime.Snapshot().Ready)
}

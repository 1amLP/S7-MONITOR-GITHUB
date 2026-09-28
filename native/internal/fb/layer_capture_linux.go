//go:build linux && (amd64 || arm64)

package fb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"perimode/native/internal/gpumenu"
	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
)

// Compose the currently committed immutable layer buffers ONLY for a requested
// screenshot. Normal monitor/menu/Preview presentation never enters this path.
func (b *Buffer) captureLayersLocked() ([]byte, error) {
	video, rotation, menu, preview := b.scanout.LayerState()
	target, pixels, err := b.scanout.DiagnosticBuffer()
	if err != nil {
		return nil, err
	}
	if b.menuVideo == nil {
		b.menuVideo, err = linuxio.NewIONDMABuf(1440 * 2560 * 4)
		if err != nil {
			return nil, err
		}
	}
	raw := int(b.menuVideo.Fd())
	if video.Lease != nil {
		view, camera := b.scanout.CameraView()
		var scaler *media.RGBScaler
		if camera {
			flipX, flipY := view.Mirror && view.PanelRotation%180 == 0, view.Mirror && view.PanelRotation%180 != 0
			scaler, err = media.OpenCameraRGBSnapshot(video, (view.Rotation+view.PanelRotation)%360, flipX, flipY, view.Zoom)
		} else {
			scaler, err = media.OpenMonitorRGBSnapshot(video, rotation)
		}
		if err != nil {
			return nil, err
		}
		err = b.scanout.CaptureClear(raw)
		if err == nil {
			err = scaler.TransformToDMABuf(video, raw, 1440*2560*4)
		}
		if err = errors.Join(err, scaler.Close()); err != nil {
			return nil, err
		}
	} else if err = b.scanout.CaptureClear(raw); err != nil {
		return nil, err
	}
	if menu.Enabled {
		if b.menuGPU == nil {
			return nil, fmt.Errorf("menu snapshot owner missing")
		}
		scene, e := gpumenu.NewScene(b.menuGPU, raw, func() (int, error) { return target, nil })
		if e != nil {
			return nil, e
		}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_, err = scene.ComposeFrame(ctx)
		cancel()
		if err != nil {
			b.hardwareGlassUnsafe = true
			return nil, err
		}
	} else if err = b.scanout.CaptureCopy(raw, target); err != nil {
		return nil, err
	}
	if err = b.scanout.CapturePreview(target, preview); err != nil {
		return nil, err
	}
	return pixels, nil
}

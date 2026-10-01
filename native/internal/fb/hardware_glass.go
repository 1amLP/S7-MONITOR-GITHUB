//go:build linux && (amd64 || arm64)

package fb

import (
	"fmt"
	"perimode/native/internal/media"
	"slices"
	"time"
)

func deferMenuTelemetry(previous, next MenuStatus, sameLayout bool, lastFrame, now time.Time) bool {
	if !sameLayout || lastFrame.IsZero() || now.Before(lastFrame) || now.Sub(lastFrame) > 50*time.Millisecond {
		return false
	}
	previous.Rate, previous.Battery = next.Rate, next.Battery
	previous.Temperature, previous.Load = next.Temperature, next.Load
	return previous == next
}

func (b *Buffer) hardwareGlassMenu(lines []string, source *media.Image, l GlassLayout) (bool, error) {
	b.cameraHUD = nil
	if b.menuGPU == nil || b.scanout == nil {
		return true, fmt.Errorf("GPU menu unavailable; no CPU fallback")
	}
	sc := b.glass
	if sc != nil && sc.sourceBacked && b.layerMenuFD > 0 && !b.menuStatusDirty && !b.menuForegroundDirty && !b.menuCategoryDirty && !b.menuHeaderDirty && slices.Equal(sc.lines, lines) && sc.layout.Panel == l.Panel && sc.layout.Scroll == l.Scroll {
		if b.layerMenuPending {
			// A stopped video stream must not leave staged telemetry invisible.
			return true, b.commitLayersLocked(nil)
		}
		return true, nil
	}
	deferCommit := source == nil && b.menuStatusDirty && deferMenuTelemetry(b.menuPreviousStatus, b.menuStatus,
		sc != nil && sc.sourceBacked && !l.Statistics && !l.PowerOnly && slices.Equal(sc.lines, lines) && sc.layout.Panel == l.Panel && sc.layout.Scroll == l.Scroll,
		b.lastMonitorFrame, time.Now())
	if sc == nil {
		sc = &glassScene{sourceBacked: true}
	}
	commands, err := b.gpuMenuCommands(lines, l)
	if err != nil {
		return true, err
	}
	_, _, active, _ := b.scanout.LayerState()
	slot := 0
	if active.Enabled && active.FD == b.menuGPU.MenuFD(0) {
		slot = 1
	}
	if err = b.scanout.WaitMenuReusable(b.menuGPU.MenuFD(slot)); err != nil {
		return true, err
	}
	fd, err := b.menuGPU.RenderSlot(int(b.Rotation), b.Width, b.Height, commands, slot)
	if err != nil {
		return true, err
	}
	sc.ink = nil
	sc.layout = l
	sc.lines = append(sc.lines[:0], lines...)
	sc.stageReady = true
	sc.previewDocked = b.preview != nil
	b.glass = sc
	b.layerMenuFD = fd
	occupied := l.Panel.Union(b.noticeBounds(b.menuStatus.Notice))
	if b.layerMenuRect, err = b.overlayBounds(occupied); err != nil {
		return true, err
	}
	if deferCommit {
		b.layerMenuPending = true
	} else {
		if err = b.commitLayersLocked(source); err != nil {
			return true, err
		}
	}
	b.liveStats.BlurUS, b.liveStats.BlendUS = 0, 0
	b.liveStats.ForegroundBuilds++
	b.menuStatusDirty = false
	b.menuForegroundDirty = false
	b.menuCategoryDirty = false
	b.menuHeaderDirty = false
	b.menuPreviousStatus = b.menuStatus
	return true, nil
}

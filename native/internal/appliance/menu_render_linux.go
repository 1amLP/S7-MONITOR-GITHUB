//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"time"

	"perimode/native/internal/fb"
	"perimode/native/internal/media"
)

type menuPaintRequest struct {
	queued     time.Time
	revision   uint64
	generation uint32
	page       string
	lines      []string
	scroll     int
	status     fb.MenuStatus
	hud        *fb.CameraHUD
}
type menuPaintResult struct {
	request menuPaintRequest
	layout  fb.GlassLayout
	err     error
}

// The input owner publishes only the newest desired scene. It never waits for
// an OpenCL dispatch, a scaler ioctl or a framebuffer commit.
func (u *UI) queueMenuPaint() {
	u.menuNeedsDraw = false
	_, generation, page := u.state.Current()
	var hud *fb.CameraHUD
	if page == "" {
		hud = u.currentCameraHUD()
	}
	if page == "" && u.paintRequestedPage == "" && hud == nil && !u.cameraHUDVisible {
		return
	}
	u.paintRevision++
	u.paintRequestedPage = page
	r := menuPaintRequest{queued: time.Now(), revision: u.paintRevision, generation: generation, page: page, scroll: u.menuScroll, hud: hud}
	if page != "" {
		r.lines = append([]string(nil), u.lines()...)
		r.status = u.currentMenuStatus()
	}
	select {
	case <-u.menuPaintJobs:
	default:
	}
	select {
	case u.menuPaintJobs <- r:
	default:
	}
}

func (u *UI) menuPaintWorker(ctx context.Context) {
	for {
		var request menuPaintRequest
		select {
		case <-ctx.Done():
			return
		case request = <-u.menuPaintJobs:
		}
		u.presentationMu.Lock()
		select {
		case request = <-u.menuPaintJobs:
		default:
		}
		if ctx.Err() != nil {
			u.presentationMu.Unlock()
			return
		}
		_, generation, page := u.state.Current()
		if request.page != page || request.generation != generation {
			u.presentationMu.Unlock()
			continue
		}
		result := menuPaintResult{request: request}
		started := time.Now()
		if u.screen != nil {
			u.screen.SetVideoGeneration(generation)
			if page == "" {
				if request.hud == nil {
					result.err = u.screen.EndGlass()
				}
				if result.err == nil {
					result.err = u.screen.RenderCameraHUD(request.hud)
				}
			} else {
				result.err = u.screen.SetMenuStatus(request.status)
				if result.err == nil {
					var source *media.Image
					if u.presentFrames != nil {
						if latest, ok := u.presentFrames.snapshotLatest(generation); ok {
							source = &latest
						}
					}
					result.layout, result.err = u.screen.GlassMenuSource(request.lines, request.scroll, source)
				}
			}
		}
		u.presentationMu.Unlock()
		u.inputTiming.menuPaint(request.queued, started, time.Now())
		select {
		case <-u.menuPaintDone:
		default:
		}
		select {
		case u.menuPaintDone <- result:
		case <-ctx.Done():
			return
		}
	}
}

func (u *UI) acceptMenuPaint(result menuPaintResult) {
	_, generation, page := u.state.Current()
	if result.request.revision < u.paintPageStart || result.request.revision <= u.paintAcceptedRevision || result.request.revision > u.paintRevision || result.request.page != page || result.request.generation != generation {
		return
	}
	if result.request.revision != u.paintRevision && result.request.status.Focused != u.menuFocus {
		return
	}
	if result.err != nil {
		u.state.Error(result.err)
		return
	}
	u.paintAcceptedRevision = result.request.revision
	if page != "" {
		u.cameraHUDVisible = false
		// Hit testing follows the frame actually committed, even while a newer
		// scroll is rendering. Do not roll back the newest desired scroll offset.
		if result.request.revision == u.paintRevision {
			u.menuScroll = result.layout.Scroll
		}
		u.menuLayout = result.layout
		u.menuLines = append(u.menuLines[:0], result.request.lines...)
	} else {
		u.cameraHUDVisible = result.request.hud != nil
		if result.request.hud != nil {
			u.cameraHUDPaint = *result.request.hud
		}
	}
}

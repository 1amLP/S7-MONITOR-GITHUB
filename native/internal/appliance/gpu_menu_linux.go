//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"perimode/native/internal/fb"
	"perimode/native/internal/gpumenu"
)

const gpuMenuExecutable = "/opt/s7-gpu/bin/menu-worker-arm64"

func (u *UI) startGPUMenu(root string) error {
	if root != gpuSystemMount {
		return fmt.Errorf("GPU SYSTEM unavailable")
	}
	if u.screen == nil {
		return fmt.Errorf("GPU menu needs framebuffer")
	}
	info, err := os.Lstat(gpuMenuExecutable)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<10 {
		return fmt.Errorf("GPU menu worker unavailable: %v", err)
	}
	return u.workers.Start("gpu-menu", func(ctx context.Context) (err error) {
		failed := func(err error) error {
			u.state.mu.Lock()
			u.state.GPUProbeStatus = "MALI MENU FAILED"
			u.state.GPUProbeError = err.Error()
			u.state.mu.Unlock()
			u.state.Fault("gpu-menu", err)
			return err
		}
		renderer, err := gpumenu.Start(ctx, root+"/system/bin/bootstrap/linker64", gpuMenuExecutable, []string{
			"LD_LIBRARY_PATH=" + gpuLibraryPath(root), "ANDROID_ROOT=" + root + "/system", "TMPDIR=/run",
		}, fb.MenuGlyphAtlas())
		if err != nil {
			return failed(err)
		}
		defer func() {
			u.screen.SetPreviewGPUFactory(nil)
			err = errors.Join(err, u.screen.ClosePreviewGPU())
			if detachErr := u.screen.AttachMenuGPU(nil); detachErr != nil {
				err = errors.Join(err, detachErr)
				return
			}
			err = errors.Join(err, renderer.Close())
		}()
		if err = u.screen.AttachMenuGPU(renderer); err != nil {
			return failed(err)
		}
		u.screen.SetPreviewGPUFactory(func() (*gpumenu.Renderer, error) {
			return gpumenu.StartPreview(ctx, root+"/system/bin/bootstrap/linker64", gpuMenuExecutable, []string{"LD_LIBRARY_PATH=" + gpuLibraryPath(root), "ANDROID_ROOT=" + root + "/system", "TMPDIR=/run"})
		})
		u.state.mu.Lock()
		u.state.GPUProbeStatus = "MALI MENU READY"
		u.state.GPUProbeError = ""
		u.state.mu.Unlock()
		log.Print("S7 Mali menu renderer ready; CPU pixel renderer disabled")
		if u.screen.BeginBootOrbit() {
			start := time.Now()
			tick := time.NewTicker(time.Second / 30)
			for ctx.Err() == nil && time.Since(start) < 15*time.Second && (!u.bootReady.Load() || time.Since(start) < 1200*time.Millisecond) {
				if !u.presentationMu.LockContext(ctx) {
					break
				}
				active, drawErr := u.screen.RenderBootOrbit(time.Since(start))
				u.presentationMu.Unlock()
				if drawErr != nil {
					tick.Stop()
					return failed(drawErr)
				}
				if !active {
					break
				}
				select {
				case <-ctx.Done():
				case <-tick.C:
				}
			}
			tick.Stop()
			if err = u.screen.EndBootOrbit(); err != nil {
				return failed(err)
			}
			select {
			case u.opsDone <- struct{}{}:
			default:
			}
		}
		<-ctx.Done()
		return ctx.Err()
	})
}

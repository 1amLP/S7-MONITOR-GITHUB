//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"fmt"
	"perimode/native/internal/lifecycle"
	"perimode/native/internal/safety"
)

// Each worker below is the existing production loop, not a substitute or test
// implementation. Camera/UVC remain owned by Transport and SharedProvider.
func (u *UI) startWorkers(health *safety.Liveness, fatal func(error)) error {
	if u.workers == nil || health == nil || fatal == nil {
		return fmt.Errorf("native worker owners absent")
	}
	if err := u.workers.Start("media-runtime", func(ctx context.Context) error {
		err := u.mediaRuntimeWorker(ctx)
		if u.labMode && err != nil && ctx.Err() == nil {
			fatal(err)
		}
		return err
	}); err != nil {
		return err
	}
	if u.presentFrames == nil {
		u.presentFrames = newFrameMailbox()
	}
	for _, w := range []struct {
		name string
		run  func(context.Context)
	}{
		{"menu-renderer", u.menuPaintWorker},
		{"monitor-presenter", u.presentationWorker},
		{"monitor", u.decode}, {"audio", func(c context.Context) { u.audio(c, fatal) }},
		{"controls", func(c context.Context) { u.controlWorker(c, health) }},
		{"rotation", u.orientationWorker}, {"brightness", u.ambientWorker},
		{"torch", func(c context.Context) { u.torchWorker(c, fatal) }}, {"preview", u.previewWorker},
		{"camera-trial", u.cameraTrialWorker},
	} {
		run := w.run
		if err := u.workers.Start(w.name, func(ctx context.Context) error { run(ctx); return ctx.Err() }); err != nil {
			return err
		}
	}
	return nil
}
func (u *UI) workerLines() []string {
	out := []string{"DEVICE / NATIVE TASKS", "RUNNING IS NOT HARDWARE ACCEPTANCE"}
	if u.workers == nil {
		out = append(out, "REGISTRY NOT ATTACHED")
	} else {
		for _, w := range u.workers.Snapshot() {
			out = append(out, w.Name+": "+w.State)
			if w.Error != "" {
				out = append(out, "ERROR: "+w.Error)
			}
		}
	}
	return append(out, "CAMERA / USB: SEE THEIR OWN STREAM STATUS", "BACK: DEVICE")
}
func (u *UI) workerSnapshot() []lifecycle.Worker {
	if u.workers == nil {
		return nil
	}
	return u.workers.Snapshot()
}

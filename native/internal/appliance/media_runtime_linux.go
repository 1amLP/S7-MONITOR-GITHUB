//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"perimode/native/internal/mediaruntime"
	"perimode/native/internal/selinux"
	"os"
	"syscall"
)

// This worker is the only runtime deployment entry during ordinary operation.
// CACHE remains read-only unless the independent settings consent already owns
// a writable mount. Missing media consent is an error, never a format/remount.
func (u *UI) mediaRuntimeWorker(ctx context.Context) error {
	if u.codecSelection.Monitor != "mediacodec" && u.codecSelection.Camera != "mediacodec" {
		<-ctx.Done()
		return ctx.Err()
	}
	if u.codecSelection.Monitor == "mediacodec" {
		u.state.SetConsumer(false)
	}
	err := u.runMediaRuntime(ctx)
	if u.codecSelection.Monitor == "mediacodec" {
		u.state.SetConsumer(false)
	}
	if err != nil {
		if ctx.Err() == nil {
			startupLog := KernelMessages()
			u.reportMu.Lock()
			if u.report == nil {
				u.report = map[string]any{}
			}
			u.report["media_startup_kernel_messages"] = startupLog
			u.reportMu.Unlock()
		}
		mediaruntime.Fail(err)
		u.state.Error(fmt.Errorf("native MediaCodec services: %w", err))
	}
	return err
}
func (u *UI) runMediaRuntime(ctx context.Context) (err error) {
	pin, e := mediaruntime.LoadPin()
	if e != nil {
		return e
	}
	serial, e := Identity()
	if e != nil {
		return e
	}
	source, e := runtimeSource()
	if e != nil {
		return e
	}
	root := cacheMount
	mounted := false
	var closeMount func() error
	if source == "system" {
		root, closeMount, e = mountSystemRuntime()
		if e != nil {
			return fmt.Errorf("mount pinned appliance SYSTEM: %w", e)
		}
		mounted = true
	} else if u.cache == nil {
		device, e := cacheDevice(false)
		if e != nil {
			return e
		}
		root = "/run/s7-media-cache"
		if e = os.Mkdir(root, 0700); e != nil {
			return e
		}
		if e = syscall.Mount(device, root, "ext4", syscall.MS_RDONLY|syscall.MS_NODEV|syscall.MS_NOSUID|syscall.MS_NOEXEC, selinux.CacheMountOptions); e != nil {
			return e
		}
		mounted = true
		closeMount = func() error { return syscall.Unmount(root, 0) }
	}
	defer func() {
		if mounted && closeMount != nil {
			err = errors.Join(err, closeMount())
		}
	}()
	f, e := mediaruntime.OpenInstalled(root+"/s7-media", pin, serial)
	if e != nil {
		return fmt.Errorf("install the pinned native-media bundle from recovery first: %w", e)
	}
	defer f.Close()
	return mediaruntime.Run(ctx, f, pin, func() error {
		if e := f.Close(); e != nil {
			return e
		}
		if mounted && closeMount != nil {
			if e := closeMount(); e != nil {
				return e
			}
			mounted = false
		}
		return nil
	}, func() {
		if u.codecSelection.Monitor == "mediacodec" {
			u.state.SetConsumer(u.monitorBackendReady())
		}
	})
}

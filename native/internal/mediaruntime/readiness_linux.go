//go:build linux && (amd64 || arm64)

package mediaruntime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// Startup uses the original read-only Binder and HIDL service clients.
// Registration of the two Binder and four HIDL endpoints is not codec readiness;
// OMX allocation and H264 availability are decided by AMediaCodec configure/start.
var brokerReady atomic.Bool

type serviceOutput struct{ text []byte }

func (b *serviceOutput) Write(p []byte) (int, error) {
	if len(b.text)+len(p) > 4096 {
		return 0, errors.New("media service response exceeds bound")
	}
	b.text = append(b.text, p...)
	return len(p), nil
}
func checkService(ctx context.Context, program, name string) error {
	if name != "media.player" && name != "media.resource_manager" {
		return errors.New("unapproved media service query")
	}
	c := exec.CommandContext(ctx, program, "check", name)
	c.WaitDelay = 200 * time.Millisecond
	c.Env = []string{"PATH=/nonexistent", "ANDROID_ROOT=/system", "ANDROID_DATA=/data", "ANDROID_RUNTIME_ROOT=/apex/com.android.runtime", "ANDROID_I18N_ROOT=/apex/com.android.i18n"}
	out, diagnostic := &serviceOutput{}, &serviceOutput{}
	c.Stdout, c.Stderr = out, diagnostic
	if e := c.Run(); e != nil {
		return fmt.Errorf("%s registration check: %w (%s)", name, errors.Join(e, ctx.Err()), strings.TrimSpace(string(diagnostic.text)))
	}
	if strings.TrimSpace(string(out.text)) != "Service "+name+": found" {
		return fmt.Errorf("%s has not registered", name)
	}
	return nil
}
func binderRegistered(ctx context.Context) error {
	for _, name := range []string{"media.player", "media.resource_manager"} {
		step, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		e := checkService(step, "/system/bin/service", name)
		cancel()
		if e != nil {
			return e
		}
	}
	return nil
}

// Query only the four fixed binderized endpoints that the byte-buffer media
// path needs. The original lshal 'wait' uses getRawServiceInternal(retry=true,
// getStub=false). It does not invoke HAL debug(), list unrelated services or
// accept a passthrough library merely because its file is present.
var requiredHIDL = [...]string{
	"android.hidl.allocator@1.0::IAllocator/ashmem",
	"android.hardware.graphics.allocator@2.0::IAllocator/default",
	"android.hardware.media.omx@1.0::IOmx/default",
	"android.hardware.media.omx@1.0::IOmxStore/default",
}

func waitHIDL(ctx context.Context, program, name string) error {
	approved := false
	for _, wanted := range requiredHIDL {
		approved = approved || name == wanted
	}
	if !approved {
		return errors.New("unapproved HIDL endpoint")
	}
	c := exec.CommandContext(ctx, program, "wait", name)
	c.WaitDelay = 200 * time.Millisecond
	c.Env = []string{"PATH=/nonexistent", "ANDROID_ROOT=/system", "ANDROID_DATA=/data", "ANDROID_RUNTIME_ROOT=/apex/com.android.runtime", "ANDROID_I18N_ROOT=/apex/com.android.i18n"}
	// 'wait' has no success text. Exit status, not a string that happens to
	// contain the endpoint name, determines success. Bound both error streams.
	out, diagnostic := &serviceOutput{}, &serviceOutput{}
	c.Stdout, c.Stderr = out, diagnostic
	if e := c.Run(); e != nil {
		return fmt.Errorf("HIDL %s not acquired: %w (%s)", name, errors.Join(e, ctx.Err()), strings.TrimSpace(string(diagnostic.text)))
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	return nil
}

type registrationGate struct {
	binder      bool
	hidl        [len(requiredHIDL)]bool
	lastFailure error
}

func (g *registrationGate) pending() string {
	if !g.binder {
		return "Binder media.player/media.resource_manager"
	}
	for i, ready := range g.hidl {
		if !ready {
			return requiredHIDL[i]
		}
	}
	return "hardening readback"
}

// Successful lookups are reused only inside one guardNativeServices lifetime.
// A lost/replaced original process stops the namespace; no stale gate is reused
// after a service restart. Lookup success still is NOT codec configure/start.
func (g *registrationGate) check(ctx context.Context, binder func(context.Context) error, lookup func(context.Context, string) error) error {
	if e := ctx.Err(); e != nil {
		return fmt.Errorf("registration stopped at %s: %w; last probe: %v", g.pending(), e, g.lastFailure)
	}
	if !g.binder {
		if e := binder(ctx); e != nil {
			g.lastFailure = e
			return e
		}
		g.binder = true
	}
	for i, name := range requiredHIDL {
		if !g.hidl[i] {
			if e := lookup(ctx, name); e != nil {
				g.lastFailure = e
				return e
			}
			g.hidl[i] = true
		}
	}
	g.lastFailure = nil
	return ctx.Err()
}

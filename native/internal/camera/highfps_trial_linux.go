//go:build linux && (amd64 || arm64)

package camera

import "context"

// RunHighFPSTrial never grants a general-purpose high-FPS source. Only this
// bounded local runner can enter the private diagnostic open path. Its guard
// must check appliance thermal/cancellation state; no automatic retry occurs.
func (p HerolteProvider) RunHighFPSTrial(ctx context.Context, fps uint32, guard func() error) (CaptureTrialReport, error) {
	clock, stop := realtimeTrialClock()
	defer stop()
	settings := highFPSSettings(fps)
	return runCaptureTrial(ctx, settings, func() (Source, error) { return p.openSession(settings, true) }, guard, clock)
}

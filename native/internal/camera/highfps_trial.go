package camera

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"perimode/native/internal/media"
)

const (
	trialWarmup    = 2 * time.Second
	trialMeasure   = 10 * time.Second
	trialStall     = 2 * time.Second
	trialMaxFrames = 8192
)

// CaptureTrialReport contains measurements only, never pictures or DMA pointers.
// A local cadence result cannot unlock a USB/Windows mode or prove new exposures.
type CaptureTrialIdentity struct {
	ProfileID string `json:"profile_id"`
	ModuleID  uint32 `json:"module_id"`
	KernelABI string `json:"kernel_abi"`
	HALSHA256 string `json:"hal_sha256"`
}

type CaptureTrialReport struct {
	Identity             CaptureTrialIdentity `json:"source_identity"`
	Schema               string               `json:"schema"`
	Mode                 Mode                 `json:"mode"`
	Stage                string               `json:"stage"`
	OpenRequested        bool                 `json:"native_open_requested"`
	Completed            bool                 `json:"completed_window"`
	Closed               bool                 `json:"source_closed"`
	WebcamVerified       bool                 `json:"webcam_verified"` // Always false in this NV12-only trial.
	WarmupFrames         uint64               `json:"warmup_frames"`
	Frames               uint64               `json:"measured_frames"`
	WindowNS             int64                `json:"window_ns"`
	FirstPTS             int64                `json:"first_sensor_pts_us"`
	LastPTS              int64                `json:"last_sensor_pts_us"`
	ArrivalSpanNS        int64                `json:"arrival_span_ns"`
	ArrivalFPS           float64              `json:"arrival_fps"`
	TimestampFPS         float64              `json:"sensor_pts_fps"`
	EstimatedMissing     uint64               `json:"estimated_missing_from_pts"`
	MaxArrivalGapNS      int64                `json:"max_arrival_gap_ns"`
	IdenticalLumaSamples uint64               `json:"identical_sparse_luma_samples"`
	CadenceObserved      bool                 `json:"local_cadence_observed"`
	Error                string               `json:"error,omitempty"`
}

func highFPSSettings(fps uint32) Settings {
	s := DefaultSettings()
	s.Sensor = Rear
	s.Mode = Mode{Width: 1280, Height: 720, FPS: fps}
	return s
}
func isHighFPSTrialSettings(s Settings) bool {
	return s.Sensor == Rear && s.Mode.Width == 1280 && s.Mode.Height == 720 &&
		(s.Mode.FPS == 120 || s.Mode.FPS == 240) && s.Image == (ImageOptions{})
}

type trialClock struct {
	now  func() time.Time
	wait func(context.Context) error
}

func realtimeTrialClock() (trialClock, func()) {
	tick := time.NewTicker(time.Millisecond)
	return trialClock{now: time.Now, wait: func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			return nil
		}
	}}, tick.Stop
}

// runCaptureTrial uses the same real source as ordinary capture, without opening
// MFC, USB, files or a preview. The guard is mandatory and checked before open,
// before every drain and inside every callback. Device ioctls cannot be forcibly
// cancelled safely: a stuck kernel call remains owned until shutdown/reboot.
func runCaptureTrial(ctx context.Context, s Settings, open func() (Source, error), guard func() error, clock trialClock) (out CaptureTrialReport, err error) {
	out = CaptureTrialReport{Schema: "S7_NATIVE_HIGH_FPS_TRIAL_1", Mode: s.Mode, Stage: "native-nv12-only", WindowNS: int64(trialMeasure)}
	defer func() {
		if err != nil {
			out.Error = err.Error()
			out.CadenceObserved = false
		}
	}()
	if ctx == nil || open == nil || guard == nil || clock.now == nil || clock.wait == nil || !isHighFPSTrialSettings(s) {
		return out, fmt.Errorf("bounded rear 720p120/240 trial with context and safety guard required")
	}
	if err = s.Validate(); err != nil {
		return out, err
	}
	check := func() error {
		if e := ctx.Err(); e != nil {
			return e
		}
		return guard()
	}
	if err = check(); err != nil {
		return out, err
	}
	out.OpenRequested = true
	src, openErr := open()
	if src != nil {
		defer func() {
			if e := src.Close(); e != nil {
				err = errors.Join(err, e, ErrOwnership)
			} else {
				out.Closed = true
			}
		}()
	}
	if openErr != nil {
		return out, openErr
	}
	if src == nil {
		return out, fmt.Errorf("trial opener returned no source")
	}
	if identified, ok := src.(interface{ CaptureIdentity() CaptureTrialIdentity }); ok {
		out.Identity = identified.CaptureIdentity()
	}
	if err = check(); err != nil {
		return out, err
	}
	start := clock.now()
	lastArrival := start
	lastClock := start
	var firstMeasured, lastMeasured time.Time
	var lastPTS int64
	havePTS := false
	var hash uint64
	contract := modeContract{settings: s.Encoder()}
	windowEnded := false
	callbacks := 0
	observe := func(im media.Image) error {
		callbacks++
		if callbacks > 256 {
			return fmt.Errorf("trial source exceeded bounded drain callbacks")
		}
		if e := check(); e != nil {
			return e
		}
		now := clock.now()
		if now.Before(lastClock) {
			return fmt.Errorf("trial local clock regressed")
		}
		lastClock = now
		elapsed := now.Sub(start)
		if elapsed >= trialWarmup+trialMeasure {
			windowEnded = true
			return nil // Let the source release this frame; do not mask release errors.
		}
		if out.WarmupFrames+out.Frames >= trialMaxFrames {
			return fmt.Errorf("trial frame budget exceeded")
		}
		if e := contract.validateImage(im); e != nil {
			return e
		}
		if im.PTS < 0 || (havePTS && im.PTS <= lastPTS) {
			return fmt.Errorf("trial sensor timestamp is negative, repeated or regressed")
		}
		if now.Sub(lastArrival) > trialStall {
			return fmt.Errorf("trial sensor delivery stalled")
		}
		if havePTS && im.PTS-lastPTS > int64(trialStall/time.Microsecond) {
			return fmt.Errorf("trial sensor timestamp gap exceeds bound")
		}
		if elapsed < trialWarmup {
			out.WarmupFrames++
		} else {
			fingerprint := sparseLumaHash(im)
			if out.Frames == 0 {
				firstMeasured = now
				out.FirstPTS = im.PTS
			} else {
				gap := now.Sub(lastMeasured)
				if int64(gap) > out.MaxArrivalGapNS {
					out.MaxArrivalGapNS = int64(gap)
				}
				if fingerprint == hash {
					out.IdenticalLumaSamples++
				}
				// Delta is bounded above before multiplication. PTS use microseconds.
				intervals := (uint64(im.PTS-out.LastPTS)*uint64(s.Mode.FPS) + 500000) / 1000000
				if intervals > 1 {
					out.EstimatedMissing += intervals - 1
				}
			}
			out.Frames++
			out.LastPTS = im.PTS
			lastMeasured = now
			hash = fingerprint
		}
		lastArrival = now
		lastPTS = im.PTS
		havePTS = true
		return nil
	}
	for {
		if err = check(); err != nil {
			return out, err
		}
		now := clock.now()
		if now.Before(lastClock) {
			return out, fmt.Errorf("trial local clock regressed")
		}
		lastClock = now
		if now.Sub(start) >= trialWarmup+trialMeasure {
			break
		}
		if now.Sub(lastArrival) > trialStall {
			return out, fmt.Errorf("trial sensor produced no frames within deadline")
		}
		callbacks = 0
		_, err = src.Drain(observe)
		// Never suppress a joined ownership/I/O error merely because it also
		// contains EAGAIN/EINTR. Only bare nonblocking statuses are retryable.
		if err != nil && err != syscall.EAGAIN && err != syscall.EINTR {
			return out, err
		}
		if windowEnded {
			err = nil
			break
		}
		if err = clock.wait(ctx); err != nil {
			return out, err
		}
	}
	out.Completed = true
	if out.Frames > 1 {
		out.ArrivalSpanNS = int64(lastMeasured.Sub(firstMeasured))
		if out.ArrivalSpanNS > 0 {
			out.ArrivalFPS = float64(out.Frames-1) * 1e9 / float64(out.ArrivalSpanNS)
		}
		if out.LastPTS > out.FirstPTS {
			out.TimestampFPS = float64(out.Frames-1) * 1e6 / float64(out.LastPTS-out.FirstPTS)
		}
	}
	target := float64(s.Mode.FPS)
	out.CadenceObserved = out.Frames >= uint64(target*trialMeasure.Seconds()*.95) &&
		out.ArrivalSpanNS >= int64(trialMeasure-250*time.Millisecond) &&
		out.ArrivalFPS >= target*.98 && out.ArrivalFPS <= target*1.02 &&
		out.TimestampFPS >= target*.98 && out.TimestampFPS <= target*1.02 &&
		out.EstimatedMissing <= out.Frames/100 && out.MaxArrivalGapNS <= int64(50*time.Millisecond)
	return out, nil
}

// Sampling 256 luminance locations is cheap and stores no picture. Equal hashes
// can be a static scene or a collision, NOT proof of duplicated exposures. A
// moving scene and an independent optical timing test are still needed.
func sparseLumaHash(im media.Image) uint64 {
	h := uint64(14695981039346656037)
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			h ^= uint64(im.Y[(y*(im.Height-1)/15)*im.StrideY+x*(im.Width-1)/15])
			h *= 1099511628211
		}
	}
	return h
}

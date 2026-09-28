package audio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"
)

// BridgePCM is the actual transfer boundary used by both ALSA and the offline
// fault-injection harness. The caller owns Close; no descriptor is closed while
// a transfer worker may still be using it.
type BridgePCM interface {
	TransferContext(context.Context, []byte) (int, error)
	Delay() (int64, error)
	Drop() error
	Recover() error
}

type BridgePair struct {
	Source, Destination           BridgePCM
	Name                          string
	InputChannels, OutputChannels int
	Downmix                       bool
}

type PCMLevels struct {
	InputPeak, OutputPeak uint16
	CaptureOnly           bool
}

func peakPCM(data []byte) uint16 {
	var peak int
	for i := 0; i+1 < len(data); i += 2 {
		sample := int(int16(uint16(data[i]) | uint16(data[i+1])<<8))
		if sample < 0 {
			sample = -sample
		}
		if sample > peak {
			peak = sample
		}
	}
	return uint16(peak)
}

// XRUNBudget bounds a burst of failed transfers. One second of delivered
// blocks rearms it so sparse USB/ALSA clock slips cannot disable audio forever.
type XRUNBudget struct {
	count int
	last  time.Time
	clean int
}

func (b *XRUNBudget) Allow(now time.Time) bool {
	if !b.last.IsZero() && now.Sub(b.last) >= 10*time.Second {
		b.count = 0
	}
	b.last = now
	if b.count >= 8 {
		return false
	}
	b.count++
	b.clean = 0
	return true
}

func (b *XRUNBudget) StableBlock() {
	if b.count == 0 {
		return
	}
	b.clean++
	if b.clean >= 100 {
		b.count = 0
		b.clean = 0
	}
}

// RunPairs drives the same bounded 10ms PCM blocks as the physical bridge. Both
// clock domains are dropped and re-prepared together on XRUN; another direction
// is cancelled on any terminal error. No retries recreate a USB interface.
func RunPairs(ctx context.Context, pairs []BridgePair, cfg BridgeConfig, volume func() (int, int), stats func(string, uint64)) error {
	return RunPairsWithMeter(ctx, pairs, cfg, volume, stats, nil)
}

func RunPairsWithMeter(ctx context.Context, pairs []BridgePair, cfg BridgeConfig, volume func() (int, int), stats func(string, uint64), meter func(string, PCMLevels)) error {
	if ctx == nil {
		return fmt.Errorf("nil audio context")
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if len(pairs) == 0 || len(pairs) > 2 {
		return fmt.Errorf("one or two PCM pairs required")
	}
	seen := map[string]bool{}
	for _, p := range pairs {
		if p.Source == nil || p.Destination == nil || p.InputChannels != 2 ||
			(p.Name != "speaker" && p.Name != "microphone") || seen[p.Name] ||
			(p.Name == "speaker" && (p.OutputChannels != 2 || p.Downmix == cfg.Headset)) ||
			(p.Name == "microphone" && (p.OutputChannels != 1 || p.Downmix)) {
			return fmt.Errorf("invalid or duplicate PCM pair %q", p.Name)
		}
		seen[p.Name] = true
	}
	if volume == nil {
		volume = func() (int, int) { return cfg.SpeakerVolume, cfg.MicrophoneVolume }
	}
	if stats == nil {
		stats = func(string, uint64) {}
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, len(pairs))
	for _, pair := range pairs {
		go func(p BridgePair) { done <- runPair(child, p, volume, stats, meter) }(pair)
	}
	var err error
	for range pairs {
		e := <-done
		if e != nil && !errors.Is(e, context.Canceled) {
			err = errors.Join(err, e)
		}
		cancel()
	}
	return err
}

func runPair(ctx context.Context, p BridgePair, volume func() (int, int), stats func(string, uint64), meter func(string, PCMLevels)) error {
	input := make([]byte, 480*p.InputChannels*2)
	var converted, matched []byte
	matcher := ClockMatcher{Channels: p.OutputChannels}
	budget := XRUNBudget{}
	fadeFrames := 240
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		failedSource := true
		n, err := p.Source.TransferContext(ctx, input)
		if err == nil && n != len(input) {
			err = io.ErrUnexpectedEOF
		}
		if err == nil {
			failedSource = false
			if p.Name == "microphone" && meter != nil {
				meter(p.Name, PCMLevels{InputPeak: peakPCM(input), CaptureOnly: true})
			}
			sv, mv := volume()
			gain := mv
			if p.Name == "speaker" {
				gain = sv
			}
			converted, err = ConvertPCMInto(converted, input, p.InputChannels, p.OutputChannels, gain, p.Downmix)
			if err == nil {
				var delay int64
				delay, err = p.Destination.Delay()
				if err == nil {
					var ppm float64
					ppm, err = RateCorrection(delay, audioTargetFrames)
					if err == nil {
						matched, err = matcher.ProcessInto(matched, converted, ppm)
					}
				}
			}
			if err == nil && len(matched) > 0 {
				if p.Name == "speaker" {
					fadeFrames = fadeInPCM(matched, p.OutputChannels, fadeFrames)
				}
				n, err = p.Destination.TransferContext(ctx, matched)
				if err == nil && n != len(matched) {
					err = io.ErrShortWrite
				}
				if err == nil {
					if p.Name == "microphone" && meter != nil {
						meter(p.Name, PCMLevels{InputPeak: peakPCM(input), OutputPeak: peakPCM(matched)})
					}
					stats(p.Name, uint64(n/(p.OutputChannels*2)))
					budget.StableBlock()
				}
			}
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if (errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ESTRPIPE)) && budget.Allow(time.Now()) {
			clear(input)
			clear(converted)
			clear(matched)
			matcher.Reset()
			fadeFrames = 240
			if failedSource {
				if e := p.Source.Drop(); e != nil {
					return fmt.Errorf("%s capture drop after XRUN: %w", p.Name, errors.Join(err, e))
				}
				if e := p.Source.Recover(); e != nil {
					return fmt.Errorf("%s capture recovery: %w", p.Name, e)
				}
			} else {
				if e := p.Destination.Drop(); e != nil {
					return fmt.Errorf("%s playback drop after XRUN: %w", p.Name, errors.Join(err, e))
				}
				if e := p.Destination.Recover(); e != nil {
					return fmt.Errorf("%s playback recovery: %w", p.Name, e)
				}
				if primer, ok := p.Destination.(interface {
					Prime(context.Context, int) error
				}); ok {
					if e := primer.Prime(ctx, audioTargetFrames); e != nil {
						return fmt.Errorf("%s playback re-prime: %w", p.Name, e)
					}
				}
			}
			continue
		}
		stage := "playback"
		if failedSource {
			stage = "capture"
		}
		return fmt.Errorf("%s %s bridge: %w", p.Name, stage, err)
	}
}

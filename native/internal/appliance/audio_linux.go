//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"perimode/native/internal/audio"
	"perimode/native/internal/safety"
)

// audioRuntime substitutes only device activity and the physical PCM boundary in
// offline coexistence runs. Native operation always uses the existing ALSA path.
type audioRuntime interface {
	Activity() (microphone, speaker bool, err error)
	Run(context.Context, audio.BridgeConfig, func() (int, int), func(string, uint64)) error
}
type audioMeterRuntime interface {
	RunWithMeter(context.Context, audio.BridgeConfig, func() (int, int), func(string, uint64), func(string, audio.PCMLevels)) error
}
type alsaRuntime struct{ transport *Transport }

func (a alsaRuntime) Activity() (bool, bool, error) { return a.transport.AudioActivity() }
func (a alsaRuntime) Run(ctx context.Context, cfg audio.BridgeConfig, volumes func() (int, int), count func(string, uint64)) error {
	return a.RunWithMeter(ctx, cfg, volumes, count, nil)
}
func (a alsaRuntime) RunWithMeter(ctx context.Context, cfg audio.BridgeConfig, volumes func() (int, int), count func(string, uint64), meter func(string, audio.PCMLevels)) error {
	cards, err := audio.DiscoverCards()
	if err != nil {
		return fmt.Errorf("ALSA card discovery: %w", err)
	}
	return audio.RunDuplexWithMeter(ctx, cards, cfg, volumes, count, meter)
}

type audioChannel struct {
	name       string
	cancel     context.CancelFunc
	done       chan error
	running    audio.BridgeConfig
	selected   bool
	retryAt    time.Time
	budget     safety.RetryBudget
	latched    bool
	stopFailed bool
	errorText  string // guarded by State.mu
	stable     int    // guarded by State.mu; 100 delivered blocks clear a fault
	hostActive time.Time
}

func (u *UI) audio(ctx context.Context, fatal func(error)) {
	backend := u.audioBackend
	native := backend == nil
	if backend == nil {
		if u.transport == nil {
			return
		}
		backend = alsaRuntime{transport: u.transport}
	}
	channels := [2]*audioChannel{{name: "speaker"}, {name: "microphone"}}
	for _, ch := range channels {
		ch.budget = safety.RetryBudget{Maximum: 3, Window: time.Minute}
	}
	updateErrorLocked := func() {
		speaker, microphone := channels[0].errorText, channels[1].errorText
		switch {
		case speaker != "" && microphone != "":
			u.state.AudioError = speaker + "; " + microphone
		case speaker != "":
			u.state.AudioError = speaker
		default:
			u.state.AudioError = microphone
		}
	}
	report := func(ch *audioChannel, e error) {
		if e == nil || errors.Is(e, context.Canceled) {
			return
		}
		u.state.mu.Lock()
		first := ch.errorText == ""
		if first {
			ch.errorText = e.Error()
			updateErrorLocked()
		}
		ch.stable = 0
		u.state.mu.Unlock()
		if first {
			u.state.Fault("audio", e)
		}
	}
	stop := func(ch *audioChannel) bool {
		if ch.stopFailed {
			return false
		}
		if ch.done == nil {
			return true
		}
		ch.cancel()
		select {
		case e := <-ch.done:
			report(ch, e)
			ch.done = nil
			return true
		case <-time.After(3 * time.Second):
			ch.stopFailed = true
			fatal(fmt.Errorf("%s audio shutdown stuck; descriptors retained until poweroff", ch.name))
			return false
		}
	}
	defer func() {
		for _, ch := range channels {
			stop(ch)
		}
	}()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	lastMic, lastSpeaker := false, false
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		u.state.mu.Lock()
		desired := audio.BridgeConfig{Speaker: u.state.SpeakerEnabled, Microphone: u.state.MicrophoneEnabled, SpeakerVolume: u.state.SpeakerVolume, MicrophoneVolume: u.state.MicrophoneVolume}
		paused := u.state.ThermalPaused
		u.state.mu.Unlock()
		requestedSpeaker, requestedMic := desired.Speaker, desired.Microphone
		mic, spk := lastMic, lastSpeaker
		if desired.Speaker || desired.Microphone {
			var e error
			mic, spk, e = backend.Activity()
			if e != nil {
				for _, ch := range channels {
					if (ch.name == "speaker" && desired.Speaker) || (ch.name == "microphone" && desired.Microphone) {
						report(ch, fmt.Errorf("%s audio USB activity: %w", ch.name, e))
					}
				}
				mic, spk = lastMic, lastSpeaker
			} else {
				lastMic, lastSpeaker = mic, spk
			}
		} else {
			lastMic, lastSpeaker = false, false
			mic, spk = false, false
		}
		desired.Speaker = desired.Speaker && spk && !paused
		desired.Microphone = desired.Microphone && mic && !paused
		output := "OFF"
		if desired.Speaker {
			output = "PHONE"
			if native {
				if jack, e := audio.HeadsetPresent(); e != nil {
					output = "PHONE / JACK UNKNOWN: " + e.Error()
				} else if jack {
					desired.Headset = true
					output = "3.5 MM"
				}
			}
		}
		u.state.mu.Lock()
		u.state.AudioOutput = output
		u.state.mu.Unlock()
		for _, ch := range channels {
			selected := requestedSpeaker
			active := desired.Speaker
			if ch.name == "microphone" {
				selected = requestedMic
				active = desired.Microphone
			}
			if active {
				ch.hostActive = time.Now()
			} else if selected && !paused && ch.done != nil && time.Since(ch.hostActive) < 300*time.Millisecond {
				// A brief host alt-setting transition must not cycle the analog
				// route. Explicit disable and thermal pause still stop immediately.
				active = true
			}
			// Explicit toggle re-arms only the selected direction. The other
			// channel keeps its owner, mixer route and retry budget.
			if selected != ch.selected {
				ch.selected = selected
				ch.latched = false
				ch.retryAt = time.Time{}
				ch.budget = safety.RetryBudget{Maximum: 3, Window: time.Minute}
				u.state.mu.Lock()
				ch.errorText = ""
				ch.stable = 0
				updateErrorLocked()
				u.state.mu.Unlock()
			}
			if ch.done != nil && (!active || (ch.name == "speaker" && desired.Headset != ch.running.Headset)) {
				if !stop(ch) {
					return
				}
				continue
			}
			if ch.done != nil {
				select {
				case e := <-ch.done:
					ch.done = nil
					ch.cancel()
					if e == nil {
						e = errors.New(ch.name + " audio bridge stopped unexpectedly")
					}
					report(ch, e)
					delay, ok := ch.budget.Next(time.Now())
					ch.latched = !ok
					ch.retryAt = time.Now().Add(delay)
				default:
				}
			}
			if ch.done != nil || ch.latched || !active || time.Now().Before(ch.retryAt) || ctx.Err() != nil {
				continue
			}
			worker, cancel := context.WithCancel(ctx)
			ch.cancel = cancel
			ch.running = audio.BridgeConfig{Speaker: ch.name == "speaker", Microphone: ch.name == "microphone", Headset: ch.name == "speaker" && desired.Headset, SpeakerVolume: desired.SpeakerVolume, MicrophoneVolume: desired.MicrophoneVolume}
			ch.done = make(chan error, 1)
			result := ch.done
			u.state.mu.Lock()
			ch.stable = 0
			if ch.name == "speaker" {
				u.state.AudioSpeakerLast = time.Time{}
			} else {
				u.state.AudioMicrophoneLast = time.Time{}
				u.state.AudioMicrophoneInputPeak = 0
				u.state.AudioMicrophoneOutputPeak = 0
				u.state.AudioMicrophonePeakAt = time.Time{}
			}
			u.state.mu.Unlock()
			go func(channel *audioChannel, cfg audio.BridgeConfig) {
				volumes := func() (int, int) {
					u.state.mu.Lock()
					defer u.state.mu.Unlock()
					return u.state.SpeakerVolume, u.state.MicrophoneVolume
				}
				count := func(name string, n uint64) {
					if name != channel.name || n == 0 {
						return
					}
					u.state.mu.Lock()
					defer u.state.mu.Unlock()
					if channel.stable < 100 {
						channel.stable++
					}
					if channel.stable == 100 && channel.errorText != "" {
						channel.errorText = ""
						updateErrorLocked()
					}
					if name == "speaker" {
						u.state.AudioSpeakerFrames += n
						u.state.AudioSpeakerLast = time.Now()
					} else {
						u.state.AudioMicrophoneFrames += n
						u.state.AudioMicrophoneLast = time.Now()
					}
				}
				if metered, ok := backend.(audioMeterRuntime); ok {
					result <- metered.RunWithMeter(worker, cfg, volumes, count, func(name string, levels audio.PCMLevels) {
						if name != "microphone" {
							return
						}
						u.state.mu.Lock()
						u.state.AudioMicrophoneInputPeak = levels.InputPeak
						if !levels.CaptureOnly {
							u.state.AudioMicrophoneOutputPeak = levels.OutputPeak
						}
						u.state.AudioMicrophonePeakAt = time.Now()
						u.state.mu.Unlock()
					})
				} else {
					result <- backend.Run(worker, cfg, volumes, count)
				}
			}(ch, ch.running)
		}
	}
}

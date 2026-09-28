//go:build linux && (amd64 || arm64)

package audio

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"perimode/native/internal/linuxio"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type Cards struct{ Physical, Gadget int }

type mixerLease struct {
	tx       *Transaction
	controls map[string]bool
}

type mixerOwner struct {
	mu       sync.Mutex
	card     int
	mixer    *Mixer
	defaults *Transaction
	active   map[string]mixerLease
}

// Keep the baseline mixer transaction alive while either independent route runs.
var nativeMixer mixerOwner

func (m *mixerOwner) acquire(card int, cfg BridgeConfig) (func() error, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := "microphone"
	if cfg.Speaker {
		key = "speaker"
	}
	if cfg.Speaker && cfg.Microphone {
		key = "duplex"
	}
	if m.mixer != nil && m.card != card {
		return nil, fmt.Errorf("physical ALSA card changed while audio active")
	}
	if _, exists := m.active[key]; exists {
		return nil, fmt.Errorf("%s ALSA route is already active", key)
	}
	created := false
	if m.mixer == nil {
		mix, err := OpenMixer(card)
		if err != nil {
			return nil, fmt.Errorf("physical ALSA mixer card%d: %w", card, err)
		}
		defaults, err := routeWithDefaults(nil)
		if err != nil {
			_ = mix.Close()
			return nil, err
		}
		baseline, err := mix.Apply(defaults)
		if err != nil {
			_ = mix.Close()
			return nil, fmt.Errorf("physical ALSA defaults: %w", err)
		}
		m.card, m.mixer, m.defaults = card, mix, baseline
		m.active = make(map[string]mixerLease)
		created = true
	}
	cleanup := func(err error) (func() error, error) {
		if created {
			err = errors.Join(err, m.defaults.Restore(), m.mixer.Close())
			m.mixer, m.defaults, m.active = nil, nil, nil
		}
		return nil, err
	}
	plan, err := ExpandRoute(VendorMixer, routeNames(cfg), false)
	if err != nil {
		return cleanup(err)
	}
	if cfg.Microphone {
		// Cold native boot has no Android mixer initialization. Keep capture at
		// unity here; the user volume (20% by default) supplies attenuation.
		for _, level := range []RouteElement{
			{XMLName: xml.Name{Local: "ctl"}, Name: "IN1R Digital Volume", Value: "128"},
			{XMLName: xml.Name{Local: "ctl"}, Name: "LHPF1 Input 1 Volume", Value: "32"},
			{XMLName: xml.Name{Local: "ctl"}, Name: "AIF1TX1 Input 1 Volume", Value: "32"},
			{XMLName: xml.Name{Local: "ctl"}, Name: "AIF1TX2 Input 1 Volume", Value: "32"},
		} {
			found := false
			for i := range plan {
				if plan[i].Name == level.Name {
					plan[i] = level
					found = true
				}
			}
			if !found {
				plan = append(plan, level)
			}
		}
	}
	controls := make(map[string]bool, len(plan))
	for _, el := range plan {
		controls[el.Name] = true
		for activeName, lease := range m.active {
			if lease.controls[el.Name] {
				return cleanup(fmt.Errorf("%s and %s routes share mixer control %s", key, activeName, el.Name))
			}
		}
	}
	tx, err := m.mixer.Apply(plan)
	if err != nil {
		return cleanup(fmt.Errorf("%s ALSA route: %w", key, err))
	}
	m.active[key] = mixerLease{tx: tx, controls: controls}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			lease := m.active[key]
			delete(m.active, key)
			releaseErr = lease.tx.Restore()
			if len(m.active) == 0 {
				releaseErr = errors.Join(releaseErr, m.defaults.Restore(), m.mixer.Close())
				m.mixer, m.defaults, m.active = nil, nil, nil
			}
		})
		return releaseErr
	}, nil
}

func DiscoverCards() (Cards, error) {
	out := Cards{Physical: -1, Gadget: -1}
	paths, err := filepath.Glob("/sys/class/sound/card[0-9]*")
	if err != nil {
		return out, fmt.Errorf("enumerate ALSA cards: %w", err)
	}
	seen := make([]string, 0, len(paths))
	for _, p := range paths {
		n, e := strconv.Atoi(strings.TrimPrefix(filepath.Base(p), "card"))
		if e != nil || n > 15 {
			continue
		}
		id, e := linuxio.ReadText(fmt.Sprintf("/proc/asound/card%d/id", n))
		if e != nil {
			continue
		}
		seen = append(seen, fmt.Sprintf("card%d=%s", n, strings.TrimSpace(id)))
		lower := strings.ToLower(id)
		if strings.TrimSpace(lower) == "s7audio" || strings.Contains(lower, "uac2") || strings.Contains(lower, "gadget") {
			if out.Gadget >= 0 {
				return out, fmt.Errorf("ambiguous USB audio card")
			}
			out.Gadget = n
			continue
		}
		m, e := OpenMixer(n)
		if e != nil {
			continue
		}
		_, _, a := m.Read("SPK Switch")
		_, _, b := m.Read("Main Mic Switch")
		_, _, c := m.Read("DSP5 Rate")
		_ = m.Close()
		if a == nil && b == nil && c == nil {
			if out.Physical >= 0 {
				return out, fmt.Errorf("ambiguous phone audio card")
			}
			out.Physical = n
		}
	}
	if out.Physical < 0 || out.Gadget < 0 {
		return out, fmt.Errorf("native physical/UAC2 ALSA card missing: %+v; detected %v", out, seen)
	}
	return out, nil
}

type BridgeConfig struct {
	Speaker, Microphone, Headset    bool
	SpeakerVolume, MicrophoneVolume int
}

const (
	audioPeriodFrames  = 480
	audioBufferPeriods = 8
	audioTargetFrames  = 4 * audioPeriodFrames
)

func parseHeadsetState(value string) (bool, error) {
	switch strings.TrimSpace(value) {
	case "0":
		return false, nil
	case "1", "2":
		return true, nil
	default:
		return false, fmt.Errorf("unexpected h2w state %q", strings.TrimSpace(value))
	}
}

func HeadsetPresent() (bool, error) {
	value, err := linuxio.ReadText("/sys/class/switch/h2w/state")
	if err != nil {
		return false, err
	}
	return parseHeadsetState(value)
}

func routeNames(cfg BridgeConfig) []string {
	routes := []string{}
	if cfg.Speaker {
		// The signal route and its gain route are separate in Samsung's XML.
		// Apply both so cold boot never inherits an arbitrary Android level.
		if cfg.Headset {
			routes = append(routes, "media-headset", "gain-media-headset")
		} else {
			routes = append(routes, "media-speaker", "gain-media-speaker")
		}
	}
	if cfg.Microphone {
		routes = append(routes, "media-mic", "gain-media-mic")
	}
	return routes
}

func applianceRoute(cfg BridgeConfig) ([]RouteElement, error) {
	return routeWithDefaults(routeNames(cfg))
}

func routeWithDefaults(names []string) ([]RouteElement, error) {
	defaults, err := ExpandRoute(VendorMixer, nil, true)
	if err != nil {
		return nil, err
	}
	selected, err := ExpandRoute(VendorMixer, names, false)
	if err != nil {
		return nil, err
	}
	// Skip volatile headset controls in generic defaults. An explicit headset
	// route still carries its own impedance settings for the 3.5 mm output.
	out := defaults[:0]
	for _, el := range defaults {
		if el.Name != "HPOUT1L Impedance Volume" && el.Name != "HPOUT1R Impedance Volume" {
			out = append(out, el)
		}
	}
	return append(out, selected...), nil
}

func (c BridgeConfig) Validate() error {
	if c.SpeakerVolume < 0 || c.SpeakerVolume > 100 || c.MicrophoneVolume < 0 || c.MicrophoneVolume > 100 {
		return fmt.Errorf("audio volume must be 0..100")
	}
	return nil
}

// RunDuplex owns its PCM descriptors until transfers finish. Any stream failure
// cancels only this audio bridge, not USB gadget/monitor. Bounded retries are the
// caller's responsibility; there is deliberately no hidden infinite restart.
func RunDuplex(ctx context.Context, cards Cards, cfg BridgeConfig, volume func() (int, int), stats func(string, uint64)) (err error) {
	return RunDuplexWithMeter(ctx, cards, cfg, volume, stats, nil)
}

func RunDuplexWithMeter(ctx context.Context, cards Cards, cfg BridgeConfig, volume func() (int, int), stats func(string, uint64), meter func(string, PCMLevels)) (err error) {
	if err = cfg.Validate(); err != nil {
		return err
	}
	if !cfg.Speaker && !cfg.Microphone {
		return nil
	}
	release, e := nativeMixer.acquire(cards.Physical, cfg)
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, release()) }()
	// Vendor pcmdai: playback_link=6, capture_link=0. Source is included unchanged.
	pairs := []BridgePair{}
	defer func() {
		for _, p := range pairs {
			err = errors.Join(err, p.Source.(*PCM).Close(), p.Destination.(*PCM).Close())
		}
	}()
	for _, speaker := range []bool{true, false} {
		if speaker && !cfg.Speaker || !speaker && !cfg.Microphone {
			continue
		}
		name := "microphone"
		sourceCard, sourceDevice, destCard, destDevice := cards.Physical, 0, cards.Gadget, 0
		in, out := 2, 1
		if speaker {
			name = "speaker"
			sourceCard, sourceDevice, destCard, destDevice = cards.Gadget, 0, cards.Physical, 6
			in, out = 2, 2
		}
		sc := PCMConfig{Channels: uint32(in), Rate: 48000, Period: audioPeriodFrames, Periods: audioBufferPeriods}
		dc := sc
		dc.Channels = uint32(out)
		src, e := OpenPCM(sourceCard, sourceDevice, true, sc)
		if e != nil {
			return fmt.Errorf("%s input: %w", name, e)
		}
		dst, e := OpenPCM(destCard, destDevice, false, dc)
		if e != nil {
			_ = src.Close()
			return fmt.Errorf("%s output: %w", name, e)
		}
		if e = dst.Prime(ctx, audioTargetFrames); e != nil {
			_ = src.Close()
			_ = dst.Close()
			return fmt.Errorf("%s playback prime: %w", name, e)
		}
		pairs = append(pairs, BridgePair{Source: src, Destination: dst, Name: name, InputChannels: in, OutputChannels: out, Downmix: speaker && !cfg.Headset})
	}
	return RunPairsWithMeter(ctx, pairs, cfg, volume, stats, meter)
}

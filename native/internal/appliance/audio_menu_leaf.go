package appliance

import (
	"fmt"
	"time"
)

// Audio navigation is owned by the middle column. Only leaf settings expose
// controls in the right column.
func (u *UI) audioLeafLines(page string) ([]string, bool) {
	return u.audioLeafLinesAt(page, time.Now())
}

func (u *UI) audioLeafLinesAt(page string, now time.Time) ([]string, bool) {
	u.state.mu.Lock()
	spk, mic := u.state.SpeakerEnabled, u.state.MicrophoneEnabled
	sv, mv := u.state.SpeakerVolume, u.state.MicrophoneVolume
	speakerFrames, micFrames := u.state.AudioSpeakerFrames, u.state.AudioMicrophoneFrames
	speakerLast, micLast := u.state.AudioSpeakerLast, u.state.AudioMicrophoneLast
	output := u.state.AudioOutput
	inputPeak, usbPeak := u.state.AudioMicrophoneInputPeak, u.state.AudioMicrophoneOutputPeak
	u.state.mu.Unlock()

	switch page {
	case "AUDIO":
		return []string{"AUDIO", "SPEAKER", "MICROPHONE", "STATISTICS", "BACK: CLOSE MENU"}, true
	case "SPEAKER":
		lines := []string{"SPEAKER", fmt.Sprintf("ENABLED: %t", spk)}
		if spk {
			lines = append(lines, fmt.Sprintf("VOLUME: %d%%", sv))
		}
		return append(lines, "BACK: AUDIO"), true
	case "MICROPHONE":
		lines := []string{"MICROPHONE", fmt.Sprintf("ENABLED: %t", mic)}
		if mic {
			lines = append(lines, fmt.Sprintf("VOLUME: %d%%", mv))
		}
		return append(lines, "BACK: AUDIO"), true
	case "AUDIO_STAT":
		lines := []string{
			"Statistics",
			"FUNCTION: AUDIO",
			"SPEAKER: " + audioActivity(spk, speakerLast, now),
			"MICROPHONE: " + audioActivity(mic, micLast, now),
			"OUTPUT: " + output,
		}
		if spk {
			lines = append(lines, fmt.Sprintf("SPEAKER VOLUME: %d%%", sv))
			if speakerFrames > 0 {
				lines = append(lines, fmt.Sprintf("SPEAKER FRAMES: %d", speakerFrames))
			}
		}
		if mic {
			lines = append(lines, fmt.Sprintf("MICROPHONE VOLUME: %d%%", mv))
			lines = append(lines, fmt.Sprintf("MIC INPUT PEAK: %d", inputPeak), fmt.Sprintf("MIC USB PEAK: %d", usbPeak))
			if micFrames > 0 {
				lines = append(lines, fmt.Sprintf("MICROPHONE FRAMES: %d", micFrames))
			}
		}
		return append(lines, "BACK: AUDIO"), true
	}
	return nil, false
}

func audioActivity(enabled bool, last, now time.Time) string {
	if !enabled {
		return "OFF"
	}
	if !last.IsZero() && !last.After(now) && now.Sub(last) <= 2*time.Second {
		return "RECENT FRAMES"
	}
	return "WAITING FOR FRAMES"
}

func (u *UI) audioLeafDetail(page string, row int, label, value string) (menuDetail, bool) {
	switch page {
	case "AUDIO":
		if row == 1 || row == 2 {
			volume := u.audioLevel(row == 2)
			d := sliderDetail(label, volume, 0, 100, 1)
			if volume == 0 {
				d.SliderText = "Disabled"
			}
			return d, true
		}
		if row == 3 {
			return menuDetail{Title: label}, true
		}
	case "SPEAKER", "MICROPHONE":
		if row == 1 {
			u.state.mu.Lock()
			on := u.state.SpeakerEnabled
			if page == "MICROPHONE" {
				on = u.state.MicrophoneEnabled
			}
			u.state.mu.Unlock()
			return switchDetail("ENABLED", on), true
		}
		if row == 2 {
			u.state.mu.Lock()
			on, volume := u.state.SpeakerEnabled, u.state.SpeakerVolume
			if page == "MICROPHONE" {
				on, volume = u.state.MicrophoneEnabled, u.state.MicrophoneVolume
			}
			u.state.mu.Unlock()
			if on {
				return sliderDetail("VOLUME", volume, 0, 100, 1), true
			}
		}
	case "AUDIO_STAT":
		return menuDetail{}, true
	}
	return menuDetail{}, false
}

func (u *UI) selectAudioLeaf(page string, row, option int) bool {
	if (page != "SPEAKER" && page != "MICROPHONE") || row != 1 || option < 0 || option > 1 {
		return false
	}
	u.setAudioEnabled(page == "MICROPHONE", option == 1)
	u.draw()
	return true
}

func (u *UI) applyAudioLeafSlider(page string, row, value int) bool {
	if page == "AUDIO" && (row == 1 || row == 2) && value >= 0 && value <= 100 {
		u.setAudioLevel(row == 2, value)
		return true
	}
	if (page != "SPEAKER" && page != "MICROPHONE") || row != 2 || value < 0 || value > 100 {
		return false
	}
	u.setAudioLevel(page == "MICROPHONE", value)
	return true
}

func (u *UI) audioLevel(microphone bool) int {
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	if microphone {
		if u.state.MicrophoneEnabled {
			return u.state.MicrophoneVolume
		}
		return 0
	}
	if u.state.SpeakerEnabled {
		return u.state.SpeakerVolume
	}
	return 0
}

func (u *UI) setAudioLevel(microphone bool, value int) {
	value = max(0, min(100, value))
	u.state.mu.Lock()
	on := u.state.SpeakerEnabled
	if microphone {
		on = u.state.MicrophoneEnabled
		u.state.MicrophoneVolume = value
		u.state.MicrophoneEnabled = value > 0
	} else {
		u.state.SpeakerVolume = value
		u.state.SpeakerEnabled = value > 0
	}
	if on != (value > 0) {
		u.state.AudioRetrySerial++
		u.state.AudioError = ""
	}
	u.state.mu.Unlock()
	if !on && value > 0 && u.transport != nil && !u.transport.AudioProvisioned() {
		u.async(func() error { return u.transport.EnsureAudio() })
	}
	u.draw()
}

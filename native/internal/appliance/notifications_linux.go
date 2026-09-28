//go:build linux && (amd64 || arm64)

package appliance

import "time"

func (u *UI) notify(text string) {
	u.noticeText = text
	u.noticeUntil = time.Now().Add(2200 * time.Millisecond)
	u.menuNeedsDraw = true
}

func (u *UI) refreshNotices(now time.Time) {
	if !u.noticeUntil.IsZero() && !now.Before(u.noticeUntil) {
		u.noticeText = ""
		u.noticeUntil = time.Time{}
		u.menuNeedsDraw = true
	}
	u.state.mu.Lock()
	flags := [4]bool{u.state.Settings.Enabled, u.state.Camera.Enabled, u.state.SpeakerEnabled, u.state.MicrophoneEnabled}
	u.state.mu.Unlock()
	if u.noticeFlagsReady {
		for i, on := range flags {
			if on != u.noticeFlags[i] {
				text := []string{"MONITOR", "CAMERA", "SPEAKER", "MICROPHONE"}[i]
				if on {
					text += " ON"
				} else {
					text += " OFF"
				}
				u.notify(text)
			}
		}
	}
	u.noticeFlags, u.noticeFlagsReady = flags, true
}

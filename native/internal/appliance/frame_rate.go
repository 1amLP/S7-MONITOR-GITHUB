package appliance

import "time"

type FrameRate struct {
	Received  float64   `json:"received_fps"`
	Decoded   float64   `json:"decoded_fps"`
	Presented float64   `json:"drawn_fps"`
	Preview   float64   `json:"preview_updates_fps"`
	WindowMS  int64     `json:"window_ms"`
	At        time.Time `json:"at"`
}
type frameRateWindow struct {
	start                        time.Time
	generation                   uint32
	received, decoded, presented uint64
	preview                      uint64
	value                        FrameRate
}

func (s *State) sampleFrameRate(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := &s.frameRate
	if w.start.IsZero() || w.generation != s.Generation || now.Before(w.start) {
		*w = frameRateWindow{start: now, generation: s.Generation, received: s.Received, decoded: s.Decoded, presented: s.Blitted, preview: s.Preview.Copied}
		return
	}
	elapsed := now.Sub(w.start)
	if elapsed < time.Second {
		return
	}
	if s.Received >= w.received && s.Decoded >= w.decoded && s.Blitted >= w.presented {
		w.value = FrameRate{Received: float64(s.Received-w.received) / elapsed.Seconds(), Decoded: float64(s.Decoded-w.decoded) / elapsed.Seconds(), Presented: float64(s.Blitted-w.presented) / elapsed.Seconds(), WindowMS: elapsed.Milliseconds(), At: now}
		if s.Preview.Copied >= w.preview {
			w.value.Preview = float64(s.Preview.Copied-w.preview) / elapsed.Seconds()
		}
	}
	w.start = now
	w.received = s.Received
	w.decoded = s.Decoded
	w.presented = s.Blitted
	w.preview = s.Preview.Copied
}
func (s *State) FrameRate() FrameRate { s.mu.Lock(); defer s.mu.Unlock(); return s.frameRate.value }

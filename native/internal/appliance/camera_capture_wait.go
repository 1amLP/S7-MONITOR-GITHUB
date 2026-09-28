package appliance

import "time"

// Preview's last reader has up to two seconds to release its DMA session.
// Waiting for that transition is not an encoder/capture failure retry.
type cameraCaptureWait struct {
	epoch uint64
	start time.Time
}

func (w *cameraCaptureWait) pending(epoch uint64, now time.Time) bool {
	if w.start.IsZero() || w.epoch != epoch {
		w.epoch, w.start = epoch, now
	}
	return now.Sub(w.start) < 3*time.Second
}

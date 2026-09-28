package decon

import "time"

// Direct layers already wait for the preceding hardware retire fence before
// another submission can complete. An independent timer can miss the next TE.
func presentationDelay(hardwareLayers bool, deadline, now time.Time) time.Duration {
	if hardwareLayers || !deadline.After(now) {
		return 0
	}
	return deadline.Sub(now)
}

// Keep the 60 Hz phase across small scheduler overshoots. Reset after a full
// missed period instead of submitting a catch-up burst of obsolete frames.
func nextPresentationDeadline(previous, now time.Time) time.Time {
	const period = time.Second / 60
	if previous.IsZero() || !now.Before(previous.Add(period)) {
		return now.Add(period)
	}
	return previous.Add(period)
}

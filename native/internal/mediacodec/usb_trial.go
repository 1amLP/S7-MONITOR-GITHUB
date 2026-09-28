//go:build linux && (amd64 || arm64)

package mediacodec

import (
	"context"
	"fmt"
	"perimode/native/internal/media"
	"time"
)

const USBTrialLimit = 30 * time.Second
const flagUSBTrial uint32 = 1 << 8

func validateUSBTrial(ctx context.Context, c media.EncodeSettings) error {
	if ctx == nil {
		return fmt.Errorf("USB trial context required")
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := c.Validate(); e != nil {
		return e
	}
	if c.Width != 1280 || c.Height != 720 || (c.FPS != 120 && c.FPS != 240) || c.GOP%c.FPS != 0 {
		return fmt.Errorf("USB trial requires exact 720p120/240 and whole-second GOP")
	}
	end, ok := ctx.Deadline()
	if !ok || time.Until(end) > USBTrialLimit {
		return fmt.Errorf("USB trial requires a deadline of at most 30 seconds")
	}
	return nil
}

// OpenUSBTrialEncoder has no effect on ordinary OpenEncoder admission. This
// requires a bounded session and an explicit C-worker command flag. The vendor
// component must accept the exact FPS/profile; there is no lower-rate fallback.
func OpenUSBTrialEncoder(ctx context.Context, c media.EncodeSettings) (*Encoder, error) {
	if e := validateUSBTrial(ctx, c); e != nil {
		return nil, e
	}
	s, e := nativeStart(ctx, roleEncode)
	if e != nil {
		return nil, e
	}
	return openEncoderFlags(s, c, flagUSBTrial)
}

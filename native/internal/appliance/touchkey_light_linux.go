//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"fmt"
	"os"
	"time"

	"perimode/native/internal/linuxio"
)

const touchKeyBrightness = "/sys/class/sec/sec_touchkey/brightness"

func touchKeyLight(ctx context.Context, pulses <-chan struct{}, hold time.Duration, write func(bool) error) error {
	if hold < 10*time.Millisecond || write == nil {
		return fmt.Errorf("invalid touchkey light worker")
	}
	timer := time.NewTimer(hold)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	lit := false
	for {
		select {
		case <-ctx.Done():
			if lit {
				return write(false)
			}
			return ctx.Err()
		case <-pulses:
			if !lit {
				if err := write(true); err != nil {
					return err
				}
				lit = true
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(hold)
		case <-timer.C:
			if lit {
				if err := write(false); err != nil {
					return err
				}
				lit = false
			}
		}
	}
}

func (u *UI) startTouchKeyLight(ctx context.Context) error {
	if _, err := os.Stat(touchKeyBrightness); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	u.touchKeyPulse = make(chan struct{}, 1)
	return u.workers.Start("touchkey-light", func(ctx context.Context) error {
		err := touchKeyLight(ctx, u.touchKeyPulse, time.Second, func(on bool) error {
			value := "0\n"
			if on {
				value = "1\n"
			}
			return linuxio.WriteAttr(touchKeyBrightness, value)
		})
		if err != nil && ctx.Err() == nil {
			u.state.Error(err)
		}
		return err
	})
}

func (u *UI) pulseTouchKey() {
	if u.touchKeyPulse == nil {
		return
	}
	select {
	case u.touchKeyPulse <- struct{}{}:
	default:
	}
}

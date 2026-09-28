//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"perimode/native/internal/safety"
)

func chargerBoot(cmdline string) bool {
	for _, field := range strings.Fields(cmdline) {
		// Matches Samsung sec_batt.c, including bootloader charger suffixes.
		if strings.HasPrefix(field, "androidboot.mode=charger") {
			return true
		}
	}
	return false
}

// Samsung skips touch drivers in lpcharge. Starting the appliance here would
// expose a working USB device with dead local controls. Power boots normally.
func runCharging(parent context.Context, health *safety.Liveness) error {
	ctx, cancel := context.WithCancel(parent)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	if err := (powerIO{read: read, write: writeAttr}).charge80(); err != nil {
		return fmt.Errorf("charger-only 50/80 policy: %w", err)
	}
	if err := writeAttr("/sys/class/graphics/fb0/blank", "4"); err != nil {
		return fmt.Errorf("charger-only screen off: %w", err)
	}
	keys := make(chan uint16, 1)
	errors := make(chan error, 1)
	report := func(err error) {
		select {
		case errors <- err:
		default:
		}
	}
	count := StartInputsTracked(ctx, false, nil, func(key uint16) {
		if key == localPowerHold {
			select {
			case keys <- key:
			default:
			}
		}
	}, nil, func(InputFrame) {}, report, func(_ string, run func(context.Context) error) error {
		workers.Add(1)
		go func() { defer workers.Done(); _ = run(ctx) }()
		return nil
	})
	if count == 0 {
		return fmt.Errorf("charger-only power key unavailable")
	}
	log.Print("S7 charger-only: screen/USB/cameras off; charge 50/80; hold Power for normal boot")
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	guard := safety.Guard{}
	offline := 0
	for {
		readings, err := Thermal()
		now := time.Now()
		sample := safety.Sample{At: now, Valid: err == nil}
		for _, r := range readings {
			if r.IsSoC() {
				sample.CPU = max(sample.CPU, r.MilliC)
			}
			if r.Role == "BATTERY" {
				sample.Battery = r.MilliC
			}
		}
		if d := guard.Evaluate(now, sample); d.Action == safety.Stop {
			return fmt.Errorf("charger-only thermal guard: %s (%v)", d.Reason, err)
		}
		health.Thermal()
		health.UI()
		connected, known := false, true
		for _, name := range []string{"ac", "usb", "wireless"} {
			v, e := read("/sys/class/power_supply/" + name + "/online")
			known = known && e == nil && (v == "0" || v == "1")
			connected = connected || v == "1"
		}
		if known && !connected {
			offline++
		} else {
			offline = 0
		}
		if offline >= 4 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errors:
			return err
		case <-keys:
			return ErrRebootRequested
		case <-tick.C:
		}
	}
}

//go:build linux && (amd64 || arm64)

package media

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type EncodeTrial struct {
	Source                               string         `json:"source"`
	SensorTested                         bool           `json:"sensor_tested"`
	Device                               string         `json:"device"`
	Settings                             EncodeSettings `json:"settings"`
	Submitted, Encoded, Keyframes, Bytes uint64
	Seconds                              float64
}

// RunEncodeTrial is a local synthetic-input diagnostic, never a webcam. This
// keeps the native encoder callable without pretending FIMC-IS is implemented.
// It does not bind/rebind USB or write captured media to any partition.
func RunEncodeTrial(ctx context.Context, path string, safe func() error) (out EncodeTrial, err error) {
	out = EncodeTrial{Source: "synthetic NV12; NOT a sensor", SensorTested: false, Device: path, Settings: EncodeSettings{1280, 720, 30, 8000000, 30}}
	if safe == nil {
		return out, fmt.Errorf("encoder test requires thermal guard")
	}
	if err = safe(); err != nil {
		return out, err
	}
	enc, err := OpenEncoder(path, out.Settings)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, enc.Close()) }()
	y, uv := make([]byte, 1280*720), make([]byte, 1280*360)
	for i := range y {
		y[i] = 32
	}
	for i := range uv {
		uv[i] = 128
	}
	im := Image{Width: 1280, Height: 720, StrideY: 1280, StrideUV: 1280, Y: y, UV: uv}
	begin := time.Now()
	next := begin
	lastStripe := -1
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for time.Since(begin) < 3*time.Second {
		if err = safe(); err != nil {
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-tick.C:
		}
		_, err = enc.Drain(func(packet Encoded) error {
			out.Encoded++
			out.Bytes += uint64(len(packet.Data))
			if packet.Key {
				out.Keyframes++
			}
			return nil
		})
		if err != nil {
			return out, err
		}
		if out.Submitted >= 60 {
			if out.Encoded >= out.Submitted {
				break
			}
			continue
		}
		if time.Now().Before(next) {
			continue
		}
		if lastStripe >= 0 {
			for row := 0; row < 720; row++ {
				for x := 0; x < 8; x++ {
					y[row*1280+lastStripe+x] = 32
				}
			}
		}
		stripe := int(out.Submitted*16) % 1264
		for row := 0; row < 720; row++ {
			for x := 0; x < 8; x++ {
				y[row*1280+stripe+x] = 200
			}
		}
		lastStripe = stripe
		im.PTS = int64(out.Submitted) * 1_000_000 / 30
		if err = enc.Submit(im); err == nil {
			out.Submitted++
			next = begin.Add(time.Duration(out.Submitted) * time.Second / 30)
		} else if !IsRetry(err) {
			return out, err
		}
	}
	out.Seconds = time.Since(begin).Seconds()
	if out.Encoded == 0 || out.Keyframes == 0 {
		return out, fmt.Errorf("encoder returned no valid H264 IDR frames")
	}
	return out, nil
}

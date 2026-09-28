package camera

import (
	"errors"
	"fmt"
	"time"

	"perimode/native/internal/media"
)

// One complete AU can survive a short full USB queue. This is not a queue of
// interdependent H264 frames. Expiry drops the AU and requires a new IDR.
// No attempt is made to replay a partially accepted USB transfer or an EIO.
const usbRetryWindow = 100 * time.Millisecond
const maxRetryAccessUnit = 4 << 20

var errPauseEncodedDrain = errors.New("camera: pause drain while USB owns retry slot")

type outputRetry struct {
	packet media.Encoded
	bytes  []byte
	since  time.Time
	active bool
}

func (r *outputRetry) retain(packet media.Encoded, now time.Time) error {
	if r.active || len(packet.Data) < 4 || len(packet.Data) > maxRetryAccessUnit {
		return fmt.Errorf("invalid/busy bounded camera retry slot")
	}
	if cap(r.bytes) < len(packet.Data) {
		r.bytes = make([]byte, len(packet.Data))
	}
	r.bytes = r.bytes[:len(packet.Data)]
	copy(r.bytes, packet.Data) // never retain codec/driver callback storage
	r.packet = media.Encoded{Data: r.bytes, PTS: packet.PTS, Key: packet.Key}
	r.since, r.active = now, true
	return nil
}
func (r *outputRetry) clear() {
	r.packet = media.Encoded{}
	r.since, r.active = time.Time{}, false
}
func (p *Pipeline) acceptedUSB(packet media.Encoded, now time.Time) {
	p.stats.USBQueued++
	p.stats.USBBytes += uint64(len(packet.Data))
	p.lastProgress = now
	if packet.Key {
		p.needKey, p.keyRequested = false, false
	}
}

func (p *Pipeline) retryUSB(now time.Time) (blocked bool, err error) {
	r := &p.retry
	if !r.active {
		return false, nil
	}
	age := now.Sub(r.since)
	if age < 0 {
		return false, fmt.Errorf("camera retry clock regressed")
	}
	if us := uint64(age / time.Microsecond); us > p.stats.MaxUSBRetryMicros {
		p.stats.MaxUSBRetryMicros = us
	}
	if age >= usbRetryWindow {
		r.clear()
		p.stats.USBPendingFrames = 0
		p.stats.USBRetryExpired++
		p.stats.EncodedDropped++
		p.needKey, p.keyRequested = true, false
		return false, nil
	}
	p.stats.USBRetryAttempts++
	if err := p.sink.Submit(r.packet); err != nil {
		if wouldBlock(err) {
			return true, nil
		}
		return false, err
	}
	p.acceptedUSB(r.packet, now)
	p.stats.USBRetryRecovered++
	p.stats.USBPendingFrames = 0
	r.clear()
	return false, nil
}

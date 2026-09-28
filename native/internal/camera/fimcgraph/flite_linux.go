//go:build linux && (amd64 || arm64)

package fimcgraph

import (
	"errors"
	"fmt"
	"sync"
)

// StreamQueue is the already configured/allocated FLITE queue. Start must not
// allocate memory or substitute a sensor profile. Stop confirms DMA is stopped.
// The caller owns both this queue and the associated physical-sensor descriptor.
type StreamQueue interface {
	Start() error
	Stop() error
}

// FliteSwitch completes the stream ordering used by the supplied HAL:
// FLITE STREAMON -> SENSOR_STREAM(1); SENSOR_STREAM(0) -> FLITE STREAMOFF.
// It is a SensorSwitch for Session, so downstream queues start before it and
// cannot be torn down until this switch has confirmed sensor stop. This does
// NOT load a setfile, configure companion/sensor inputs or supply sensor buffers.
// The objects passed here must refer to the SAME configured physical sensor.
type FliteSwitch struct {
	mu                          sync.Mutex
	queue                       StreamQueue
	sensor                      SensorSwitch
	queueTouched, sensorTouched bool
	on, uncertain, closed       bool
}

func NewFliteSwitch(queue StreamQueue, sensor SensorSwitch) (*FliteSwitch, error) {
	if queue == nil || sensor == nil {
		return nil, fmt.Errorf("configured FLITE queue and sensor control required")
	}
	return &FliteSwitch{queue: queue, sensor: sensor}, nil
}

func (f *FliteSwitch) SetStreaming(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return fmt.Errorf("FLITE switch closed")
	}
	if !on {
		return f.stopLocked()
	}
	if f.uncertain {
		return fmt.Errorf("FLITE stop not confirmed; retry stop before restart")
	}
	if f.on {
		return nil
	}
	// An ioctl may change hardware state even when it returns an error. Record
	// ownership BEFORE calling it, never after success only.
	f.queueTouched = true
	if e := f.queue.Start(); e != nil {
		return errors.Join(fmt.Errorf("FLITE STREAMON: %w", e), f.stopLocked())
	}
	f.sensorTouched = true
	if e := f.sensor.SetStreaming(true); e != nil {
		return errors.Join(fmt.Errorf("FLITE sensor-on: %w", e), f.stopLocked())
	}
	f.on = true
	return nil
}

func (f *FliteSwitch) stopLocked() error {
	f.on = false
	// Never stop/release upstream DMA or let Session destroy downstream buffers
	// while the sensor might still be producing frames.
	if f.sensorTouched {
		if e := f.sensor.SetStreaming(false); e != nil {
			f.uncertain = true
			return fmt.Errorf("FLITE sensor-off not confirmed; queue retained: %w", e)
		}
		f.sensorTouched = false
	}
	if f.queueTouched {
		if e := f.queue.Stop(); e != nil {
			f.uncertain = true
			return fmt.Errorf("FLITE STREAMOFF not confirmed; buffers retained: %w", e)
		}
		f.queueTouched = false
	}
	f.uncertain = false
	return nil
}

// Close stops the stream but does not close borrowed descriptors or free pools.
// If stop fails, the switch stays live so the owner can retry. A closed switch
// cannot accidentally restart an old sensor during a later session.
func (f *FliteSwitch) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	if e := f.stopLocked(); e != nil {
		return e
	}
	f.closed = true
	return nil
}

// StopConfirmed is useful to gate release of the caller's borrowed resources.
// False is conservative: it includes attempted/failed ioctls, not only known-on.
func (f *FliteSwitch) StopConfirmed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.on && !f.uncertain && !f.sensorTouched && !f.queueTouched
}

var _ SensorSwitch = (*FliteSwitch)(nil)

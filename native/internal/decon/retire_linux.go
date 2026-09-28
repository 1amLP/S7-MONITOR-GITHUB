//go:build linux && (amd64 || arm64)

package decon

import (
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
)

type retiredVideo struct {
	fence     int
	lease     *media.FrameLease
	submitted time.Time
}

// The previous frame remains pinned until DECON retires it. Two records keep
// normal frame submissions independent; a late hardware fence applies backpressure.
const maxPendingRetires = 2

func (p *Presenter) waitRetireFence(fd int, timeout int32) error {
	if p.waitFenceTest != nil {
		return p.waitFenceTest(fd, timeout)
	}
	return linuxio.Ioctl(fd, syncWait, unsafe.Pointer(&timeout))
}

func (p *Presenter) retireOldest(block bool) (bool, error) {
	if len(p.retired) == 0 {
		return false, nil
	}
	r := p.retired[0]
	var timeout int32
	if block {
		timeout = 250
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = p.waitRetireFence(r.fence, timeout)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if !block && (errors.Is(err, syscall.ETIME) || errors.Is(err, syscall.ETIMEDOUT)) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("DECON video retire fence: %w", err)
	}
	if err = syscall.Close(r.fence); err != nil {
		return false, fmt.Errorf("close confirmed DECON fence: %w", err)
	}
	if err = r.lease.Release(); err != nil {
		return false, fmt.Errorf("release retired DECON video: %w", err)
	}
	p.stats.RetiredVideo++
	p.stats.LastFenceUS = time.Since(r.submitted).Microseconds()
	copy(p.retired, p.retired[1:])
	p.retired = p.retired[:len(p.retired)-1]
	p.stats.PendingRetires = len(p.retired)
	return true, nil
}

func (p *Presenter) reclaimRetired(block bool) error {
	for len(p.retired) != 0 {
		done, err := p.retireOldest(block)
		if err != nil {
			return err
		}
		if !done {
			return nil
		}
	}
	return nil
}

func (p *Presenter) makeRoomForRetire() error {
	if err := p.reclaimRetired(false); err != nil {
		return err
	}
	if len(p.retired) >= maxPendingRetires {
		if _, err := p.retireOldest(true); err != nil {
			return err
		}
	}
	return nil
}

func (p *Presenter) presentVideoFast(config winConfigData, old *media.FrameLease) error {
	if err := p.makeRoomForRetire(); err != nil {
		return p.fail(err)
	}
	started := time.Now()
	previousAt := p.lastSubmit
	fence, err := p.submitConfig(config)
	if err != nil {
		return p.fail(err)
	}
	previous := p.retireFD
	p.retireFD, p.lastSubmit = fence, started
	p.retired = append(p.retired, retiredVideo{fence: previous, lease: old, submitted: previousAt})
	p.stats.PendingRetires = len(p.retired)
	p.stats.LastPaceUS = 0
	p.stats.Active = true
	p.stats.Frames++
	return nil
}

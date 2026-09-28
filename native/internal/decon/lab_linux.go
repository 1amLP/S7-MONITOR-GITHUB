//go:build linux && (amd64 || arm64)

package decon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"perimode/native/internal/linuxio"
)

const (
	labPolicyPath = "/etc/s7-lab.json"
	pinnedKernel  = "3.18.140-ge41817ea9198"
	getFBVar      = 0x4600
	getFBFix      = 0x4602
	ionImport     = 0xc0084905
	ionFree       = 0xc0044901
	syncWait      = 0x40043e00
)

// The caller must pass these facts only after BeginLabAttempt accepted the
// one-shot CACHE marker and stopped the normal fb0 presenter.
type LabPermit struct {
	AttemptArmed     bool
	PresenterStopped bool
	Record           func(bool, error) error
}

type labPolicy struct {
	Schema           string `json:"schema"`
	TrialSeconds     int    `json:"trial_seconds"`
	AttemptID        string `json:"attempt_id"`
	ReturnToRecovery bool   `json:"return_to_recovery"`
}

func checkLabGate(ctx context.Context, permit LabPermit, pid int, kernel string, policy []byte) error {
	if pid != 1 || !permit.AttemptArmed || !permit.PresenterStopped || permit.Record == nil || kernel != pinnedKernel {
		return fmt.Errorf("DECON lab requires pinned PID1, one-shot marker and stopped presenter")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 120*time.Second {
		return fmt.Errorf("DECON lab requires bounded 120-second context")
	}
	var p labPolicy
	if len(policy) > 4096 || json.Unmarshal(policy, &p) != nil || p.Schema != "S7-NATIVE-LAB-1" ||
		p.TrialSeconds != 120 || !p.ReturnToRecovery || len(p.AttemptID) != 64 {
		return fmt.Errorf("DECON lab policy mismatch")
	}
	for _, c := range p.AttemptID {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return fmt.Errorf("DECON lab attempt id mismatch")
		}
	}
	return nil
}

func checkFBLayout(v linuxio.FBVariable, f linuxio.FBFixed) error {
	if v.X != panelWidth || v.Y != panelHeight || v.Bits != 32 || v.XOffset != 0 || v.YOffset != 0 ||
		f.LineLength != panelWidth*4 || f.MemoryStart == 0 || f.MemoryLength < panelBytes {
		return fmt.Errorf("DECON lab requires unmodified 1440x2560 fb0 layout")
	}
	return nil
}

type ionFD struct{ Handle, FD int32 }
type ionHandle struct{ Handle int32 }

var trialStarted atomic.Bool

func checkIONFD(fd int) error {
	device, err := os.OpenFile("/dev/ion", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("DECON lab ION open: %w", err)
	}
	defer device.Close()
	ref := ionFD{Handle: -1, FD: int32(fd)}
	if err = linuxio.Ioctl(int(device.Fd()), ionImport, unsafe.Pointer(&ref)); err != nil || ref.Handle < 0 {
		return fmt.Errorf("DECON lab ION import rejected: %v", err)
	}
	h := ionHandle{Handle: ref.Handle}
	if err = linuxio.Ioctl(int(device.Fd()), ionFree, unsafe.Pointer(&h)); err != nil {
		return fmt.Errorf("DECON lab ION handle release: %w", err)
	}
	return nil
}

type trialOps struct {
	submit   func(*winConfigData) error
	wait     func(int) error
	pause    func(context.Context) error
	record   func(bool, error) error
	recovery func() error
}

// The e418 update worker releases the old fb0 IOVMM mapping. There is no
// same-boot FBIOPAN rollback after a successful WIN_CONFIG; Recovery resets it.
func runTrial(ctx context.Context, dmaFD int, ops trialOps) (submitted bool, err error) {
	if ops.submit == nil || ops.wait == nil || ops.pause == nil || ops.record == nil || ops.recovery == nil {
		return false, fmt.Errorf("DECON lab missing evidence or Recovery handler")
	}
	config, err := fullFrame(dmaFD)
	if err != nil {
		return false, err
	}
	defer func() {
		err = errors.Join(err, ops.recovery())
	}()
	defer func() {
		err = errors.Join(err, ops.record(submitted, err))
	}()
	defer func() {
		if config.Fence >= 0 {
			err = errors.Join(err, syscall.Close(int(config.Fence)))
		}
	}()
	if err = ops.submit(&config); err != nil {
		return false, fmt.Errorf("DECON lab WIN_CONFIG: %w", err)
	}
	submitted = true
	if config.Fence < 0 {
		return submitted, fmt.Errorf("DECON lab returned no scanout fence")
	}
	if err = ops.wait(int(config.Fence)); err != nil {
		return submitted, fmt.Errorf("DECON lab scanout fence: %w", err)
	}
	if err = ops.pause(ctx); err != nil {
		return submitted, err
	}
	return submitted, nil
}

// ProbeOnce is deliberately uncalled by the persistent appliance. An accepted
// 120-second lab BOOT must supply a completed scaler DMA-BUF and PID1 Recovery.
func ProbeOnce(ctx context.Context, permit LabPermit, completedFrame *os.File, recovery func() error) (bool, error) {
	if ctx == nil || completedFrame == nil || recovery == nil {
		return false, fmt.Errorf("DECON lab missing context, frame or Recovery")
	}
	policy, err := os.ReadFile(labPolicyPath)
	if err != nil {
		return false, err
	}
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return false, err
	}
	if err = checkLabGate(ctx, permit, os.Getpid(), strings.TrimSpace(string(kernel)), policy); err != nil {
		return false, err
	}
	if err = verifyABI(); err != nil {
		return false, err
	}
	frame, err := syscall.Dup(int(completedFrame.Fd()))
	if err != nil {
		return false, err
	}
	defer syscall.Close(frame)
	var st syscall.Stat_t
	if err = syscall.Fstat(frame, &st); err != nil || st.Size > 0 && st.Size < panelBytes {
		return false, fmt.Errorf("DECON lab frame shorter than panel: %v", err)
	}
	if err = checkIONFD(frame); err != nil {
		return false, err
	}
	device, err := os.OpenFile("/dev/fb0", os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer device.Close()
	var v linuxio.FBVariable
	var f linuxio.FBFixed
	if err = linuxio.Ioctl(int(device.Fd()), getFBVar, unsafe.Pointer(&v)); err != nil {
		return false, err
	}
	if err = linuxio.Ioctl(int(device.Fd()), getFBFix, unsafe.Pointer(&f)); err != nil {
		return false, err
	}
	if err = checkFBLayout(v, f); err != nil {
		return false, err
	}
	if !trialStarted.CompareAndSwap(false, true) {
		return false, fmt.Errorf("DECON lab already attempted in this boot")
	}
	return runTrial(ctx, frame, trialOps{
		submit: func(c *winConfigData) error {
			return linuxio.Ioctl(int(device.Fd()), winConfigIOCTL, unsafe.Pointer(c))
		},
		wait: func(fence int) error {
			timeout := int32(1000)
			return linuxio.Ioctl(fence, syncWait, unsafe.Pointer(&timeout))
		},
		pause: func(ctx context.Context) error {
			timer := time.NewTimer(250 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
		recovery: recovery,
		record:   permit.Record,
	})
}

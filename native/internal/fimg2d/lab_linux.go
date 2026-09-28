//go:build linux && (amd64 || arm64)

package fimg2d

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"perimode/native/internal/linuxio"
)

const pinnedKernel = "3.18.140-ge41817ea9198"

type LabPermit struct {
	AttemptArmed     bool
	PresenterStopped bool
	Record           func(Report, error) error
}

type Report struct {
	ProcessAttempted bool
	ProcessReturned  bool
	ProcessErrno     int
	ProcessFlags     uint32
	PayloadBytes     uint32
	Readback         [4]uint32
	Completed        bool
	PixelsVerified   bool
	Restored         bool
	OffscreenBytes   int
}

type labPolicy struct {
	Schema           string `json:"schema"`
	TrialSeconds     int    `json:"trial_seconds"`
	AttemptID        string `json:"attempt_id"`
	ReturnToRecovery bool   `json:"return_to_recovery"`
	Probe            string `json:"probe"`
}

func checkLabGate(ctx context.Context, permit LabPermit, recovery func() error, pid int, kernel string, policy []byte) error {
	if ctx == nil || pid != 1 || !permit.AttemptArmed || !permit.PresenterStopped || permit.Record == nil || recovery == nil || kernel != pinnedKernel {
		return fmt.Errorf("FIMG2D probe requires pinned PID1, armed lab and stopped presenter")
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
		return fmt.Errorf("FIMG2D probe requires a local five-second operation deadline")
	}
	var p labPolicy
	if len(policy) > 4096 || json.Unmarshal(policy, &p) != nil || p.Schema != "S7-NATIVE-LAB-1" || p.TrialSeconds != 0 || p.Probe != "fimg2d-offscreen" || !p.ReturnToRecovery || len(p.AttemptID) != 64 {
		return fmt.Errorf("FIMG2D lab policy mismatch")
	}
	for _, c := range p.AttemptID {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return fmt.Errorf("FIMG2D attempt id mismatch")
		}
	}
	return nil
}

func checkFBLayout(v variable, f fixed) error {
	if err := verifyFBABI(); err != nil {
		return err
	}
	if v.X != panelWidth || v.Y != panelHeight || v.XVirtual != panelWidth || v.YVirtual != panelHeight ||
		v.XOffset != 0 || v.YOffset != 0 || v.Bits != 32 || v.NonStd != 0 ||
		v.Red != (bitField{Offset: 16, Length: 8}) || v.Green != (bitField{Offset: 8, Length: 8}) ||
		v.Blue != (bitField{Offset: 0, Length: 8}) || v.Alpha != (bitField{}) ||
		f.MemoryStart != 0x10000000 || f.MemoryLength != 2*frameBytes || f.LineLength != panelWidth*4 ||
		(f.Visual != 0 && f.Visual != 2) {
		return fmt.Errorf("FIMG2D probe requires the pinned fb0 two-frame allocation and one-frame virtual height")
	}
	return nil
}

func deviceNumbers(rdev uint64) (major, minor uint64) {
	return (rdev>>8)&0xfff | (rdev>>32)&0xfffff000,
		(rdev & 0xff) | ((rdev >> 12) & 0xffffff00)
}

func checkDeviceIdentity(st syscall.Stat_t, identity []byte) error {
	major, minor := deviceNumbers(uint64(st.Rdev))
	if st.Mode&syscall.S_IFMT != syscall.S_IFCHR || major != 10 ||
		strings.TrimSpace(string(identity)) != fmt.Sprintf("%d:%d", major, minor) {
		return fmt.Errorf("unexpected G2D one-shot device identity %d:%d", major, minor)
	}
	return nil
}

type trialOps struct {
	process  func(*task, *image) error
	record   func(Report, error) error
	recovery func() error
}

// Only the second physical frame is touched. fb0 advertises one virtual frame,
// so FBIOPAN_DISPLAY cannot make this probe region visible on the pinned boot.
func probeCore(ctx context.Context, mapped []byte, ops trialOps) (result Report, quarantine bool, err error) {
	if ctx == nil || ops.process == nil || ops.record == nil || ops.recovery == nil {
		return result, false, fmt.Errorf("FIMG2D probe missing bounded lab handlers")
	}
	if err = ctx.Err(); err != nil {
		return result, false, err
	}
	src, command, err := solidOffscreen(mapped)
	if err != nil {
		return result, false, err
	}
	region := mapped[frameBytes : frameBytes+probeMappedSpan]
	saved := bytes.Clone(region)
	clear(region)
	result.OffscreenBytes = len(region)
	if err = ctx.Err(); err != nil {
		copy(region, saved)
		return result, false, err
	}
	defer func() { err = errors.Join(err, ops.recovery()) }()
	defer func() { err = errors.Join(err, ops.record(result, err)) }()
	result.ProcessAttempted = true
	quarantine = true
	if err = ops.process(command, src); err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) {
			result.ProcessErrno = int(errno)
		}
		return result, quarantine, fmt.Errorf("G2D one-shot PROCESS: %w", err)
	}
	result.ProcessReturned = true
	result.ProcessFlags = command.Flags
	result.PayloadBytes = command.Target.Plane[0].Payload
	if command.Flags&processError != 0 || result.PayloadBytes != probeSpan {
		return result, quarantine, fmt.Errorf("G2D one-shot completion failed flags=0x%x payload=%d", command.Flags, result.PayloadBytes)
	}
	runtime.KeepAlive(src)
	runtime.KeepAlive(command)
	runtime.KeepAlive(mapped)
	result.Completed = true
	for i, offset := range []int{0, (probeCols - 1) * 4, (probeRows - 1) * panelWidth * 4, (probeRows-1)*panelWidth*4 + (probeCols-1)*4} {
		result.Readback[i] = binary.LittleEndian.Uint32(region[offset:])
		if result.Readback[i]&0xffffff != 0xffffff {
			err = fmt.Errorf("FIMG2D offscreen readback differs from solid white")
			break
		}
	}
	result.PixelsVerified = err == nil
	copy(region, saved)
	result.Restored = true
	quarantine = false
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	return result, quarantine, err
}

type resources struct {
	fb, g2d *os.File
	mapping []byte
}

func (r *resources) close() error {
	var err error
	if len(r.mapping) != 0 {
		err = errors.Join(err, syscall.Munmap(r.mapping))
	}
	if r.g2d != nil {
		err = errors.Join(err, r.g2d.Close())
	}
	if r.fb != nil {
		err = errors.Join(err, r.fb.Close())
	}
	return err
}

var once atomic.Bool
var retained struct {
	sync.Mutex
	owners []*resources
}

// ProbeOnce is not wired into the persistent appliance. Main must call it only
// after the lab CACHE marker and Recovery rollback script have been armed.
func ProbeOnce(ctx context.Context, permit LabPermit, recovery func() error) (result Report, err error) {
	policy, err := os.ReadFile("/etc/s7-lab.json")
	if err != nil {
		return result, err
	}
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return result, err
	}
	if err = checkLabGate(ctx, permit, recovery, os.Getpid(), strings.TrimSpace(string(kernel)), policy); err != nil {
		return result, err
	}
	owner := &resources{}
	defer func() {
		if owner == nil {
			return
		}
		err = errors.Join(err, owner.close())
	}()
	owner.fb, err = os.OpenFile("/dev/fb0", os.O_RDWR, 0)
	if err != nil {
		return result, err
	}
	var v variable
	var f fixed
	if err = linuxio.Ioctl(int(owner.fb.Fd()), 0x4600, unsafe.Pointer(&v)); err != nil {
		return result, err
	}
	if err = linuxio.Ioctl(int(owner.fb.Fd()), 0x4602, unsafe.Pointer(&f)); err != nil {
		return result, err
	}
	if err = checkFBLayout(v, f); err != nil {
		return result, err
	}
	owner.mapping, err = syscall.Mmap(int(owner.fb.Fd()), 0, int(f.MemoryLength), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return result, err
	}
	owner.g2d, err = os.OpenFile("/dev/fimg2d", os.O_RDWR, 0)
	if err != nil {
		return result, err
	}
	var st syscall.Stat_t
	if err = syscall.Fstat(int(owner.g2d.Fd()), &st); err != nil {
		return result, err
	}
	identity, err := os.ReadFile("/sys/class/misc/fimg2d/dev")
	if err != nil {
		return result, err
	}
	if err = checkDeviceIdentity(st, identity); err != nil {
		return result, err
	}
	if !once.CompareAndSwap(false, true) {
		return result, fmt.Errorf("FIMG2D lab already attempted in this boot")
	}
	var unsafeOwnership bool
	result, unsafeOwnership, err = probeCore(ctx, owner.mapping, trialOps{
		process: func(command *task, src *image) error {
			e := linuxio.Ioctl(int(owner.g2d.Fd()), processIOCTL, unsafe.Pointer(command))
			runtime.KeepAlive(src)
			return e
		},
		record:   permit.Record,
		recovery: recovery,
	})
	if unsafeOwnership {
		retained.Lock()
		retained.owners = append(retained.owners, owner)
		retained.Unlock()
		owner = nil
	}
	return result, err
}

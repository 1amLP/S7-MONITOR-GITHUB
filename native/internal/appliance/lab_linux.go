//go:build linux && (amd64 || arm64)

package appliance

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"perimode/native/internal/selinux"
)

const labPolicyPath = "/etc/s7-lab.json"
const labMount = "/run/s7-lab-cache"

type LabPolicy struct {
	Schema           string `json:"schema"`
	TrialSeconds     int    `json:"trial_seconds"`
	AttemptID        string `json:"attempt_id"`
	ReturnToRecovery bool   `json:"return_to_recovery"`
	Probe            string `json:"probe,omitempty"`
}

func (p LabPolicy) Validate() error {
	id, err := hex.DecodeString(p.AttemptID)
	if err != nil || len(id) != 32 || p.Schema != "S7-NATIVE-LAB-1" || !p.ReturnToRecovery ||
		!(p.TrialSeconds == 120 && p.Probe == "" || p.TrialSeconds == 0 && p.Probe == "fimg2d-offscreen") {
		return fmt.Errorf("invalid native laboratory policy")
	}
	return nil
}

func LoadLabPolicy() (*LabPolicy, error) { return loadLabPolicy(labPolicyPath) }

func openLabRegular(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("native laboratory input is not regular")
	}
	return f, nil
}

func loadLabPolicy(path string) (*LabPolicy, error) {
	f, err := openLabRegular(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 4097))
	dec.DisallowUnknownFields()
	var p LabPolicy
	if err = dec.Decode(&p); err != nil {
		return nil, err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("trailing native laboratory policy")
	}
	return &p, p.Validate()
}

func labMarker(policy LabPolicy) string {
	return "S7-NATIVE-LAB-1\n" + PinnedSerial + "\n" + policy.AttemptID + "\n"
}

// recordLabAttempt returns true only for the first exact attempt. A second boot
// cannot silently repeat the candidate after a watchdog reset.
func recordLabAttempt(base string, policy LabPolicy) (bool, error) {
	if err := policy.Validate(); err != nil {
		return false, err
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return false, err
	}
	st, err := os.Lstat(base)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("unsafe native laboratory marker directory")
	}
	path := filepath.Join(base, "attempt-"+policy.AttemptID)
	content := []byte(labMarker(policy))
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_SYNC, 0600)
	if errors.Is(err, syscall.EEXIST) {
		f, openErr := openLabRegular(path)
		if openErr != nil {
			return false, openErr
		}
		data, readErr := io.ReadAll(io.LimitReader(f, 257))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil || string(data) != string(content) {
			return false, fmt.Errorf("native laboratory marker mismatch")
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	f := os.NewFile(uintptr(fd), path)
	if _, err = f.Write(content); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err == nil, err
}

func BeginLabAttempt(policy LabPolicy) (first bool, err error) {
	if os.Getpid() != 1 {
		return false, fmt.Errorf("native laboratory attempt requires PID1")
	}
	device, err := cacheDevice(true)
	if err != nil {
		return false, err
	}
	if err = os.Mkdir(labMount, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return false, err
	}
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
	if err = syscall.Mount(device, labMount, "ext4", flags, selinux.CacheRWMountOptions); err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, syscall.Unmount(labMount, 0)) }()
	first, err = recordLabAttempt(filepath.Join(labMount, "s7-lab"), policy)
	if err == nil {
		syscall.Sync()
	}
	return first, err
}

type labStatus struct {
	Schema      string          `json:"schema"`
	Serial      string          `json:"serial"`
	AttemptID   string          `json:"attempt_id"`
	Stage       string          `json:"stage"`
	OK          bool            `json:"ok"`
	Error       string          `json:"error,omitempty"`
	Kernel      string          `json:"kernel,omitempty"`
	UnixMS      int64           `json:"unix_ms"`
	FinalReport json.RawMessage `json:"final_report,omitempty"`
	ReportError string          `json:"report_error,omitempty"`
}

const labReportLimit = 256 << 10

func readLabReport(path string) (json.RawMessage, error) {
	f, err := openLabRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, labReportLimit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > labReportLimit || !json.Valid(b) {
		return nil, fmt.Errorf("invalid or oversized final native report")
	}
	return json.RawMessage(b), nil
}

func validLabStage(stage string) bool {
	if len(stage) == 0 || len(stage) > 64 {
		return false
	}
	return strings.IndexFunc(stage, func(r rune) bool {
		return !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) < 0
}

func saveLabEvidenceOnce(path string, data []byte) error {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if errors.Is(err, syscall.EEXIST) {
		st, e := os.Lstat(path)
		if e != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("unsafe lab evidence destination")
		}
		return nil
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// A reset re-enters bootstrap before BeginLabAttempt rejects the marker.
// Preserve the first attempt and previous kernel now, before either TWRP or
// another status write can replace that evidence. This is lab-only CACHE I/O.
func preserveLabReset(base string, p LabPolicy, kernelPath string) error {
	if err := p.Validate(); err != nil {
		return err
	}
	marker, err := openLabRegular(filepath.Join(base, "attempt-"+p.AttemptID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(marker, 257))
	closeErr := marker.Close()
	if err != nil || closeErr != nil || string(b) != labMarker(p) {
		return fmt.Errorf("invalid repeated attempt marker")
	}
	var result error
	status := filepath.Join(base, "status-"+p.AttemptID+".json")
	if prior, e := readLabReport(status); e == nil {
		result = saveLabEvidenceOnce(filepath.Join(base, "before-reset-"+p.AttemptID+".json"), prior)
	} else if !errors.Is(e, os.ErrNotExist) {
		result = e
	}
	f, e := openLabRegular(kernelPath)
	if e != nil {
		return errors.Join(result, e)
	}
	// Samsung's retained kernel ring is bounded independently from live logs.
	data, e := io.ReadAll(io.LimitReader(f, 4<<20))
	e = errors.Join(e, f.Close())
	if e == nil {
		if len(data) > 2<<20 {
			data = data[len(data)-(2<<20):]
		}
		e = saveLabEvidenceOnce(filepath.Join(base, "previous-kernel-"+p.AttemptID+".log"), data)
	}
	return errors.Join(result, e)
}

func writeLabStatus(base string, policy LabPolicy, stage string, failure error, now time.Time) error {
	if err := policy.Validate(); err != nil || !validLabStage(stage) {
		return fmt.Errorf("invalid native laboratory status")
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(base)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe native laboratory status directory")
	}
	if stage == "bootstrap-root-ready" {
		if err := preserveLabReset(base, policy, "/proc/last_kmsg"); err != nil {
			return err
		}
	}
	message := ""
	if failure != nil {
		runes := []rune(strings.ToValidUTF8(failure.Error(), "?"))
		if len(runes) > 512 {
			runes = runes[:512]
		}
		message = string(runes)
	}
	kernel, _ := read("/proc/sys/kernel/osrelease")
	record := labStatus{Schema: "S7-NATIVE-LAB-STATUS-1", Serial: PinnedSerial,
		AttemptID: policy.AttemptID, Stage: stage, OK: failure == nil, Error: message,
		Kernel: kernel, UnixMS: now.UnixMilli()}
	if stage == "run-native-stop" {
		record.FinalReport, err = readLabReport("/run/final-status.json")
		if err != nil {
			record.ReportError = err.Error()
		}
	}
	data, err := json.Marshal(record)
	if err != nil || len(data) > labReportLimit+4096 {
		return fmt.Errorf("native laboratory status is oversized")
	}
	path := filepath.Join(base, "status-"+policy.AttemptID+".json")
	if current, statErr := os.Lstat(path); statErr == nil && !current.Mode().IsRegular() {
		return fmt.Errorf("unsafe native laboratory status destination")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	f, err := os.CreateTemp(base, ".status-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(data, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp, path); err != nil {
		return err
	}
	dir, err := os.Open(base)
	if err != nil {
		return err
	}
	err = dir.Sync()
	return errors.Join(err, dir.Close())
}

func recordLabStatus(policy LabPolicy, stage string, failure error, beforePolicy bool) (err error) {
	if os.Getpid() != 1 {
		return fmt.Errorf("native laboratory status requires PID1")
	}
	if _, err = physicalSerial(); err != nil {
		return err
	}
	device, mountData := "", selinux.CacheRWMountOptions
	if beforePolicy {
		device, err = cacheDeviceBeforePolicy(true)
		mountData = "errors=remount-ro"
	} else {
		device, err = cacheDevice(true)
	}
	if err != nil {
		return err
	}
	if err = os.Mkdir(labMount, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	flags := uintptr(syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC | syscall.MS_NOATIME)
	if err = syscall.Mount(device, labMount, "ext4", flags, mountData); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, syscall.Unmount(labMount, 0)) }()
	err = writeLabStatus(filepath.Join(labMount, "s7-lab"), policy, stage, failure, time.Now())
	if err == nil {
		syscall.Sync()
	}
	return err
}

// RecordLabStatus writes one bounded, serial-bound record to CACHE after the
// pinned SELinux policy is active. It never changes protected partitions.
func RecordLabStatus(policy LabPolicy, stage string, failure error) error {
	return recordLabStatus(policy, stage, failure, false)
}

// RecordLabStatusBeforePolicy is only for the one-shot bootstrap boundary. It
// uses the same pinned CACHE identity and clean-superblock checks without an
// object label or context mount that cannot exist before the first policy load.
func RecordLabStatusBeforePolicy(policy LabPolicy, stage string, failure error) error {
	return recordLabStatus(policy, stage, failure, true)
}

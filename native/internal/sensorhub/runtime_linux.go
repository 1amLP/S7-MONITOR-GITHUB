//go:build linux && (amd64 || arm64)

package sensorhub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"perimode/native/internal/linuxio"
	"perimode/native/internal/selinux"
)

const Prefix = "/opt/s7-hub"
const LHDHash = "22075698ef8f3ee761a8a88558bce9483a021fc4c6eb032d79b1c493c1887c49"
const WakeName = "s7_native_lhd"

var runtimeFiles = map[string]bool{
	"bin/lhd": true, "bin/linker64": true, "lhd.conf": true,
	"lib64/libc.so": true, "lib64/libdl.so": true, "lib64/libm.so": true, "lib64/libc++.so": true,
	"lib64/liblog.so": true, "lib64/libutils.so": true, "lib64/libhardware_legacy.so": true,
}

type manifest struct {
	Schema           string   `json:"schema"`
	Prefix           string   `json:"prefix"`
	LHDHash          string   `json:"lhd_sha256"`
	HardwareAccepted bool     `json:"hardware_accepted"`
	Services         []string `json:"android_services"`
	UsesBionic       bool     `json:"uses_bionic_libraries"`
	Files            []struct {
		Path  string `json:"path"`
		SHA   string `json:"sha256"`
		Bytes int64  `json:"bytes"`
		Mode  uint32 `json:"mode"`
	} `json:"files"`
}

// VerifyRuntime never executes code. Runtime paths and complete membership are
// allowlisted; a manifest cannot introduce an executable or redirect to SYSTEM.
func VerifyRuntime(prefix, manifestPath string) error {
	return verifyRuntime(context.Background(), prefix, manifestPath, false)
}

// The source archive stores payload bytes, not executable permissions. Only
// the installed ramdisk is subject to launch-mode checks. Both paths verify
// exactly the same file contents; launch never changes permissions to hide an
// incomplete installation.
func verifyRuntime(ctx context.Context, prefix, manifestPath string, launch bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(prefix) || filepath.Clean(prefix) != prefix || prefix == "/" {
		return errors.New("sensor hub prefix must be canonical and absolute")
	}
	f, e := os.OpenFile(manifestPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() || st.Size() <= 0 || st.Size() > 32768 {
		return errors.New("sensor hub manifest must be a bounded regular file")
	}
	d := json.NewDecoder(io.LimitReader(f, 32769))
	d.DisallowUnknownFields()
	var m manifest
	if e = d.Decode(&m); e != nil {
		return e
	}
	var extra any
	if e = d.Decode(&extra); e != io.EOF {
		return errors.New("trailing hub manifest data")
	}
	if m.Schema != "S7_SENSORHUB_RUNTIME_1" || m.Prefix != "opt/s7-hub" || m.LHDHash != LHDHash || len(m.Services) != 0 || !m.UsesBionic || len(m.Files) != len(runtimeFiles) {
		return errors.New("unsupported isolated sensor hub manifest")
	}
	seen := make(map[string]bool)
	for _, v := range m.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		wantMode := uint32(0644)
		if v.Path == "bin/lhd" || v.Path == "bin/linker64" {
			wantMode = 0755
		}
		if v.Mode != wantMode {
			return fmt.Errorf("invalid sensor hub declared mode: %s", v.Path)
		}
		if !runtimeFiles[v.Path] || seen[v.Path] || len(v.SHA) != 64 || v.Bytes <= 0 || v.Bytes > 4<<20 {
			return errors.New("invalid sensor hub file entry")
		}
		seen[v.Path] = true
		path := filepath.Join(prefix, v.Path)
		// Do not follow a replaced runtime directory or file through a symlink.
		for p := path; ; p = filepath.Dir(p) {
			st, err := os.Lstat(p)
			if err != nil {
				return err
			}
			if st.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("sensor hub symlink refused: %s", p)
			}
			if p == prefix {
				break
			}
			if p == filepath.Dir(p) {
				return errors.New("runtime outside prefix")
			}
		}
		fd, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		st, err := fd.Stat()
		if err != nil {
			fd.Close()
			return err
		}
		if !st.Mode().IsRegular() || st.Size() != v.Bytes || (launch && (uint32(st.Mode().Perm()) != v.Mode || st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0)) {
			fd.Close()
			return fmt.Errorf("sensor hub file size/type: %s", v.Path)
		}
		h := sha256.New()
		_, err = io.Copy(h, &cancelReader{ctx: ctx, reader: io.LimitReader(fd, (4<<20)+1)})
		ce := fd.Close()
		if err != nil || ce != nil {
			return errors.Join(err, ce)
		}
		if hex.EncodeToString(h.Sum(nil)) != v.SHA || v.Path == "bin/lhd" && v.SHA != LHDHash {
			return fmt.Errorf("sensor hub hash mismatch: %s", v.Path)
		}
	}
	return nil
}

type cancelReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *cancelReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func preflight() error {
	for _, path := range []string{"/dev/bbd_patch", "/dev/bbd_control", "/dev/bbd_sensor", "/dev/ttyBCM"} {
		st, e := os.Stat(path)
		if e != nil {
			return fmt.Errorf("sensor hub device absent %s: %w", path, e)
		}
		if st.Mode()&os.ModeCharDevice == 0 {
			return fmt.Errorf("sensor hub device is not character device: %s", path)
		}
	}
	for _, path := range []string{"/sys/class/sec/sensorhub/mcu_power", "/sys/class/sec/gps/GPS_PWR_EN/value"} {
		if _, e := os.Stat(path); e != nil {
			return fmt.Errorf("base-firmware hub power control absent: %s: %w", path, e)
		}
	}
	return nil
}
func cleanupWake() error {
	b, e := os.ReadFile("/sys/power/wake_lock")
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	for _, name := range strings.Fields(string(b)) {
		if name == WakeName {
			return linuxio.WriteAttr("/sys/power/wake_unlock", WakeName)
		}
	}
	return nil
}
func StartNative(ctx context.Context) (Process, error) {
	if err := selinux.RequireContext(selinux.NativeContext); err != nil {
		return nil, err
	}
	if err := verifyRuntime(ctx, Prefix, "/etc/s7-sensorhub.json", true); err != nil {
		return nil, fmt.Errorf("hub runtime: %w", err)
	}
	if err := preflight(); err != nil {
		return nil, err
	}
	if err := prepareLaunch(ctx, nativeLaunchFS()); err != nil {
		return nil, fmt.Errorf("hub launch preparation: %w", err)
	}
	// Only the helper enters a private network namespace. Its original TCP
	// endpoints remain on private loopback; USB/monitor never change namespace.
	// Exec of the original lhd enters s7_sensorhub through its pinned file label.
	return startIsolatedHub(ctx)
}

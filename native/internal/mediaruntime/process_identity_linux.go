//go:build linux && (amd64 || arm64)

package mediaruntime

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var requiredNativeServices = [...]struct{ path, domain string }{
	{"/vendor/bin/vndservicemanager", "vndservicemanager"},
	{"/system/bin/servicemanager", "servicemanager"},
	{"/system/system_ext/bin/hwservicemanager", "hwservicemanager"},
	{"/system/bin/mediaserver", "mediaserver"},
	{"/system/system_ext/bin/hw/android.hidl.allocator@1.0-service", "hal_allocator_default"},
	{"/vendor/bin/hw/android.hardware.graphics.allocator@2.0-service", "hal_graphics_allocator_default"},
	{"/vendor/bin/hw/android.hardware.media.omx@1.0-service", "mediacodec"},
}

// A command name alone is not identity. Require a child of the private init
// in its exact service domain, plus a start time to detect PID reuse. Unlike
// /proc/PID/exe this does not require ptrace access across service UIDs.
func serviceIdentity(pid string, command, label, stat []byte) (path, identity string, err error) {
	n, e := strconv.Atoi(pid)
	if e != nil || n <= 1 {
		return "", "", errors.New("invalid service PID")
	}
	name, _, terminated := bytes.Cut(command, []byte{0})
	if !terminated {
		return "", "", errors.New("unterminated service argv")
	}
	canonical := ""
	for _, s := range requiredNativeServices {
		// The pinned OMX main overwrites argv[0] with media.codec after exec.
		matched := string(name) == s.path || (s.domain == "mediacodec" && string(name) == "media.codec")
		if matched && strings.TrimRight(string(label), "\x00\n") == "u:r:"+s.domain+":s0" {
			canonical = s.path
			break
		}
	}
	if canonical == "" {
		return "", "", fmt.Errorf("service path/domain not allowlisted: argv0=%.160q context=%.80q", name, label)
	}
	text := string(stat)
	end := strings.LastIndexByte(text, ')')
	if !strings.HasPrefix(text, pid+" (") || end < 0 {
		return "", "", errors.New("invalid service stat")
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 20 || fields[1] != "1" || fields[0] == "Z" || fields[0] == "X" {
		return "", "", errors.New("service is not a live private-init child")
	}
	start, e := strconv.ParseUint(fields[19], 10, 64)
	if e != nil || start == 0 {
		return "", "", errors.New("invalid service start time")
	}
	return canonical, fmt.Sprintf("%s:%d", pid, start), nil
}

func boundedProcRead(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err == nil && len(b) > 4096 {
		err = errors.New("proc service record exceeds bound")
	}
	return b, err
}

func readServiceIdentity(pid string) (string, string, error) {
	root := filepath.Join("/proc", pid)
	command, err := boundedProcRead(root + "/cmdline")
	if err != nil {
		return "", "", err
	}
	label, err := boundedProcRead(root + "/attr/current")
	if err != nil {
		return "", "", err
	}
	stat, err := boundedProcRead(root + "/stat")
	if err != nil {
		return "", "", err
	}
	return serviceIdentity(pid, command, label, stat)
}

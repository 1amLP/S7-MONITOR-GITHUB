//go:build linux && (amd64 || arm64)

package selinux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type FileLabel struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Context  string `json:"context"`
	Optional bool   `json:"optional,omitempty"`
}
type LabelPlan struct {
	Schema        string      `json:"schema"`
	RuntimeSHA256 string      `json:"runtime_sha256"`
	Files         []FileLabel `json:"files"`
	VideoContext  string      `json:"video_context"`
}

func (p LabelPlan) Validate() error {
	if p.Schema != "S7-SELINUX-LABELS-1" || !validHash(p.RuntimeSHA256) || len(p.Files) < 10 || len(p.Files) > 4096 || p.VideoContext != "u:object_r:video_device:s0" {
		return errors.New("invalid runtime label plan")
	}
	seen := map[string]bool{}
	for _, f := range p.Files {
		if !filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path || strings.ContainsAny(f.Path, "\\\x00\n\r") || len(f.Path) > 512 || seen[f.Path] || !contextPattern.MatchString(f.Context) {
			return errors.New("unsafe/duplicate runtime label entry")
		}
		if f.Kind != KindFile && f.Kind != KindDir && f.Kind != KindChar {
			return errors.New("unsupported runtime label kind")
		}
		if f.Optional && f.Path != "/dev/kmsg" && f.Path != "/dev/mali0" &&
			f.Path != "/opt/s7-gpu" && f.Path != "/opt/s7-gpu/bin" && f.Path != "/opt/s7-gpu/bin/gpu-probe-arm64" {
			return errors.New("mandatory runtime label cannot be optional")
		}
		seen[f.Path] = true
	}
	for path, want := range map[string]string{"/system/bin/init": "u:object_r:init_exec:s0", "/s7/native": "u:object_r:s7_native_exec:s0", "/s7/mediacodec-bridge": "u:object_r:s7_codec_exec:s0"} {
		found := false
		for _, v := range p.Files {
			if v.Path == path {
				found = v.Context == want && v.Kind == KindFile && !v.Optional
				break
			}
		}
		if !found {
			return fmt.Errorf("missing critical runtime label %s", path)
		}
	}
	return nil
}
func LoadLabelPlan(runtimeSHA string) (LabelPlan, error) {
	var p LabelPlan
	pin, e := LoadPin()
	if e != nil {
		return p, e
	}
	if runtimeSHA != pin.RuntimeSHA256 {
		return p, errors.New("security plan/runtime generation mismatch")
	}
	b, e := pinned(Base+"/labels.json", pin.LabelsBytes, pin.LabelsSHA256)
	if e != nil {
		return p, e
	}
	if e = decode(b, &p); e != nil {
		return p, e
	}
	if p.RuntimeSHA256 != runtimeSHA {
		return p, errors.New("label plan references another runtime")
	}
	return p, p.Validate()
}
func dynamic(path string) bool {
	for _, d := range []string{"/dev", "/proc", "/sys", "/data", "/run", "/tmp", "/s7-control"} {
		if path == d || strings.HasPrefix(path, d+"/") {
			return true
		}
	}
	return false
}
func implicitKernelLabel(path string) bool {
	return path == "/proc" || strings.HasPrefix(path, "/proc/") || path == "/sys" || strings.HasPrefix(path, "/sys/") || path == "/dev/s7-cgroup"
}

// Immutable entries are labeled before readonly bind/remount; dynamic entries
// only after their new tmpfs mounts. Never walk a live global /dev or /sys here.
func (p LabelPlan) Apply(ctx context.Context, root string, afterMount bool) error {
	return p.apply(ctx, root, afterMount, Label)
}
func (p LabelPlan) apply(ctx context.Context, root string, afterMount bool, set func(string, string, string) error) error {
	if e := p.Validate(); e != nil {
		return e
	}
	if root != "/run/s7-media-root" {
		return errors.New("label root outside owned media RAM")
	}
	for _, f := range p.Files {
		if dynamic(f.Path) != afterMount || implicitKernelLabel(f.Path) {
			continue
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		path := root + f.Path
		if f.Path == "/" {
			path = root
		}
		e := set(path, f.Context, f.Kind)
		if f.Optional && errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return fmt.Errorf("apply runtime context: %w", e)
		}
	}
	return nil
}

var videoPath = regexp.MustCompile(`^/dev/video[0-9]{1,4}$`)

func LabelMFC(root, path string) error {
	if root != "/run/s7-media-root" || !videoPath.MatchString(path) {
		return errors.New("invalid selected MFC path")
	}
	return Label(root+path, "u:object_r:video_device:s0", KindChar)
}

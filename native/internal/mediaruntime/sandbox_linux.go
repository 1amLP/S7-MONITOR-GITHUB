//go:build linux && (amd64 || arm64)

package mediaruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// The retained ARM32 OMX process opens these paths in its original minijail
// implementation. Its vendor extension is optional, the system policy is not.
const omxSandbox = "/system/etc/seccomp_policy/mediacodec.policy"
const vendorOMXSandbox = "/vendor/etc/seccomp_policy/mediacodec.policy"
const maxPolicyBytes = 64 << 10

var sandboxName = regexp.MustCompile(`^/(system|vendor|apex/[A-Za-z0-9_.-]+)/etc/seccomp_policy/[A-Za-z0-9_.-]+\.policy$`)

// validateSandbox runs after full bundle authentication, before executing init.
// It checks the original filter's required FILE dependencies only. It neither
// compiles BPF nor changes rules, seccomp mode, SELinux or allowed syscalls.
func validateSandbox(ctx context.Context, root string) error {
	return checkSandbox(ctx, func(name string) ([]byte, error) {
		// Extract created this tree as private RAM without any symlinks. Still
		// reject a replaced path rather than follow it into the build/host tree.
		parent := root
		for _, part := range strings.Split(strings.TrimPrefix(name, "/"), "/") {
			parent = filepath.Join(parent, part)
			st, err := os.Lstat(parent)
			if err != nil {
				return nil, err
			}
			if st.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("sandbox symlink: %s", name)
			}
		}
		f, err := OpenRegular(parent)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if st.Mode().Perm()&0022 != 0 {
			return nil, fmt.Errorf("writable sandbox policy: %s", name)
		}
		return io.ReadAll(io.LimitReader(contextReader{ctx, f}, maxPolicyBytes+1))
	})
}

func checkSandbox(ctx context.Context, read func(string) ([]byte, error)) error {
	seen := make(map[string][]byte)
	total := 0
	var walk func(string, int, bool) error
	walk = func(name string, depth int, optional bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !sandboxName.MatchString(name) || path.Clean(name) != name {
			return fmt.Errorf("invalid sandbox include path: %q", name)
		}
		raw, ok := seen[name]
		if !ok {
			if len(seen) >= 64 {
				return errors.New("sandbox dependency limit")
			}
			var err error
			raw, err = read(name)
			if optional && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("sandbox dependency %s: %w", name, err)
			}
			if len(raw) == 0 || len(raw) > maxPolicyBytes {
				return fmt.Errorf("sandbox policy size: %s", name)
			}
			for _, b := range raw {
				if b == 0 || b >= 128 {
					return fmt.Errorf("sandbox policy text: %s", name)
				}
			}
			total += len(raw)
			if total > 1<<20 {
				return errors.New("sandbox dependency byte limit")
			}
			seen[name] = raw
		}
		for number, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "@") {
				continue
			}
			if !strings.HasPrefix(line, "@include ") {
				return fmt.Errorf("sandbox directive %s:%d", name, number+1)
			}
			if depth > 0 {
				return fmt.Errorf("nested sandbox include rejected by minijail: %s", name)
			}
			child := strings.TrimPrefix(line, "@include ")
			if err := walk(child, depth+1, false); err != nil {
				return fmt.Errorf("sandbox include from %s:%d: %w", name, number+1, err)
			}
		}
		return nil
	}
	if err := walk(omxSandbox, 0, false); err != nil {
		return err
	}
	return walk(vendorOMXSandbox, 0, true)
}

//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

const gpuProbeExecutable = "/opt/s7-gpu/bin/gpu-probe-arm64"

func (s *State) gpuProbeDiagnostic() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]string{"status": s.GPUProbeStatus, "error": s.GPUProbeError}
}

func gpuLibraryPath(root string) string {
	// Android's normal libc/libdl/libm/libdl_android entries point into APEX.
	// Native has no APEX mounts; use the real bootstrap files from this SYSTEM.
	return strings.Join([]string{root + "/system/lib64/bootstrap", root + "/system/lib64", root + "/system/vendor/lib64", root + "/system/vendor/lib64/egl"}, ":")
}

type gpuProbeOutput struct{ data []byte }

func (o *gpuProbeOutput) Write(p []byte) (int, error) {
	n := len(p)
	if len(o.data) < 4096 {
		o.data = append(o.data, p[:min(n, 4096-len(o.data))]...)
	}
	return n, nil
}

func gpuProbeResult(output string, runErr error) (string, error) {
	if runErr != nil {
		return "FAILED", fmt.Errorf("GPU probe: %w; %s", runErr, strings.TrimSpace(output))
	}
	seen := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		seen[strings.TrimSpace(line)] = true
	}
	for _, marker := range []string{"MALI_LIBRARY=OK", "OPENCL_GPU=OK", "DMA_BUF_EXTENSION=YES", "ION_DMA_BUF=OK", "DMA_BUF_IMPORT=OK"} {
		if !seen[marker] {
			return "FAILED", fmt.Errorf("GPU probe missing %s: %s", marker, strings.TrimSpace(output))
		}
	}
	if seen["GPU_NDRANGE_RESULT=0"] && seen["GPU_NDRANGE_ENQUEUE_RESULT=0"] && seen["GPU_NDRANGE_FINISH_RESULT=0"] {
		return "OPENCL KERNEL/ION OK; SCANOUT UNTESTED", nil
	}
	for marker := range seen {
		if strings.HasPrefix(marker, "GPU_NDRANGE_RESULT=") {
			return "FAILED", fmt.Errorf("GPU kernel incomplete: %s", strings.TrimSpace(output))
		}
	}
	if !seen["GPU_FILL_RESULT=0"] {
		return "FAILED", fmt.Errorf("GPU probe missing successful fill: %s", strings.TrimSpace(output))
	}
	return "OPENCL/ION OK; SCANOUT UNTESTED", nil
}

func runGPUProbe(ctx context.Context) (status string, err error) {
	root, unmount, err := mountGPUSystem()
	if err != nil {
		return "FAILED", err
	}
	defer func() {
		if closeErr := unmount(); closeErr != nil {
			status = "FAILED"
			err = errors.Join(err, closeErr)
		}
	}()
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	linker := root + "/system/bin/bootstrap/linker64"
	cmd := exec.CommandContext(probeCtx, linker, gpuProbeExecutable)
	cmd.Env = []string{
		"LD_LIBRARY_PATH=" + gpuLibraryPath(root),
		"ANDROID_ROOT=" + root + "/system",
		"TMPDIR=/run",
	}
	var stdout, stderr gpuProbeOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if probeCtx.Err() != nil {
		runErr = errors.Join(runErr, probeCtx.Err())
	}
	return gpuProbeResult(string(stdout.data)+"\n"+string(stderr.data), runErr)
}

func (u *UI) startGPUProbe() error {
	st, err := os.Lstat(gpuProbeExecutable)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !st.Mode().IsRegular() || st.Size() > 65536 {
		return fmt.Errorf("invalid GPU probe executable: %v", err)
	}
	return u.workers.Start("gpu-probe", func(ctx context.Context) error {
		status, probeErr := runGPUProbe(ctx)
		u.state.mu.Lock()
		u.state.GPUProbeStatus = status
		if probeErr != nil {
			u.state.GPUProbeError = probeErr.Error()
		}
		u.state.mu.Unlock()
		log.Printf("GPU probe status=%s error=%v", status, probeErr)
		u.state.Error(probeErr)
		<-ctx.Done()
		return ctx.Err()
	})
}

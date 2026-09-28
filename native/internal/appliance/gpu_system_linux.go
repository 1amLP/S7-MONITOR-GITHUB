//go:build linux && (amd64 || arm64)

package appliance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const gpuSystemMount = "/run/s7-gpu-system"
const gpuBlobPath = "system/vendor/lib64/egl/libGLES_mali.so"
const gpuBlobSHA256 = "c9803ca74aa6b5d0c25fbad045483cf4342c717cd8f62b771a7b46c1bd4c0500"
const gpuBlobBytes = 30873936
const gpuSystemMountOptions = "noload,context=u:object_r:s7_gpu_system:s0"

var lineageSystemUUID = [16]byte{0xd3, 0x92, 0xd8, 0xbb, 0x6c, 0x67, 0x58, 0xf7, 0xb0, 0xb2, 0x09, 0x1a, 0xd4, 0x6c, 0x5b, 0xc2}

func verifyGPUBlob(root string, size int64, want string) error {
	path := filepath.Join(root, gpuBlobPath)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != size {
		return fmt.Errorf("invalid Mali blob size or file type: %v", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("Mali blob SHA256 mismatch: %s", got)
	}
	return nil
}

func mountGPUSystem() (root string, close func() error, err error) {
	if os.Getpid() != 1 {
		return "", nil, fmt.Errorf("GPU SYSTEM mount requires PID1")
	}
	device, err := systemDeviceWithIdentity(lineageSystemUUID, "/")
	if err != nil {
		return "", nil, err
	}
	if err = os.Mkdir(gpuSystemMount, 0555); err != nil && !errors.Is(err, os.ErrExist) {
		return "", nil, err
	}
	st, err := os.Lstat(gpuSystemMount)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", nil, fmt.Errorf("unsafe GPU SYSTEM mountpoint: %v", err)
	}
	flags := uintptr(syscall.MS_RDONLY | syscall.MS_NODEV | syscall.MS_NOSUID)
	if err = syscall.Mount(device, gpuSystemMount, "ext4", flags, gpuSystemMountOptions); err != nil {
		return "", nil, err
	}
	close = func() error { return syscall.Unmount(gpuSystemMount, 0) }
	if err = verifyGPUBlob(gpuSystemMount, gpuBlobBytes, gpuBlobSHA256); err != nil {
		return "", nil, errors.Join(err, close())
	}
	return gpuSystemMount, close, nil
}

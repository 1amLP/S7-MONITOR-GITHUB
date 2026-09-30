//go:build linux && (amd64 || arm64)

package camera

import (
	"fmt"
	"strings"
	"syscall"

	"perimode/native/internal/camera/fimcdma"
	"perimode/native/pkg/kernelpin"
)

// These releases share the camera/ION/UAPI source trees and embedded config.
// 481 is e418's direct child; only the private USB UVC lifecycle changed.
// fd26 is e418 with the DWC3 optional-resume callback guard only.
// 3dfe changes only non-DRM H.264 decoder source cache synchronization.
// See hardware/source-config/camera-kernel-abi-evidence.json. This admission
// does not claim UVC producer-close isolation or successful sensor capture.
var cameraKernelReleases = [...]string{
	"3.18.140-g481bdb278a10",
	"3.18.140-ge41817ea9198",
	kernelpin.USBWakeupFix,
	kernelpin.MFCCacheFix,
}

func requireRunningCameraKernel(abi string) error {
	if e := fimcdma.ValidateKernelABI(abi); e != nil {
		return e
	}
	if abi == "" {
		return nil
	}
	var u syscall.Utsname
	if e := syscall.Uname(&u); e != nil {
		return e
	}
	text := func(a []int8) string {
		b := make([]byte, 0, len(a))
		for _, v := range a {
			if v == 0 {
				break
			}
			b = append(b, byte(v))
		}
		return string(b)
	}
	return matchCameraKernel(text(u.Sysname[:]), text(u.Release[:]), text(u.Machine[:]))
}
func matchCameraKernel(system, release, machine string) error {
	if system == "Linux" && machine == "aarch64" {
		for _, version := range cameraKernelReleases {
			if release == version {
				return nil
			}
		}
	}
	return fmt.Errorf("native camera requires verified herolte camera ABI (%s)/aarch64; found %s/%s/%s", strings.Join(cameraKernelReleases[:], ", "), system, release, machine)
}

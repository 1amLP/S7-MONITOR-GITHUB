//go:build linux && (amd64 || arm64)

package fimcdma

import (
	"fmt"
	"perimode/native/internal/media"
	"unsafe"
)

// Herolte481ABI names the camera ABI shared by the verified 481 and e418 builds,
// not the whole kernel or its USB lifecycle. It is not a general V4L2 mode.
// The owner verifies an exact release and sysfs/dev_t before opening nodes.
// See hardware/source-config/camera-kernel-abi-evidence.json for source/config
// identity and CAMERA_PROFILES_RU.md for the QUERYCAP/G_FMT callbacks.
const Herolte481ABI = "herolte-3.18.140-481bdb278a10"

func ValidateKernelABI(abi string) error {
	if abi != "" && abi != Herolte481ABI {
		return fmt.Errorf("unknown camera kernel ABI %q", abi)
	}
	return nil
}
func queryNodeIdentityABI(driver, card string, kind uint32, abi string, call func(uintptr, unsafe.Pointer) error) error {
	if e := ValidateKernelABI(abi); e != nil {
		return e
	}
	if abi == "" {
		return queryNodeIdentity(driver, card, kind, call)
	}
	if driver == "" || card == "" || len(driver) >= 16 || len(card) >= 32 || call == nil || (kind != 0 && kind != media.Output && kind != media.Capture) {
		return fmt.Errorf("invalid pinned FIMC identity/type")
	}
	// The pinned preprocessor's ioctl table has a NULL QUERYCAP callback.
	// Do not issue an unsupported ioctl and then hide its error. Its exact sysfs
	// identity/dev_t have already been validated by the owner before this call.
	if kind == 0 && card == "exynos-fimc-is-pre0" {
		return nil
	}
	// e418's ISP QUERYCAP casts video_drvdata(file) to fimc_is_core, but
	// fimc_is_video_probe stores a fimc_is_video there. Dereferencing pdev
	// panics in the kernel. r48 pmsg ends at this exact ioctl on video130.
	// The owning open already verifies the node name and dev_t against sysfs;
	// use that identity for these two pinned ISP nodes, not the broken callback.
	if driver == "exynos-fimc-is" && kind == media.Output &&
		(card == "exynos-fimc-is-i0s" || card == "exynos-fimc-is-i1s") {
		return nil
	}
	var caps media.Capability
	// Preserve actual ioctl errors. Empty capabilities are expected, not proof of
	// identity: sensor/3AA/MCSC callbacks are MOV W0,#0; RET.
	// Identity comes from the prior sysfs+dev_t gate.
	if e := call(media.QueryCap, unsafe.Pointer(&caps)); e != nil {
		return fmt.Errorf("pinned FIMC QUERYCAP: %w", e)
	}
	return nil
}

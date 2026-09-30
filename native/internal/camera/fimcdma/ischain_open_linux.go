//go:build linux && (amd64 || arm64)

package fimcdma

import (
	"fmt"
	"unsafe"

	"perimode/native/internal/linuxio"
	"perimode/native/internal/media"
)

// The e418/481 FIMC-IS kernel completes ischain_open_wrap(true) only after
// this control. Group leaders must already be open, and S_INPUT must follow.
const EndOfStreamCID uint32 = 0x009a103a
const HALVersionCID uint32 = 0x009a1386
const cameraHAL32 int32 = 1

func CompleteISChainOpen(fd int, driver, card, abi string) error {
	if err := verifyVideoFD(fd); err != nil {
		return err
	}
	return completeISChainOpen(driver, card, abi, func(r uintptr, p unsafe.Pointer) error {
		return linuxio.Ioctl(fd, r, p)
	})
}

func completeISChainOpen(driver, card, abi string, call func(uintptr, unsafe.Pointer) error) error {
	if abi != Herolte481ABI || driver != "exynos-fimc-is" || card != "exynos-fimc-is-30s" || call == nil {
		return fmt.Errorf("pinned 3AA leader required for FIMC chain completion")
	}
	if err := queryNodeIdentityABI(driver, card, media.Output, abi, call); err != nil {
		return err
	}
	ctrl := media.Control{ID: EndOfStreamCID}
	if err := call(media.SetControl, unsafe.Pointer(&ctrl)); err != nil {
		return fmt.Errorf("FIMC END_OF_STREAM on 3AA leader: %w", err)
	}
	if ctrl.ID != EndOfStreamCID || ctrl.Value != 0 {
		return fmt.Errorf("FIMC changed END_OF_STREAM control")
	}
	// Resource open defaults to HAL 1.0. Select the Camera2 request scheduler
	// and stock HAL 3.2 DVFS table after open, before S_INPUT/STREAMON.
	ctrl = media.Control{ID: HALVersionCID, Value: cameraHAL32}
	if err := call(media.SetControl, unsafe.Pointer(&ctrl)); err != nil {
		return fmt.Errorf("FIMC Camera2 HAL version: %w", err)
	}
	if ctrl.ID != HALVersionCID || ctrl.Value != cameraHAL32 {
		return fmt.Errorf("FIMC changed HAL version control")
	}
	return nil
}

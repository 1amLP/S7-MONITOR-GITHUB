//go:build linux && (amd64 || arm64)

package fimcdma

import (
	"fmt"
	"perimode/native/internal/linuxio"
	"unsafe"
)

// ConfigureRouteOnly borrows an exclusive descriptor. No S_FMT, REQBUFS or
// STREAMON is issued: an OTF route must not acquire a fictional DMA queue.
func ConfigureRouteOnly(fd int, driver, card, abi string, route Route) error {
	if err := verifyVideoFD(fd); err != nil {
		return err
	}
	return configureRouteOnly(driver, card, abi, route, func(r uintptr, p unsafe.Pointer) error { return linuxio.Ioctl(fd, r, p) })
}
func configureRouteOnly(driver, card, abi string, route Route, call func(uintptr, unsafe.Pointer) error) error {
	selector, err := route.Selector()
	if err != nil {
		return err
	}
	if route.Connection != 1 || route.Reprocessing {
		return fmt.Errorf("only streaming OTF route is supported")
	}
	if err = queryNodeIdentityABI(driver, card, 0, abi, call); err != nil {
		return err
	}
	sent := selector
	if err = call(SetInput, unsafe.Pointer(&sent)); err != nil {
		return fmt.Errorf("OTF route S_INPUT: %w", err)
	}
	if sent != selector {
		return fmt.Errorf("driver altered OTF route")
	}
	return nil
}

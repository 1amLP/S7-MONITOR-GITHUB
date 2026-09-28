// Package usb provides high-level control for USB Gadget functions
// and UDC (USB Device Controller) management.
package usb


import (
	"os"
	"path/filepath"
	"strings"
)

func FindUDC() (string, error) {
	entries, err := os.ReadDir("/sys/class/udc")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		return entry.Name(), nil // return the first one
	}
	return "", os.ErrNotExist
}

func FindDevicePath(name string) (string, error) {
	// Look through /dev/hidg*
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "hidg") {
			return filepath.Join("/dev", entry.Name()), nil
		}
	}
	return "", os.ErrNotExist
}

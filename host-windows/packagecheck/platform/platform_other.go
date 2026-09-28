//go:build !windows

package platform

import (
	"errors"
	"s7.local/packagecheck/audit"
)

func Environment() (audit.Environment, error) {
	return audit.Environment{}, errors.New("native Windows required; --static permits content-only development checks")
}
func Trust() audit.Trust { return nil }

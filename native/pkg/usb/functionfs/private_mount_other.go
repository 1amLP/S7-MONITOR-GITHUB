//go:build !linux

package functionfs

import "errors"

func MountPrivate(name, path, fileSystem string) (func() error, error) {
	return nil, errors.New("private FunctionFS mounts require Linux")
}

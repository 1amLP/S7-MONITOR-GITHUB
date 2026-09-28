//go:build !linux

package functionfs

import (
	"errors"
	"os"
)

func acknowledgeEmptyRead(file *os.File) error {
	return errors.New("FunctionFS control transfers require Linux")
}

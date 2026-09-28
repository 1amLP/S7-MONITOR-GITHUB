package functionfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Endpoint struct {
	file *os.File
	path string
}

func OpenEndpoint(mountPath string, num int, out bool) (*Endpoint, error) {
	return openEndpoint(mountPath, num, out, 0)
}

// OpenEndpointNonblocking avoids waiting for a disabled endpoint to be enabled.
// An already queued synchronous transfer still requires UDC unbind to stop it.
func OpenEndpointNonblocking(mountPath string, num int, out bool) (*Endpoint, error) {
	if nonblockFlag == 0 {
		return nil, errors.New("nonblocking FunctionFS requires Linux")
	}
	return openEndpoint(mountPath, num, out, nonblockFlag)
}

func openEndpoint(mountPath string, num int, out bool, flags int) (*Endpoint, error) {
	name := fmt.Sprintf("ep%d", num)
	path := filepath.Join(mountPath, name)
	mode := os.O_RDWR
	if out {
		mode = os.O_RDONLY
	} else {
		mode = os.O_WRONLY
	}

	f, err := os.OpenFile(path, mode|flags, 0)
	if err != nil {
		return nil, err
	}
	return &Endpoint{file: f, path: path}, nil
}

func (e *Endpoint) Close() error {
	return e.file.Close()
}

func (e *Endpoint) Read(buf []byte) (int, error) {
	return endpointIO(e.file, buf, true)
}

func (e *Endpoint) Write(buf []byte) (int, error) {
	return endpointIO(e.file, buf, false)
}

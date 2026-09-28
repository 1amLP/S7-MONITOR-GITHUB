//go:build !linux

package functionfs

import "os"

func endpointIO(file *os.File, data []byte, reading bool) (int, error) {
	if reading {
		return file.Read(data)
	}
	return file.Write(data)
}

func controlIO(file *os.File, data []byte, reading bool) (int, error) {
	return endpointIO(file, data, reading)
}

//go:build linux && (amd64 || arm64)

package mediacodec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"syscall"
)

const SelectionPath = "/etc/s7-codec/selection.json"

type Selection struct {
	Monitor string `json:"monitor"`
	Camera  string `json:"camera"`
}

func DefaultSelection() Selection { return Selection{Monitor: "mediacodec", Camera: "mediacodec"} }
func ParseSelection(b []byte) (Selection, error) {
	var s Selection
	if len(b) > 1024 {
		return s, fmt.Errorf("oversized codec selection")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return s, fmt.Errorf("invalid codec selection object")
	}
	seen := map[string]bool{}
	for d.More() {
		key, e := d.Token()
		if e != nil {
			return s, e
		}
		name, ok := key.(string)
		if !ok || seen[name] || (name != "monitor" && name != "camera") {
			return s, fmt.Errorf("duplicate/unknown codec selection field")
		}
		seen[name] = true
		var value string
		if e = d.Decode(&value); e != nil {
			return s, e
		}
		if value != "mfc" && value != "mediacodec" {
			return s, fmt.Errorf("unsupported codec backend; no software fallback")
		}
		if name == "monitor" {
			s.Monitor = value
		} else {
			s.Camera = value
		}
	}
	if _, e = d.Token(); e != nil {
		return s, e
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF || len(seen) != 2 {
		return s, fmt.Errorf("incomplete/trailing codec selection data")
	}
	return s, nil
}
func LoadSelection() (Selection, error) {
	// Even a substituted FIFO or symlink must not stall the startup/control task.
	fd, e := syscall.Open(SelectionPath, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(e) {
		return DefaultSelection(), nil
	}
	if e != nil {
		return Selection{}, e
	}
	f := os.NewFile(uintptr(fd), SelectionPath)
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return Selection{}, e
	}
	if !st.Mode().IsRegular() {
		return Selection{}, fmt.Errorf("codec selection is not regular")
	}
	b, e := io.ReadAll(io.LimitReader(f, 1025))
	if e != nil {
		return Selection{}, e
	}
	return ParseSelection(b)
}

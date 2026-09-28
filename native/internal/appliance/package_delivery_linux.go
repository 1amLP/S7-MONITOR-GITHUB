//go:build linux

package appliance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const packagePinPath = "/etc/s7-windows-package.json"
const packagePayloadPath = gpuSystemMount + "/s7-windows/package.zip"
const maxPackageBytes = 128 << 20
const packageChunkBytes = 64 << 10

type packagePin struct {
	MediaBytes  int64  `json:"media_bytes,omitempty"`
	MediaSHA256 string `json:"media_sha256,omitempty"`
	Schema      string `json:"schema"`
	Release     uint32 `json:"release"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
}

type packageDelivery struct {
	file *os.File
	pin  packagePin
}

func openPackageDelivery(pinPath, payloadPath string) (*packageDelivery, error) {
	if !filepath.IsAbs(pinPath) || !filepath.IsAbs(payloadPath) {
		return nil, fmt.Errorf("absolute package paths required")
	}
	fd, err := syscall.Open(pinPath, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("package pin %s: %w", pinPath, err)
	}
	pinFile := os.NewFile(uintptr(fd), pinPath)
	defer pinFile.Close()
	st, err := pinFile.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 4096 {
		return nil, fmt.Errorf("invalid package pin")
	}
	var pin packagePin
	decoder := json.NewDecoder(io.LimitReader(pinFile, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pin); err != nil {
		return nil, err
	}
	if decoder.Decode(new(any)) != io.EOF || pin.Schema != "S7_PHONE_PACKAGE_1" || pin.Release == 0 || pin.Bytes <= 0 || pin.Bytes > maxPackageBytes {
		return nil, fmt.Errorf("unsupported phone package")
	}
	digest, err := hex.DecodeString(pin.SHA256)
	if err != nil || len(digest) != 32 || hex.EncodeToString(digest) != pin.SHA256 {
		return nil, fmt.Errorf("invalid package digest")
	}
	fd, err = syscall.Open(payloadPath, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("package payload %s: %w", payloadPath, err)
	}
	f := os.NewFile(uintptr(fd), payloadPath)
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	st, err = f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != pin.Bytes {
		return nil, fmt.Errorf("package payload size/type differs")
	}
	var fs syscall.Statfs_t
	if err = syscall.Fstatfs(fd, &fs); err != nil {
		return nil, err
	}
	if fs.Flags&1 == 0 {
		return nil, fmt.Errorf("package must be on read-only SYSTEM")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return nil, err
	}
	if hex.EncodeToString(h.Sum(nil)) != pin.SHA256 {
		return nil, fmt.Errorf("package payload SHA256 differs from BOOT pin")
	}
	ok = true
	return &packageDelivery{file: f, pin: pin}, nil
}

func (p *packageDelivery) response(c debugCommand) debugResult {
	if p == nil {
		return debugErrorResult(c.Sequence, fmt.Errorf("Windows package not provisioned on S7"))
	}
	if c.Kind == 10 {
		meta, err := json.Marshal(p.pin)
		if err != nil {
			return debugErrorResult(c.Sequence, err)
		}
		return debugResult{sequence: c.Sequence, status: 2, meta: meta}
	}
	offset, count := int64(c.X), int64(c.Y)
	if c.Kind != 11 || c.Capture != p.pin.Release || offset < 0 || count < 1 || count > packageChunkBytes || offset > p.pin.Bytes-count {
		return debugErrorResult(c.Sequence, fmt.Errorf("invalid package chunk or release"))
	}
	data := make([]byte, count)
	if _, err := p.file.ReadAt(data, offset); err != nil {
		return debugErrorResult(c.Sequence, err)
	}
	meta, _ := json.Marshal(map[string]any{"release": p.pin.Release, "offset": offset, "bytes": count})
	return debugResult{sequence: c.Sequence, status: 2, meta: meta, pixels: data}
}

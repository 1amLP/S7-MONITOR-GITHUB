//go:build linux && (amd64 || arm64)

// Package mediaruntime hosts selected original native Android media services.
// The appliance's PID1, USB, sensors, firmware and thermal policy stay outside.
package mediaruntime

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

const PinPath = "/etc/s7-codec/runtime.json"
const Root = "/run/s7-media-root"
const Control = "/run/s7-media-control"
const Socket = Control + "/codec.sock"
const MaxCompressed = 128 << 20
const MaxUnpacked = 384 << 20

type Pin struct {
	Schema           string `json:"schema"`
	SHA256           string `json:"sha256"`
	Bytes            int64  `json:"bytes"`
	Unpacked         int64  `json:"unpacked_bytes"`
	Members          int    `json:"members"`
	MaxUnpackedBytes int64  `json:"max_unpacked_bytes"`
	OTASHA256        string `json:"ota_sha256"`
}

func (p Pin) Validate() error {
	h, e := hex.DecodeString(p.SHA256)
	if e != nil || len(h) != 32 || p.Schema != "S7-MEDIA-BUNDLE-1" || p.Bytes < 1 || p.Bytes > MaxCompressed || p.Unpacked < 1 || p.Unpacked > MaxUnpacked || p.Members < 1 || p.Members > 4096 || p.MaxUnpackedBytes != MaxUnpacked {
		return errors.New("invalid pinned MediaCodec runtime extent")
	}
	return nil
}
func OpenRegular(name string) (*os.File, error) {
	fd, e := syscall.Open(name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), name)
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("MediaCodec input is not a regular file")
	}
	return f, nil
}
func LoadPin() (Pin, error) {
	var p Pin
	f, e := OpenRegular(PinPath)
	if e != nil {
		return p, e
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 4097))
	d.DisallowUnknownFields()
	if e = d.Decode(&p); e != nil {
		return p, e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return p, errors.New("trailing media runtime pin")
	}
	return p, p.Validate()
}
func Consent(pin Pin, serial string) string {
	return "S7-NATIVE-MEDIA-1\n" + serial + "\n" + pin.SHA256 + "\n"
}
func BundleName(pin Pin) string { return "runtime-" + pin.SHA256 + ".tar.gz" }

// OpenInstalled uses pinned names and directory-relative O_NOFOLLOW opens. The
// caller owns CACHE; this routine cannot mount/remount or write any partition.
func OpenInstalled(base string, pin Pin, serial string) (*os.File, error) {
	d, e := syscall.Open(base, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	defer syscall.Close(d)
	open := func(name string) (*os.File, error) {
		fd, e := syscall.Openat(d, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if e != nil {
			return nil, e
		}
		f := os.NewFile(uintptr(fd), name)
		s, e := f.Stat()
		if e != nil || !s.Mode().IsRegular() {
			f.Close()
			return nil, errors.New("nonregular installed media file")
		}
		return f, nil
	}
	f, e := open("consent-" + pin.SHA256)
	if e != nil {
		return nil, e
	}
	b, e := io.ReadAll(io.LimitReader(f, 257))
	f.Close()
	if e != nil || string(b) != Consent(pin, serial) {
		return nil, errors.New("media runtime installation consent absent/mismatched")
	}
	f, e = open(BundleName(pin))
	if e != nil {
		return nil, e
	}
	st, e := f.Stat()
	if e != nil || st.Size() != pin.Bytes {
		f.Close()
		return nil, errors.New("installed runtime size mismatch")
	}
	return f, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(b []byte) (int, error) {
	if e := c.ctx.Err(); e != nil {
		return 0, e
	}
	return c.r.Read(b)
}

// Extract is bounded and creates only regular files/directories in a fresh root.
// No input is executed before the COMPLETE compressed stream passes its BOOT pin.
// Source replacement during the read cannot pass a different SHA256.
func Extract(ctx context.Context, input io.Reader, root string, pin Pin) error {
	if e := pin.Validate(); e != nil {
		return e
	}
	entries, e := os.ReadDir(root)
	if e != nil {
		return e
	}
	if len(entries) != 0 {
		return errors.New("media staging root not empty")
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: contextReader{ctx, input}, N: pin.Bytes + 1}
	wire := io.TeeReader(limited, hash)
	gz, e := gzip.NewReader(wire)
	if e != nil {
		return e
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	total := int64(0)
	members := 0
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		members++
		n := strings.TrimSuffix(h.Name, "/")
		if n == "" || n == "." || len(n) > 512 || path.Clean(n) != n || strings.HasPrefix(n, "/") || strings.HasPrefix(n, "../") || strings.ContainsAny(n, "\\\x00\r\n") || seen[n] || members > pin.Members {
			return errors.New("invalid/duplicate media archive path")
		}
		seen[n] = true
		if h.Uid != 0 || h.Gid != 0 || h.Mode&^int64(0777) != 0 || h.Mode&0022 != 0 || h.Size < 0 || h.Size > 64<<20 {
			return errors.New("unsafe media entry metadata")
		}
		p := filepath.Join(root, filepath.FromSlash(n))
		if e = os.MkdirAll(filepath.Dir(p), 0755); e != nil {
			return e
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if h.Size != 0 {
				return errors.New("directory payload")
			}
			if e = os.MkdirAll(p, 0755); e != nil {
				return e
			}
		case tar.TypeReg:
			total += h.Size
			if total > pin.Unpacked {
				return errors.New("runtime exceeds pinned unpacked bound")
			}
			f, e := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(h.Mode))
			if e != nil {
				return e
			}
			_, e = io.CopyN(f, contextReader{ctx, tr}, h.Size)
			ce := f.Close()
			if e != nil {
				return e
			}
			if ce != nil {
				return ce
			}
		default:
			return errors.New("media archive links/devices/sparse/extended records refused")
		}
	}
	// Drain gzip footer/padding so a valid-looking first archive cannot hide data.
	if _, e = io.Copy(io.Discard, contextReader{ctx, gz}); e != nil {
		return e
	}
	if _, e = io.Copy(io.Discard, wire); e != nil {
		return e
	}
	if pin.Bytes+1-limited.N != pin.Bytes || hex.EncodeToString(hash.Sum(nil)) != pin.SHA256 || total != pin.Unpacked || members != pin.Members {
		return fmt.Errorf("runtime archive pin/content mismatch")
	}
	return nil
}

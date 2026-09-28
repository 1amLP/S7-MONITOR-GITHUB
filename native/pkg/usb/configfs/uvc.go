package configfs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const UVCFunctionName = "uvc.s7cam"

// UVCFunction owns only a newly allocated child of an already validated gadget.
// The caller must unbind before creating it and remove its config link before cleanup.
type UVCFunction struct {
	Path    string
	gadget  string
	files   fileOps
	created []string
}

func CreateUVC(gadget string) (*UVCFunction, error) {
	return createUVC(gadget, systemFiles{})
}

func createUVC(gadget string, files fileOps) (*UVCFunction, error) {
	u := &UVCFunction{Path: filepath.Join(gadget, "functions", UVCFunctionName), gadget: gadget, files: files}
	if !filepath.IsAbs(gadget) {
		return nil, errors.New("absolute gadget path required")
	}
	if err := u.unbound(); err != nil {
		return nil, err
	}
	if err := u.mkdir(""); err != nil {
		return nil, err
	}
	fail := func(err error) (*UVCFunction, error) { return nil, errors.Join(err, u.Close()) }
	for _, name := range []string{"control/header/h", "streaming/uncompressed/yuy2", "streaming/uncompressed/yuy2/480p", "streaming/header/h"} {
		if err := u.mkdir(name); err != nil {
			return fail(err)
		}
	}
	for _, pair := range [][2]string{
		{"streaming_interval", "1"}, {"streaming_maxpacket", "2048"}, {"streaming_maxburst", "0"},
		{"control/header/h/bcdUVC", "0x0100"}, {"control/header/h/dwClockFrequency", "48000000"},
		{"streaming/uncompressed/yuy2/480p/wWidth", "640"}, {"streaming/uncompressed/yuy2/480p/wHeight", "480"},
		{"streaming/uncompressed/yuy2/480p/dwMinBitRate", "73728000"}, {"streaming/uncompressed/yuy2/480p/dwMaxBitRate", "73728000"},
		{"streaming/uncompressed/yuy2/480p/dwMaxVideoFrameBufferSize", "614400"},
		{"streaming/uncompressed/yuy2/480p/dwDefaultFrameInterval", "666666"}, {"streaming/uncompressed/yuy2/480p/dwFrameInterval", "666666"},
	} {
		if err := files.WriteAttr(filepath.Join(u.Path, pair[0]), []byte(pair[1]+"\n")); err != nil {
			return fail(err)
		}
	}
	guid := []byte{'Y', 'U', 'Y', '2', 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}
	if err := files.WriteAttr(filepath.Join(u.Path, "streaming/uncompressed/yuy2/guidFormat"), guid); err != nil {
		return fail(err)
	}
	got, err := files.ReadFile(filepath.Join(u.Path, "streaming/uncompressed/yuy2/guidFormat"))
	if err != nil || !bytes.Equal(got, guid) {
		return fail(errors.New("UVC format GUID readback failed"))
	}
	for _, pair := range [][2]string{
		{"control/header/h", "control/class/fs/h"}, {"control/header/h", "control/class/ss/h"},
		{"streaming/uncompressed/yuy2", "streaming/header/h/yuy2"},
		{"streaming/header/h", "streaming/class/fs/h"}, {"streaming/header/h", "streaming/class/hs/h"}, {"streaming/header/h", "streaming/class/ss/h"},
	} {
		path := filepath.Join(u.Path, pair[1])
		if err := files.Symlink(filepath.Join(u.Path, pair[0]), path); err != nil {
			return fail(err)
		}
		u.created = append(u.created, path)
	}
	return u, nil
}

func (u *UVCFunction) unbound() error {
	data, err := u.files.ReadFile(filepath.Join(u.gadget, "UDC"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "" {
		return errors.New("UVC changes require an unbound gadget")
	}
	return nil
}
func (u *UVCFunction) mkdir(name string) error {
	path := filepath.Join(u.Path, name)
	if err := u.files.Mkdir(path, 0755); err != nil {
		return err
	}
	u.created = append(u.created, path)
	return nil
}
func (u *UVCFunction) Close() error {
	if len(u.created) == 0 {
		return nil
	}
	if err := u.unbound(); err != nil {
		return err
	}
	for len(u.created) > 0 {
		i := len(u.created) - 1
		path := u.created[i]
		if err := u.files.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove UVC object %s: %w", path, err)
		}
		u.created = u.created[:i]
	}
	return nil
}

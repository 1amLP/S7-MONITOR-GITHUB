package configfs

import (
	"bytes"
	"errors"
	"fmt"
	"perimode/native/pkg/usb/uvcmode"
	"path/filepath"
	"strings"
)

// CreateAvailableH264UVC advertises only a provider-admitted table. The table is
// immutable while this USB function exists; selecting a mode never unbinds USB.
// Unlike the older compatibility builder, this path cannot create YUY2 fallback.
func CreateAvailableH264UVC(gadget string, table uvcmode.Table) (*UVCFunction, error) {
	return createAvailableH264UVC(gadget, table, systemFiles{})
}
func createAvailableH264UVC(gadget string, table uvcmode.Table, files fileOps) (*UVCFunction, error) {
	frames := table.Frames()
	if len(frames) == 0 || !filepath.IsAbs(gadget) {
		return nil, errors.New("supported frame table and absolute gadget path required")
	}
	u := &UVCFunction{Path: filepath.Join(gadget, "functions", UVCFunctionName), gadget: gadget, files: files}
	if err := u.unbound(); err != nil {
		return nil, err
	}
	if err := u.mkdir(""); err != nil {
		return nil, err
	}
	fail := func(e error) (*UVCFunction, error) { return nil, errors.Join(e, u.Close()) }
	const format = "streaming/framebased/h264"
	for _, name := range []string{"control/header/h", format, "streaming/header/h"} {
		if err := u.mkdir(name); err != nil {
			return fail(err)
		}
	}
	guid := []byte{'H', '2', '6', '4', 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}
	actual, err := files.ReadFile(filepath.Join(u.Path, format, "guidFormat"))
	if err != nil || !bytes.Equal(actual, guid) {
		return fail(errors.New("H264 kernel GUID mismatch"))
	}
	// Keep payload size identical to the responder. 2048 includes two HS transactions.
	attrs := [][2]string{{"streaming_interval", "1"}, {"streaming_maxpacket", "2048"}, {"streaming_maxburst", "0"}, {"control/header/h/bcdUVC", "0x0110"}, {"control/header/h/dwClockFrequency", "48000000"}}
	for _, pair := range attrs {
		if err := files.WriteAttr(filepath.Join(u.Path, pair[0]), []byte(pair[1]+"\n")); err != nil {
			return fail(err)
		}
	}
	for _, frame := range frames {
		path := fmt.Sprintf("%s/frame%d", format, frame.Index)
		if err := u.mkdir(path); err != nil {
			return fail(err)
		}
		var intervals []string
		for _, fps := range frame.FPS {
			intervals = append(intervals, fmt.Sprint(10_000_000/fps))
		}
		attrs := [][2]string{{"wWidth", fmt.Sprint(frame.Width)}, {"wHeight", fmt.Sprint(frame.Height)}, {"dwMinBitRate", "1000000"}, {"dwMaxBitRate", "60000000"}, {"dwMaxVideoFrameBufferSize", "4194304"}, {"dwDefaultFrameInterval", intervals[len(intervals)-1]}, {"dwFrameInterval", strings.Join(intervals, "\n")}}
		for _, pair := range attrs {
			if err := files.WriteAttr(filepath.Join(u.Path, path, pair[0]), []byte(pair[1]+"\n")); err != nil {
				return fail(err)
			}
		}
	}
	for _, pair := range [][2]string{{"control/header/h", "control/class/fs/h"}, {"control/header/h", "control/class/ss/h"}, {format, "streaming/header/h/h264"}, {"streaming/header/h", "streaming/class/fs/h"}, {"streaming/header/h", "streaming/class/hs/h"}, {"streaming/header/h", "streaming/class/ss/h"}} {
		path := filepath.Join(u.Path, pair[1])
		if err := files.Symlink(filepath.Join(u.Path, pair[0]), path); err != nil {
			return fail(err)
		}
		u.created = append(u.created, path)
	}
	return u, nil
}

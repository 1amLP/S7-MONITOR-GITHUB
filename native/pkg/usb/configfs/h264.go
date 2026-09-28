package configfs

import (
	"bytes"
	"errors"
	"fmt"
	"perimode/native/pkg/usb/uvcmode"
	"path/filepath"
)

type H264Mode struct{ Width, Height, FPS, Bitrate int }

func H264Preset(name string) (H264Mode, error) {
	switch name {
	case "rear-1440p30", "front-1440p30", "h264-1440p30":
		return H264Mode{2560, 1440, 30, 16000000}, nil
	case "rear-1080p60", "h264-1080p60":
		return H264Mode{1920, 1080, 60, 16000000}, nil
	case "rear-1080p30", "front-1080p30", "h264-1080p30":
		return H264Mode{1920, 1080, 30, 8000000}, nil
	case "rear-720p60", "h264-720p60":
		return H264Mode{1280, 720, 60, 8000000}, nil
	case "rear-720p30", "front-720p30", "h264-720p30":
		return H264Mode{1280, 720, 30, 4000000}, nil
	default:
		return H264Mode{}, errors.New("removed, hidden or unsupported H264 preset")
	}
}

func (m H264Mode) PacketSize() int {
	if m.FPS >= 60 {
		return 2048
	}
	return 1024
}
func CreateH264UVC(gadget, preset string) (*UVCFunction, error) {
	return createH264UVC(gadget, preset, systemFiles{})
}
func CreateStaticH264UVC(gadget, preset string) (*UVCFunction, error) {
	return createEncodedUVCModes(gadget, preset, systemFiles{}, false, true)
}
func createH264UVC(gadget, preset string, files fileOps) (*UVCFunction, error) {
	return createEncodedUVC(gadget, preset, files, false)
}
func CreateMixedUVC(gadget, preset string) (*UVCFunction, error) {
	return createEncodedUVC(gadget, preset, systemFiles{}, true)
}
func createEncodedUVC(gadget, preset string, files fileOps, compatible bool) (*UVCFunction, error) {
	return createEncodedUVCModes(gadget, preset, files, compatible, false)
}
func createEncodedUVCModes(gadget, preset string, files fileOps, compatible, multimode bool) (*UVCFunction, error) {
	selected, err := H264Preset(preset)
	if err != nil {
		return nil, err
	}
	if multimode {
		table, e := uvcmode.New([]uvcmode.Mode{{Width: 1280, Height: 720, FPS: 30}, {Width: 1280, Height: 720, FPS: 60}, {Width: 1920, Height: 1080, FPS: 30}, {Width: 1920, Height: 1080, FPS: 60}, {Width: 2560, Height: 1440, FPS: 30}})
		if e != nil {
			return nil, e
		}
		return createAvailableH264UVC(gadget, table, files)
	}
	if !filepath.IsAbs(gadget) {
		return nil, errors.New("absolute gadget path required")
	}
	u := &UVCFunction{Path: filepath.Join(gadget, "functions", UVCFunctionName), gadget: gadget, files: files}
	if err := u.unbound(); err != nil {
		return nil, err
	}
	if err := u.mkdir(""); err != nil {
		return nil, err
	}
	fail := func(err error) (*UVCFunction, error) { return nil, errors.Join(err, u.Close()) }
	const format = "streaming/framebased/h264"
	for _, name := range []string{"control/header/h", format, "streaming/header/h"} {
		if err := u.mkdir(name); err != nil {
			return fail(err)
		}
	}
	guid := []byte{'H', '2', '6', '4', 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}
	actual, err := files.ReadFile(filepath.Join(u.Path, format, "guidFormat"))
	if err != nil || !bytes.Equal(actual, guid) {
		return fail(errors.New("H264 kernel GUID does not match the private backport"))
	}
	packet := selected.PacketSize()
	if compatible || multimode {
		packet = 2048
	}
	for _, p := range [][2]string{
		{"streaming_interval", "1"}, {"streaming_maxpacket", fmt.Sprint(packet)}, {"streaming_maxburst", "0"},
		{"control/header/h/bcdUVC", "0x0110"}, {"control/header/h/dwClockFrequency", "48000000"},
	} {
		if err := files.WriteAttr(filepath.Join(u.Path, p[0]), []byte(p[1]+"\n")); err != nil {
			return fail(err)
		}
	}
	// Fixed descriptors: choosing a camera mode must never unbind the monitor.
	modes := []H264Mode{selected}

	for i, m := range modes {
		frame := fmt.Sprintf("%s/frame%d", format, i+1)
		if err := u.mkdir(frame); err != nil {
			return fail(err)
		}
		intervals := fmt.Sprint(10000000 / m.FPS)

		maxBitrate := m.Bitrate * 2
		if multimode {
			maxBitrate = 60000000
		} // Native UI permits up to 60 Mbit/s.
		for _, p := range [][2]string{
			{"wWidth", fmt.Sprint(m.Width)}, {"wHeight", fmt.Sprint(m.Height)},
			{"dwMinBitRate", "1000000"}, {"dwMaxBitRate", fmt.Sprint(maxBitrate)},
			{"dwMaxVideoFrameBufferSize", "4194304"}, {"dwDefaultFrameInterval", fmt.Sprint(10000000 / m.FPS)}, {"dwFrameInterval", intervals},
		} {
			if err := files.WriteAttr(filepath.Join(u.Path, frame, p[0]), []byte(p[1]+"\n")); err != nil {
				return fail(err)
			}
		}
	}
	if compatible {
		const raw = "streaming/uncompressed/yuy2"
		for _, name := range []string{raw, raw + "/480p"} {
			if err := u.mkdir(name); err != nil {
				return fail(err)
			}
		}
		rawGUID := []byte{'Y', 'U', 'Y', '2', 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}
		if err := files.WriteAttr(filepath.Join(u.Path, raw, "guidFormat"), rawGUID); err != nil {
			return fail(err)
		}
		got, err := files.ReadFile(filepath.Join(u.Path, raw, "guidFormat"))
		if err != nil || !bytes.Equal(got, rawGUID) {
			return fail(errors.New("compatible format GUID mismatch"))
		}
		for _, p := range [][2]string{{"wWidth", "640"}, {"wHeight", "480"}, {"dwMinBitRate", "73728000"}, {"dwMaxBitRate", "73728000"}, {"dwMaxVideoFrameBufferSize", "614400"}, {"dwDefaultFrameInterval", "666666"}, {"dwFrameInterval", "666666"}} {
			if err := files.WriteAttr(filepath.Join(u.Path, raw, "480p", p[0]), []byte(p[1]+"\n")); err != nil {
				return fail(err)
			}
		}
	}
	links := [][2]string{
		{"control/header/h", "control/class/fs/h"}, {"control/header/h", "control/class/ss/h"},
		{format, "streaming/header/h/h264"},
	}
	if compatible {
		links = append(links, [2]string{"streaming/uncompressed/yuy2", "streaming/header/h/yuy2"})
	}
	links = append(links, [][2]string{{"streaming/header/h", "streaming/class/fs/h"},
		{"streaming/header/h", "streaming/class/hs/h"}, {"streaming/header/h", "streaming/class/ss/h"},
	}...)
	for _, p := range links {
		path := filepath.Join(u.Path, p[1])
		if err := files.Symlink(filepath.Join(u.Path, p[0]), path); err != nil {
			return fail(err)
		}
		u.created = append(u.created, path)
	}
	return u, nil
}

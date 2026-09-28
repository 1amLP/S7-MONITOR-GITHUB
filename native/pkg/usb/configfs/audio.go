package configfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const AudioFunctionName = "uac2.s7audio"

// AudioFunction owns one newly created function, never the containing gadget.
type AudioFunction struct {
	Path   string
	gadget string
	files  fileOps
	owned  bool
}

func CreateAudio(gadget string) (*AudioFunction, error) {
	return createAudio(gadget, systemFiles{})
}

func CreateMonoAudio(gadget string) (*AudioFunction, error) {
	return createAudioChannels(gadget, systemFiles{}, "4")
}

func createAudio(gadget string, files fileOps) (*AudioFunction, error) {
	return createAudioChannels(gadget, files, "3")
}

func createAudioChannels(gadget string, files fileOps, playbackMask string) (*AudioFunction, error) {
	if playbackMask != "3" && playbackMask != "4" {
		return nil, errors.New("only mono-center or stereo playback supported")
	}
	if !filepath.IsAbs(gadget) {
		return nil, errors.New("absolute gadget path required")
	}
	a := &AudioFunction{Path: filepath.Join(gadget, "functions", AudioFunctionName), gadget: gadget, files: files}
	if err := a.unbound(); err != nil {
		return nil, err
	}
	if err := files.Mkdir(a.Path, 0755); err != nil {
		return nil, err
	}
	a.owned = true
	fail := func(err error) (*AudioFunction, error) { return nil, errors.Join(err, a.Close()) }
	// Legacy kernels require stereo; the mono kernel accepts center-mono too.
	for _, pair := range [][2]string{
		{"p_chmask", "1"}, {"p_srate", "48000"}, {"p_ssize", "2"},
		{"c_chmask", playbackMask}, {"c_srate", "48000"}, {"c_ssize", "2"},
		{"c_sync", "async"}, {"req_number", "8"}, {"fb_max", "5"},
		{"p_mute_present", "0"}, {"p_volume_present", "0"},
		{"c_mute_present", "0"}, {"c_volume_present", "0"},
	} {
		path := filepath.Join(a.Path, pair[0])
		if err := files.WriteAttr(path, []byte(pair[1]+"\n")); err != nil {
			return fail(err)
		}
		got, err := files.ReadFile(path)
		if err != nil {
			return fail(fmt.Errorf("read audio attribute %s: %w", pair[0], err))
		}
		if strings.TrimSpace(string(got)) != pair[1] {
			return fail(fmt.Errorf("audio attribute %s readback mismatch: %q", pair[0], got))
		}
	}
	// These attributes distinguish the hardened S7 backport from the old driver.
	if _, _, err := a.Activity(); err != nil {
		return fail(err)
	}
	return a, nil
}

func (a *AudioFunction) unbound() error {
	data, err := a.files.ReadFile(filepath.Join(a.gadget, "UDC"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "" {
		return errors.New("audio changes require an unbound gadget")
	}
	return nil
}

// Activity reports host stream intent, not proof of local hardware capture.
func (a *AudioFunction) Activity() (microphone, speaker bool, err error) {
	if !a.owned {
		return false, false, errors.New("audio function is closed")
	}
	values := [2]bool{}
	for i, name := range []string{"p_active", "c_active"} {
		data, err := a.files.ReadFile(filepath.Join(a.Path, name))
		if err != nil {
			return false, false, err
		}
		switch strings.TrimSpace(string(data)) {
		case "0":
		case "1":
			values[i] = true
		default:
			return false, false, fmt.Errorf("invalid audio stream state: %s", name)
		}
	}
	return values[0], values[1], nil
}

func (a *AudioFunction) Close() error {
	if !a.owned {
		return nil
	}
	if err := a.unbound(); err != nil {
		return err
	}
	if err := a.files.Remove(a.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	a.owned = false
	return nil
}

// CreateAudioFunction records ownership in the same list as other functions.
// AudioFunction is not closed independently after this call: Gadget.Delete owns
// unlink/remove ordering and never deletes unrelated kernel objects.
func (g *Gadget) CreateAudioFunction() (*AudioFunction, error) {
	if err := g.requireOwner(); err != nil {
		return nil, err
	}
	a, err := createAudio(g.Path, g.fs())
	if err != nil {
		return nil, err
	}
	g.created = append(g.created, a.Path)
	return a, nil
}

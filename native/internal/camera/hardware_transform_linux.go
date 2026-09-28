//go:build linux && (amd64 || arm64)

package camera

import (
	"errors"
	"perimode/native/internal/media"
)

type hardwareTransformSource struct {
	source  Source
	scaler  frameTransformer
	mode    Mode
	options ImageOptions
	zoom    func() int
	open    func(Mode, ImageOptions) (frameTransformer, error)
}

type frameTransformer interface {
	SetZoom(int) error
	Apply(media.Image) (media.Image, error)
	Close() error
}

func openFrameTransformer(mode Mode, options ImageOptions) (frameTransformer, error) {
	return media.NewNV12Transformer(int(mode.Width), int(mode.Height), int(options.Rotation), options.Mirror, options.Zoom())
}

func WrapHardwareTransform(source Source, mode Mode, options ImageOptions) (Source, error) {
	return WrapLiveHardwareTransform(source, mode, options, nil)
}

func WrapLiveHardwareTransform(source Source, mode Mode, options ImageOptions, zoom func() int) (Source, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if options.Rotation == 0 && !options.Mirror && options.Zoom() == 100 && zoom == nil {
		return source, nil
	}
	return &hardwareTransformSource{source: source, mode: mode, options: options, zoom: zoom, open: openFrameTransformer}, nil
}
func (s *hardwareTransformSource) Drain(emit func(media.Image) error) (int, error) {
	return s.source.Drain(func(im media.Image) error {
		options := s.options
		if s.zoom != nil {
			value := s.zoom()
			if value < 100 || value > 400 {
				return errors.New("invalid live camera zoom")
			}
			options.ZoomPercent = uint16(value)
		}
		if options.Rotation == 0 && !options.Mirror && options.Zoom() == 100 {
			return emit(im)
		}
		if s.scaler == nil {
			var err error
			s.scaler, err = s.open(s.mode, options)
			if err != nil {
				return err
			}
		}
		if err := s.scaler.SetZoom(options.Zoom()); err != nil {
			return err
		}
		next, err := s.scaler.Apply(im)
		if err != nil {
			return err
		}
		defer next.Lease.Release()
		return emit(next)
	})
}
func (s *hardwareTransformSource) Close() error {
	var err error
	if s.scaler != nil {
		err = s.scaler.Close()
	}
	return errors.Join(err, s.source.Close())
}

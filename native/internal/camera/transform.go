package camera

import (
	"fmt"
	"perimode/native/internal/media"
)

// ImageOptions changes pixels, not the sensor mode or the dimensions advertised
// over USB. Quarter turns are fitted without cropping into the selected canvas.
// Padding is limited-range NV12 black (Y=16, U=V=128). The path is CPU processing,
// not an assertion of hardware rotation or sustained 120 FPS.
type ImageOptions struct {
	Rotation uint16 `json:"rotation"`
	Mirror   bool   `json:"mirror"`
	// Zero is legacy 1x. Nonzero values are explicit digital center-crop zoom.
	ZoomPercent uint16 `json:"zoom_percent,omitempty"`
}

// Manual camera orientation is a correction to the current panel orientation.
// Front module21 was verified upside down with mount=270 on the r55 capture.
func PreviewRotation(sensor Sensor, panel, correction int) int {
	mount := 90
	return (mount - panel + correction + 720) % 360
}

func (o ImageOptions) Zoom() int {
	if o.ZoomPercent == 0 {
		return 100
	}
	return int(o.ZoomPercent)
}

func (o ImageOptions) Validate() error {
	if o.Zoom() < 100 || o.Zoom() > 400 {
		return fmt.Errorf("digital zoom must be 100..400 percent")
	}
	if o.Rotation != 0 && o.Rotation != 90 && o.Rotation != 180 && o.Rotation != 270 {
		return fmt.Errorf("camera rotation must be 0, 90, 180 or 270 degrees")
	}
	return nil
}

type ImageRect struct{ X, Y, Width, Height int }

// Transform is owned by one capture worker. The returned image is valid until
// the next Apply and must be consumed synchronously, like a Source callback.
// A disabled transform returns the original DMA-backed image without a copy.
type Transform struct {
	width, height            int
	options                  ImageOptions
	rect                     ImageRect
	crop                     ImageRect
	xmap, ymap               []int
	xoff, yoff, uxoff, uyoff []int
	strideY, strideUV        int
	data                     []byte
}

func NewTransform(width, height int, o ImageOptions) (*Transform, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if width < 2 || height < 2 || width > 4096 || height > 4096 || width%2 != 0 || height%2 != 0 {
		return nil, fmt.Errorf("invalid even camera transform dimensions")
	}
	// Keep the crop at the original rational aspect ratio whenever possible.
	// All edges are even so UV pairs describe the same 2x2 luma area.
	cw, ch := ZoomCrop(width, height, o.Zoom())
	t := &Transform{width: width, height: height, options: o, rect: ImageRect{0, 0, width, height}, crop: ImageRect{((width - cw) / 2) &^ 1, ((height - ch) / 2) &^ 1, cw, ch}}
	if o.Rotation == 0 && !o.Mirror && o.Zoom() == 100 {
		return t, nil
	}
	rw, rh := cw, ch
	if o.Rotation == 90 || o.Rotation == 270 {
		rw, rh = ch, cw
	}
	ow, oh := width, height
	if int64(width)*int64(rh) <= int64(height)*int64(rw) {
		oh = (width * rh / rw) &^ 1
	} else {
		ow = (height * rw / rh) &^ 1
	}
	if ow < 2 || oh < 2 {
		return nil, fmt.Errorf("camera image aspect ratio too narrow")
	}
	// Even offsets keep each U/V pair aligned with its 2x2 luma sample group.
	t.rect = ImageRect{((width - ow) / 2) &^ 1, ((height - oh) / 2) &^ 1, ow, oh}
	t.xmap = make([]int, ow)
	t.ymap = make([]int, oh)
	for x := range t.xmap {
		t.xmap[x] = min(rw-1, int((int64(2*x+1)*int64(rw))/(2*int64(ow))))
	}
	for y := range t.ymap {
		t.ymap[y] = min(rh-1, int((int64(2*y+1)*int64(rh))/(2*int64(oh))))
	}
	// Separable offsets: O(width+height), not a full-frame index table.
	offsets := make([]int, (ow+oh)*3/2)
	t.xoff, offsets = offsets[:ow], offsets[ow:]
	t.yoff, offsets = offsets[:oh], offsets[oh:]
	t.uxoff, t.uyoff = offsets[:ow/2], offsets[ow/2:]
	t.data = make([]byte, width*height*3/2)
	for i := 0; i < width*height; i++ {
		t.data[i] = 16
	}
	for i := width * height; i < len(t.data); i++ {
		t.data[i] = 128
	}
	return t, nil
}

// ZoomCrop uses integer aspect units, avoiding anisotropic enlargement on
// 720p/1080p/4K. Tiny test or exotic dimensions fall back to even cropping.
func ZoomCrop(width, height, zoom int) (int, int) {
	gcd := func(a, b int) int {
		for b != 0 {
			a, b = b, a%b
		}
		return a
	}
	g := gcd(width, height)
	if g <= 0 || zoom < 100 || zoom > 400 {
		return 0, 0
	}
	unitW, unitH := width/g, height/g
	k := (g * 100 / zoom) &^ 1
	if k >= 2 {
		return unitW * k, unitH * k
	}
	return max(2, (width*100/zoom)&^1), max(2, (height*100/zoom)&^1)
}
func (t *Transform) ContentRect() ImageRect { return t.rect }
func (t *Transform) SourceCrop() ImageRect  { return t.crop }
func validateImage(im media.Image, width, height int) error {
	if im.Width != width || im.Height != height || im.StrideY < width || im.StrideUV < width || im.StrideY > 65536 || im.StrideUV > 65536 || im.PTS < 0 {
		return fmt.Errorf("camera frame dimensions/strides/timestamp changed")
	}
	// The final row need not have trailing padding. Never sample padding bytes.
	needY := (height-1)*im.StrideY + width
	needUV := (height/2-1)*im.StrideUV + width
	if len(im.Y) < needY || len(im.UV) < needUV {
		return fmt.Errorf("truncated camera NV12 plane")
	}
	return nil
}
func (t *Transform) sourcePoint(x, y int) (int, int) {
	rw := t.crop.Width
	if t.options.Rotation == 90 || t.options.Rotation == 270 {
		rw = t.crop.Height
	}
	if t.options.Mirror {
		x = rw - 1 - x
	}
	switch t.options.Rotation {
	case 90:
		return t.crop.X + y, t.crop.Y + t.crop.Height - 1 - x
	case 180:
		return t.crop.X + t.crop.Width - 1 - x, t.crop.Y + t.crop.Height - 1 - y
	case 270:
		return t.crop.X + t.crop.Width - 1 - y, t.crop.Y + x
	default:
		return t.crop.X + x, t.crop.Y + y
	}
}
func (t *Transform) Apply(im media.Image) (media.Image, error) {
	if t == nil {
		return media.Image{}, fmt.Errorf("nil camera transform")
	}
	if err := validateImage(im, t.width, t.height); err != nil {
		return media.Image{}, err
	}
	if t.data == nil {
		return im, nil
	}
	t.prepareOffsets(im.StrideY, im.StrideUV)
	dstY, dstUV := t.data[:t.width*t.height], t.data[t.width*t.height:]
	r := t.rect
	for y, ybase := range t.yoff {
		row := dstY[(r.Y+y)*t.width+r.X:][:r.Width]
		for x, delta := range t.xoff {
			row[x] = im.Y[ybase+delta]
		}
	}
	for y, ybase := range t.uyoff {
		row := dstUV[(r.Y/2+y)*t.width+r.X:][:r.Width]
		for x, delta := range t.uxoff {
			off := ybase + delta
			row[2*x], row[2*x+1] = im.UV[off], im.UV[off+1]
		}
	}
	return media.Image{Width: t.width, Height: t.height, StrideY: t.width, StrideUV: t.width, Y: dstY, UV: dstUV, PTS: im.PTS}, nil
}

// TransformSource preserves the capture provider's ownership boundary. It never
// retains a sensor buffer, submits work asynchronously, or changes timestamps.
type TransformSource struct {
	source    Source
	transform *Transform
}

func WrapTransform(source Source, mode Mode, options ImageOptions) (Source, error) {
	if source == nil {
		return nil, fmt.Errorf("camera source required")
	}
	t, err := NewTransform(int(mode.Width), int(mode.Height), options)
	if err != nil {
		return nil, err
	}
	return &TransformSource{source: source, transform: t}, nil
}
func (s *TransformSource) Drain(emit func(media.Image) error) (int, error) {
	if emit == nil {
		return 0, fmt.Errorf("camera transform requires consumer")
	}
	return s.source.Drain(func(im media.Image) error {
		next, err := s.transform.Apply(im)
		if err != nil {
			return err
		}
		return emit(next)
	})
}
func (s *TransformSource) Close() error { return s.source.Close() }

// Rotation, mirror and center-crop are separable by axis. Compile affine source
// addresses only when the negotiated row strides change. PTS and frame storage
// are not cached. Source bounds are checked by Apply before this function.
func (t *Transform) prepareOffsets(strideY, strideUV int) {
	if t.strideY == strideY && t.strideUV == strideUV {
		return
	}
	sx0, sy0 := t.sourcePoint(t.xmap[0], t.ymap[0])
	baseY := sy0*strideY + sx0
	baseUV := (sy0/2)*strideUV + (sx0 &^ 1)
	for x := range t.xoff {
		sx, sy := t.sourcePoint(t.xmap[x], t.ymap[0])
		t.xoff[x] = sy*strideY + sx - baseY
		if x%2 == 0 {
			t.uxoff[x/2] = (sy/2)*strideUV + (sx &^ 1) - baseUV
		}
	}
	for y := range t.yoff {
		sx, sy := t.sourcePoint(t.xmap[0], t.ymap[y])
		t.yoff[y] = sy*strideY + sx
		if y%2 == 0 {
			t.uyoff[y/2] = (sy/2)*strideUV + (sx &^ 1)
		}
	}
	t.strideY, t.strideUV = strideY, strideUV
}

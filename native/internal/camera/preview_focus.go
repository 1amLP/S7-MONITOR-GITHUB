package camera

import (
	"perimode/native/internal/camera/fimcshot"
	"perimode/native/internal/media"
)

// Inverse of the GPU's fit/rotation/mirror/zoom transform. Only the tapped
// coordinate is transformed on CPU; no video pixels are read or modified.
func PreviewFocusRegion(settings Settings, panel, x, y, width, height int, crop fimcshot.Rect) (fimcshot.Region, bool) {
	if settings.Validate() != nil || width < 2 || height < 2 || crop.Width < 2 || crop.Height < 2 || x < 0 || y < 0 || x >= width || y >= height {
		return fimcshot.Region{}, false
	}
	sw, sh := int(settings.Mode.Width), int(settings.Mode.Height)
	cw, ch := media.PreviewCrop(sw, sh, settings.Image.Zoom())
	rotation := PreviewRotation(settings.Sensor, panel, int(settings.Image.Rotation))
	rw, rh := cw, ch
	if rotation%180 != 0 {
		rw, rh = rh, rw
	}
	dw, dh := width, (width*rh/rw)&^1
	if dh > height {
		dh, dw = height, (height*rw/rh)&^1
	}
	left, top := ((width-dw)/2)&^1, ((height-dh)/2)&^1
	if x < left || y < top || x >= left+dw || y >= top+dh || dw < 2 || dh < 2 {
		return fimcshot.Region{}, false
	}
	sx, sy := min(rw-1, (2*(x-left)+1)*rw/(2*dw)), min(rh-1, (2*(y-top)+1)*rh/(2*dh))
	if settings.Image.Mirror {
		sx = rw - 1 - sx
	}
	px, py := sx, sy
	switch rotation {
	case 90:
		px, py = sy, ch-1-sx
	case 180:
		px, py = cw-1-sx, ch-1-sy
	case 270:
		px, py = cw-1-sy, sx
	}
	px += ((sw - cw) / 2) &^ 1
	py += ((sh - ch) / 2) &^ 1
	w, h := max(2, crop.Width/12)&^1, max(2, crop.Height/12)&^1
	cx := crop.X + uint32(uint64(px)*uint64(crop.Width)/uint64(sw))
	cy := crop.Y + uint32(uint64(py)*uint64(crop.Height)/uint64(sh))
	rx := uint32(max(int64(crop.X), min(int64(crop.X+crop.Width-w), int64(cx)-int64(w/2))))
	ry := uint32(max(int64(crop.Y), min(int64(crop.Y+crop.Height-h), int64(cy)-int64(h/2))))
	return fimcshot.Region{Rect: fimcshot.Rect{X: rx, Y: ry, Width: w, Height: h}, Weight: 1000}, true
}

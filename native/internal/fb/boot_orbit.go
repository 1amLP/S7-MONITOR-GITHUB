package fb

import (
	"fmt"
	"image"
	"math"
	"time"

	"perimode/native/internal/gpumenu"
)

func bootOrbitCommands(w, h int, elapsed time.Duration) ([]gpumenu.Command, error) {
	if w < 160 || h < 160 || w > 4096 || h > 4096 || elapsed < 0 {
		return nil, fmt.Errorf("invalid boot orbit geometry")
	}
	bounds := image.Rect(0, 0, w, h)
	d := &gpuDrawList{commands: make([]gpumenu.Command, 0, 192)}
	d.add(gpumenu.Rectangle, bounds, 0, 255, 0, 0, w, h, 0)
	cx, cy := float64(w)/2, float64(h)/2
	short := float64(min(w, h))
	radius, size := short*.24, short*.095
	seconds := elapsed.Seconds()
	const gold = uint32(0xffdf32)
	line := func(x1, y1, x2, y2, stroke, alpha int) {
		pad := stroke + 2
		clip := image.Rect(min(x1, x2)-pad, min(y1, y2)-pad, max(x1, x2)+pad+1, max(y1, y2)+pad+1).Intersect(bounds)
		d.add(gpumenu.Line, clip, gold, alpha, x1, y1, x2, y2, stroke)
	}
	point := func(x, y float64) (int, int) { return int(math.Round(x)), int(math.Round(y)) }
	stroke := max(3, int(short/240))
	for i := 0; i < 48; i++ {
		a := float64(i)*math.Pi/24 + seconds*.18
		x1, y1 := point(cx+math.Cos(a)*(radius-size*.85), cy+math.Sin(a)*(radius-size*.85))
		x2, y2 := point(cx+math.Cos(a)*(radius-size*.95), cy+math.Sin(a)*(radius-size*.95))
		line(x1, y1, x2, y2, max(1, stroke/2), 110)
	}
	for i, kind := range []string{"monitor", "camera", "webcam", "speaker", "touch", "sniper"} {
		angle := float64(i)*math.Pi/3 + seconds*.65
		x, y := cx+radius*math.Cos(angle), cy+radius*math.Sin(angle)
		spin := seconds*2.6 + float64(i)*math.Pi/3
		co, si := math.Cos(spin), math.Sin(spin)
		transform := func(px, py int) (int, int) {
			dx, dy := (float64(px)-8)*size/16, (float64(py)-8)*size/16
			return point(x+dx*co-dy*si, y+dx*si+dy*co)
		}
		for _, seg := range icon(kind) {
			x1, y1 := transform(seg.X1, seg.Y1)
			x2, y2 := transform(seg.X2, seg.Y2)
			line(x1, y1, x2, y2, stroke*3, 45)
			line(x1, y1, x2, y2, stroke, 255)
		}
	}
	return d.commands, d.err
}

func (b *Buffer) BeginBootOrbit() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.menuGPU == nil || b.scanout == nil || b.glass != nil || (b.cameraHUD != nil && b.cameraHUD.Count > 0) {
		return false
	}
	b.bootOrbit = true
	return true
}

func (b *Buffer) RenderBootOrbit(elapsed time.Duration) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.bootOrbit {
		return false, nil
	}
	if b.closed || b.hardwareGlassUnsafe || b.menuGPU == nil || b.scanout == nil {
		return false, fmt.Errorf("boot GPU unavailable")
	}
	commands, err := bootOrbitCommands(b.Width, b.Height, elapsed)
	if err != nil {
		return false, err
	}
	_, _, active, _ := b.scanout.LayerState()
	slot := 0
	if active.Enabled && active.FD == b.menuGPU.MenuFD(0) {
		slot = 1
	}
	if err = b.scanout.WaitMenuReusable(b.menuGPU.MenuFD(slot)); err != nil {
		return false, err
	}
	fd, err := b.menuGPU.RenderSlot(int(b.Rotation), b.Width, b.Height, commands, slot)
	if err != nil {
		return false, err
	}
	b.layerMenuFD = fd
	b.layerMenuRect = image.Rect(0, 0, 1440, 2560)
	return true, b.commitLayersLocked(nil)
}

func (b *Buffer) EndBootOrbit() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.bootOrbit {
		return nil
	}
	b.bootOrbit = false
	b.layerMenuFD = 0
	return b.commitLayersLocked(nil)
}

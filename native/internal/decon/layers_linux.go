//go:build linux && (amd64 || arm64)

package decon

import (
	"fmt"
	"perimode/native/internal/fimg2d"
	"perimode/native/internal/media"
	"perimode/native/pkg/monitor"
	"os"
)

type RGBLayer struct {
	Enabled, Blank    bool
	FD, Width, Height int
	Rect              fimg2d.Rect
	Source            fimg2d.Rect
}

type CameraView struct {
	Rotation, PanelRotation, Zoom int
	Mirror                        bool
}

// Slots: black backdrop, VGR0 video, G1 menu, G2 Preview. G0 is reserved for
// WIN7 by e418 and cannot be assigned to one of these ordinary windows.
func layerConfig(video media.Image, rotation int, menu, preview RGBLayer) (winConfigData, error) {
	return layerConfigView(video, rotation, menu, preview, nil)
}

func layerConfigView(video media.Image, rotation int, menu, preview RGBLayer, camera *CameraView) (winConfigData, error) {
	d := winConfigData{Fence: -1, FDODMA: -1}
	d.Config[0].State = 1
	d.Config[0].Dst = frame{W: panelWidth, H: panelHeight, FW: panelWidth, FH: panelHeight}
	if video.Lease != nil {
		l := video.Lease
		validMode := monitor.ValidMode(uint32(l.Width), uint32(l.Height)) && l.Color == media.BT709Limited
		if camera != nil {
			validMode = (l.Width == 1280 && l.Height == 720 || l.Width == 1920 && l.Height == 1080 || l.Width == 2560 && l.Height == 1440) && l.Color == media.BT601Limited
		}
		if !l.Live() || !validMode || video.Width != l.Width || video.Height != l.Height || video.PTS != l.PTS || len(l.Planes) != 2 || l.StorageWidth < l.Width || l.StorageWidth > 4096 || l.StorageWidth%16 != 0 || l.StorageHeight < l.Height || l.StorageHeight > l.Height+48 || l.StorageHeight%2 != 0 {
			return d, fmt.Errorf("VPP requires a pinned, admitted NV12M layout")
		}
		for i, plane := range l.Planes {
			rows := l.StorageHeight
			if i == 1 {
				rows /= 2
			}
			if plane.Offset != 0 || plane.Stride != uint32(l.StorageWidth) || uint64(plane.Length) < uint64(rows*l.StorageWidth) || plane.FD < 0 || plane.FD > 0x7fffffff {
				return d, fmt.Errorf("unsupported VPP plane offset/stride/allocation")
			}
		}
		if l.Planes[0].FD == l.Planes[1].FD {
			return d, fmt.Errorf("VPP NV12M needs separate plane allocations")
		}
		rot := int32(0)
		dst := frame{X: 0, Y: 875, W: 1440, H: 810, FW: 1440, FH: 2560}
		switch rotation {
		case 0:
		case 180:
			rot = 3
		case 90:
			rot = 4
			dst = frame{W: 1440, H: 2560, FW: 1440, FH: 2560}
		case 270:
			rot = 7
			dst = frame{W: 1440, H: 2560, FW: 1440, FH: 2560}
		default:
			return d, fmt.Errorf("invalid VPP rotation")
		}
		w := &d.Config[1]
		w.State = winStateBuffer
		// Use the WIN_CONFIG NV12M ABI, not the internal VPP register enum.
		// The kernel's format translation includes the hardware byte order.
		w.Buffer = winBuffer{FD: [3]int32{int32(l.Planes[0].FD), int32(l.Planes[1].FD), -1}, FenceFD: -1, PlaneAlpha: 255, IDMA: 6, Format: 15,
			VPP: vppParams{Rot: rot, CSC: 2}, Src: frame{W: uint32(l.Width), H: uint32(l.Height), FW: uint32(l.StorageWidth), FH: uint32(l.StorageHeight)}}
		w.Dst = dst
		if camera != nil {
			if err := applyCameraView(w, l, *camera); err != nil {
				return d, err
			}
		}
	}
	for i, layer := range []RGBLayer{menu, preview} {
		if !layer.Enabled {
			continue
		}
		r := layer.Rect
		if r.X < 0 || r.Y < 0 || r.W < 1 || r.H < 1 || r.X+r.W > panelWidth || r.Y+r.H > panelHeight {
			return d, fmt.Errorf("overlay outside panel")
		}
		w := &d.Config[i+2]
		w.Dst = frame{X: int32(r.X), Y: int32(r.Y), W: uint32(r.W), H: uint32(r.H), FW: panelWidth, FH: panelHeight}
		if layer.Blank {
			w.State = 1
			continue
		}
		source := layer.Source
		if source.W == 0 && source.H == 0 {
			source = fimg2d.Rect{W: layer.Width, H: layer.Height}
		}
		if layer.FD < 0 || layer.FD > 0x7fffffff || layer.Width < 1 || layer.Height < 1 || layer.Width > panelWidth || layer.Height > panelHeight || source.X < 0 || source.Y < 0 || source.X > layer.Width || source.Y > layer.Height || source.W != r.W || source.H != r.H || source.W > layer.Width-source.X || source.H > layer.Height-source.Y {
			return d, fmt.Errorf("RGB overlay crop must match destination without scaling")
		}
		idma := int32(1)
		if i == 1 {
			idma = 4
		}
		w.State = winStateBuffer
		w.Buffer = winBuffer{FD: [3]int32{int32(layer.FD), -1, -1}, FenceFD: -1, PlaneAlpha: 255, IDMA: idma, Format: formatBGRA8888, Blending: 1,
			Src: frame{X: int32(source.X), Y: int32(source.Y), W: uint32(source.W), H: uint32(source.H), FW: uint32(layer.Width), FH: uint32(layer.Height)}}
	}
	return d, nil
}

func (p *Presenter) PresentLayers(video media.Image, rotation int, menu, preview RGBLayer) error {
	return p.presentLayersWith(video, rotation, menu, preview, p.presentConfig)
}
func (p *Presenter) presentLayersWith(video media.Image, rotation int, menu, preview RGBLayer, present func(winConfigData) error) error {
	return p.presentViewWith(video, rotation, menu, preview, nil, present)
}

func (p *Presenter) PresentCamera(video media.Image, view CameraView, menu RGBLayer) error {
	return p.presentViewWith(video, view.PanelRotation, menu, RGBLayer{}, &view, p.presentConfig)
}

func (p *Presenter) presentViewWith(video media.Image, rotation int, menu, preview RGBLayer, camera *CameraView, present func(winConfigData) error) error {
	if p == nil || p.closed {
		return os.ErrClosed
	}
	if p.poisoned {
		return fmt.Errorf("DECON layer ownership uncertain")
	}
	config, err := layerConfigView(video, rotation, menu, preview, camera)
	if err != nil {
		return err
	}
	if video.Lease != nil {
		if err = video.Lease.Retain(); err != nil {
			return err
		}
	}
	old := p.video.Lease
	// Telemetry can swap an immutable menu slot together with a new video frame.
	// Menu writers call WaitMenuReusable before touching an inactive slot.
	previousMenu := p.menuLayer
	if old != video.Lease && menu.Enabled && previousMenu.Enabled {
		previousMenu.FD = menu.FD
	}
	fast := (p.device != nil || p.submitConfigTest != nil) && p.stats.HardwareLayers && old != nil && video.Lease != nil && camera == nil && p.cameraView == nil &&
		menu == previousMenu && rotation == p.layerRotation && !preview.Enabled && !p.previewLayer.Enabled && p.retireFD >= 0
	if fast {
		err = p.presentVideoFast(config, old)
	} else {
		err = present(config)
	}
	if err != nil {
		if video.Lease != nil {
			p.uncertainVideo = append(p.uncertainVideo, video.Lease)
		}
		return p.fail(err)
	}
	p.video, p.layerRotation, p.menuLayer, p.previewLayer = video, rotation, menu, preview
	p.cameraView = nil
	if camera != nil {
		copy := *camera
		p.cameraView = &copy
	}
	p.stats.HardwareLayers = true
	p.stats.LastCopyUS = 0
	p.stats.LayerUpdates++
	if old != video.Lease && video.Lease != nil {
		p.stats.NV12Frames++
	}
	if fast {
		return nil // The old lease is owned by its retire-fence record.
	}
	return old.Release()
}

func (p *Presenter) CameraView() (CameraView, bool) {
	if p.cameraView == nil {
		return CameraView{}, false
	}
	return *p.cameraView, true
}

func applyCameraView(w *winConfig, l *media.FrameLease, view CameraView) error {
	if view.Rotation < 0 || view.Rotation > 270 || view.Rotation%90 != 0 || view.PanelRotation < 0 || view.PanelRotation > 270 || view.PanelRotation%90 != 0 || view.Zoom < 100 || view.Zoom > 400 {
		return fmt.Errorf("invalid hardware camera view")
	}
	cw, ch := media.PreviewCrop(l.Width, l.Height, view.Zoom)
	w.Buffer.Src.X, w.Buffer.Src.Y = int32((l.Width-cw)/2&^1), int32((l.Height-ch)/2&^1)
	w.Buffer.Src.W, w.Buffer.Src.H = uint32(cw), uint32(ch)
	r := (view.Rotation + view.PanelRotation) % 360
	w.Buffer.VPP.Rot = [4]int32{0, 4, 3, 7}[r/90]
	if view.Mirror {
		axis := int32(1)
		if view.PanelRotation%180 != 0 {
			axis = 2
		}
		w.Buffer.VPP.Rot ^= axis
	}
	w.Buffer.VPP.CSC = 0 // Camera ISP produces BT.601 limited, not desktop BT.709.
	if r%180 != 0 {
		cw, ch = ch, cw
	}
	dw, dh := panelWidth, panelWidth*ch/cw
	if dh > panelHeight {
		dh, dw = panelHeight, panelHeight*cw/ch
	}
	dw, dh = dw&^1, dh&^1
	w.Dst = frame{X: int32((panelWidth - dw) / 2 &^ 1), Y: int32((panelHeight - dh) / 2 &^ 1), W: uint32(dw), H: uint32(dh), FW: panelWidth, FH: panelHeight}
	return nil
}

func (p *Presenter) LayerState() (media.Image, int, RGBLayer, RGBLayer) {
	return p.video, p.layerRotation, p.menuLayer, p.previewLayer
}
func (p *Presenter) UsesLayers() bool { return p != nil && p.stats.HardwareLayers }
func (p *Presenter) DropVideo() error {
	if p == nil || p.video.Lease == nil {
		return nil
	}
	return p.PresentLayers(media.Image{}, p.layerRotation, p.menuLayer, p.previewLayer)
}

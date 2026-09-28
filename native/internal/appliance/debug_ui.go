package appliance

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"perimode/native/internal/fb"
	"perimode/native/pkg/hid"
)

const (
	debugCapture         = 1
	debugOpen            = 2
	debugClose           = 3
	debugBack            = 4
	debugTap             = 5
	debugScroll          = 6
	debugRecents         = 7
	debugRecovery        = 8
	debugRecoveryConfirm = 0x52564352
)

type debugCommand struct {
	Sequence, Kind uint32
	X, Y           int32
	Capture        uint32
	Epoch          uint64
}
type debugResult struct {
	sequence     uint32
	status       uint32
	meta, pixels []byte
}
type debugUI struct {
	mu                       sync.Mutex
	requests                 chan debugCommand
	result                   debugResult
	epoch                    uint64
	pending                  bool
	lastRequest, lastCapture time.Time
	recent                   [32]uint32
	recentNext               int
	// Only the UI owner accesses the captured hit targets.
	captureID       uint32
	captureEpoch    uint64
	capturePage     string
	captureFocus    int
	captureGeometry [3]int
	captureLayout   fb.GlassLayout
	captureLines    []string
}

func newDebugUI() *debugUI { return &debugUI{requests: make(chan debugCommand, 1)} }

func debugUIEnabled(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 65536 {
		return false
	}
	var cfg struct {
		Enabled bool `json:"ui_diagnostics"`
	}
	return json.Unmarshal(b, &cfg) == nil && cfg.Enabled
}

func parseDebugCommand(b []byte) (debugCommand, error) {
	if len(b) != 24 || !bytes.Equal(b[:4], []byte("UI01")) {
		return debugCommand{}, fmt.Errorf("invalid UI diagnostic packet")
	}
	c := debugCommand{Sequence: binary.LittleEndian.Uint32(b[4:]), Kind: binary.LittleEndian.Uint32(b[8:]),
		X: int32(binary.LittleEndian.Uint32(b[12:])), Y: int32(binary.LittleEndian.Uint32(b[16:])), Capture: binary.LittleEndian.Uint32(b[20:])}
	if c.Sequence == 0 || c.Kind < debugCapture || c.Kind > debugRecovery {
		return c, fmt.Errorf("unsupported UI command")
	}
	if c.Kind == debugRecovery {
		if c.X != debugRecoveryConfirm || c.Y != 0 || c.Capture != 0 {
			return c, fmt.Errorf("Recovery confirmation missing")
		}
	} else if c.Kind == debugTap {
		if c.X < 0 || c.Y < 0 || c.X > 32767 || c.Y > 32767 || c.Capture == 0 {
			return c, fmt.Errorf("invalid tap")
		}
	} else if c.Kind == debugScroll {
		if c.X == 0 || c.X < -720 || c.X > 720 || c.Y != 0 || c.Capture == 0 {
			return c, fmt.Errorf("invalid scroll")
		}
	} else if c.X != 0 || c.Y != 0 || c.Capture != 0 {
		return c, fmt.Errorf("unexpected UI arguments")
	}
	return c, nil
}

func (d *debugUI) submit(b []byte, now time.Time) error {
	c, err := parseDebugCommand(b)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, sequence := range d.recent {
		if c.Sequence == sequence {
			return nil
		}
	}
	if d.pending || now.Sub(d.lastRequest) < 100*time.Millisecond {
		return fmt.Errorf("UI diagnostic busy")
	}
	if c.Kind == debugCapture && now.Sub(d.lastCapture) < time.Second {
		return fmt.Errorf("capture rate limited")
	}
	c.Epoch = d.epoch
	select {
	case d.requests <- c:
		d.recent[d.recentNext] = c.Sequence
		d.recentNext = (d.recentNext + 1) % len(d.recent)
		d.pending = true
		d.lastRequest = now
		if c.Kind == debugCapture {
			d.lastCapture = now
		}
		d.result = debugResult{sequence: c.Sequence, status: 1}
		return nil
	default:
		return fmt.Errorf("UI diagnostic queue full")
	}
}

func (d *debugUI) disconnect() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.epoch++
	d.pending = false
	d.result = debugResult{}
	select {
	case <-d.requests:
	default:
	}
}

func (d *debugUI) read(page uint16, length int) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.result
	if page == 0 {
		b := make([]byte, 32)
		copy(b, "UI01")
		binary.LittleEndian.PutUint32(b[4:], r.sequence)
		binary.LittleEndian.PutUint32(b[8:], r.status)
		binary.LittleEndian.PutUint32(b[12:], uint32(len(r.meta)))
		binary.LittleEndian.PutUint32(b[16:], uint32(len(r.pixels)))
		return b[:min(length, len(b))]
	}
	if r.status != 2 && r.status != 3 {
		return []byte{32}
	}
	start := int(page-1) * 4096
	total := len(r.meta) + len(r.pixels)
	if start >= total {
		return []byte{32}
	}
	end := min(total, start+length)
	b := make([]byte, end-start)
	if start < len(r.meta) {
		copy(b, r.meta[start:min(end, len(r.meta))])
	}
	if end > len(r.meta) {
		copy(b[max(0, len(r.meta)-start):], r.pixels[max(0, start-len(r.meta)):end-len(r.meta)])
	}
	return b
}

func (u *UI) handleDebugCommand(c debugCommand) {
	d := u.debug
	if d == nil {
		return
	}
	d.mu.Lock()
	current := c.Epoch == d.epoch
	d.mu.Unlock()
	if !current {
		return
	}
	_, _, page := u.state.Current()
	var err error
	var capture fb.Capture
	if c.Kind == debugCapture {
		if u.screen == nil {
			err = fmt.Errorf("framebuffer unavailable")
		} else {
			capture, err = u.screen.Capture()
		}
		if err == nil {
			d.captureID, d.captureEpoch, d.capturePage = c.Sequence, c.Epoch, page
			d.captureFocus = u.menuFocus
			w, h, rotation := u.screen.Geometry()
			d.captureGeometry = [3]int{w, h, int(rotation)}
			d.captureLayout = u.menuLayout
			d.captureLines = append([]string(nil), u.menuLines...)
		}
	} else if u.touchDown || u.lastInputCount != 0 {
		err = fmt.Errorf("physical input active")
	} else {
		switch c.Kind {
		case debugRecovery:
			// Give the host a bounded window to read the acknowledgement. The UI
			// owner then follows the existing safe shutdown/Recovery path.
			u.debugRecoveryAt = time.Now().Add(time.Second)
		case debugOpen:
			if page == "" {
				u.menu("MONITOR")
			}
		case debugClose:
			if page != "" {
				u.menu("")
			}
		case debugBack:
			u.key(158)
		case debugRecents:
			u.key(254)
		case debugTap, debugScroll:
			if u.screen == nil || page == "" || c.Capture != d.captureID || c.Epoch != d.captureEpoch || page != d.capturePage {
				err = fmt.Errorf("capture is stale or menu closed")
				break
			}
			w, h, rotation := u.screen.Geometry()
			if d.captureGeometry != [3]int{w, h, int(rotation)} || d.captureFocus != u.menuFocus {
				err = fmt.Errorf("menu focus or orientation changed; capture again")
				break
			}
			if c.Kind == debugScroll {
				u.menuScroll = max(0, min(u.menuLayout.Maximum, u.menuScroll+int(c.X)))
				u.menuNeedsDraw = true
				u.flushMenuScroll()
				break
			}
			x, y := uint16(c.X), uint16(c.Y)
			row := u.menuLayout.Hit(w, h, x, y)
			oldRow := d.captureLayout.Hit(w, h, x, y)
			rect, label := menuTarget(u.menuLayout, u.menuLines, row)
			oldRect, oldLabel := menuTarget(d.captureLayout, d.captureLines, oldRow)
			if row != oldRow || rect != oldRect || label != oldLabel {
				err = fmt.Errorf("tap target moved; capture again")
				break
			}
			if !debugTapAllowed(page, row, u.menuLines) {
				err = fmt.Errorf("local-only menu action")
				break
			}
			u.menuInput([]hid.Contact{{ID: 0, X: x, Y: y}})
			u.menuInput(nil)
		}
	}
	_, _, page = u.state.Current()
	u.state.mu.Lock()
	gpuStatus, gpuError := u.state.GPUProbeStatus, u.state.GPUProbeError
	u.state.mu.Unlock()
	meta := map[string]any{"page": page, "focus": u.menuFocus, "lines": u.menuLines, "layout": u.menuLayout,
		"gpu_probe": gpuStatus, "gpu_error": gpuError, "capture": capture, "source": "committed layer-buffer snapshot; hardware composed on demand, not panel readback"}
	meta["recovery_requested"] = !u.debugRecoveryAt.IsZero()
	status := uint32(2)
	if err != nil {
		status = 3
		meta["error"] = err.Error()
	}
	b, marshalErr := json.Marshal(meta)
	if marshalErr != nil {
		status = 3
		b = []byte(`{"error":"metadata encoding failed"}`)
		capture.Pixels = nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if c.Epoch != d.epoch {
		return
	}
	d.result = debugResult{sequence: c.Sequence, status: status, meta: b, pixels: capture.Pixels}
	d.pending = false
}

func debugTapAllowed(page string, row int, lines []string) bool {
	// These pages contain power, persistence, USB-rebind or engineering commands.
	// They may be inspected, but activated only by the physical user.
	switch page {
	case "POWER", "DEVICE_POWER", "STORAGE", "STORAGE_CONFIRM", "DIAGNOSTICS", "DIAGNOSTICS_ENABLE", "DIAGNOSTICS_CLEAR", "DIAGNOSTICS_DISABLE", "CAMERA_TRIAL", "CAMERA_TRIAL_CONFIRM", "MONITOR_LINK":
		return row >= fb.GlassCategoryBase && row < fb.GlassDetailBase || row > 0 && row < len(lines) && strings.HasPrefix(lines[row], "BACK:")
	}
	return true
}

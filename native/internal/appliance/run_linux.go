//go:build linux && (amd64 || arm64)

package appliance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"perimode/native/internal/ambient"
	"perimode/native/internal/camera"
	"perimode/native/internal/fb"
	"perimode/native/internal/journal"
	"perimode/native/internal/lifecycle"
	"perimode/native/internal/media"
	"perimode/native/internal/mediacodec"
	"perimode/native/internal/mediaruntime"
	"perimode/native/internal/orientation"
	"perimode/native/internal/safety"
	"perimode/native/internal/sensorhub"
	"perimode/native/internal/torch"
	"perimode/native/pkg/hid"
	"perimode/native/pkg/monitor"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type UI struct {
	bootReady                           atomic.Bool
	cameraControlsMu                    sync.Mutex
	hapticPulse                         func()
	hapticContacts                      uint32
	cameraPropertiesChanged             chan struct{}
	installer                           *installerSession
	installerDone                       chan struct{}
	opsDone                             chan struct{}
	noticeText                          string
	noticeUntil                         time.Time
	noticeFlags                         [4]bool
	noticeFlagsReady                    bool
	cameraHUDSelection                  string
	cameraHUDPaint                      fb.CameraHUD
	cameraHUDVisible, cameraHUDDragging bool
	cameraHUDLastValue                  int
	cameraHUDRefresh, cameraHUDFocusAt  time.Time
	cameraHUDFocusX, cameraHUDFocusY    uint16
	menuPaintJobs                       chan menuPaintRequest
	menuPaintDone                       chan menuPaintResult
	paintRevision                       uint64
	paintAcceptedRevision               uint64
	paintPageStart                      uint64
	paintRequestedPage                  string
	debug                               *debugUI
	debugRecoveryAt                     time.Time
	inputTiming                         inputTiming
	labMode                             bool
	usbTrialFPS                         uint32
	codecSelection                      mediacodec.Selection
	presentFrames                       *frameMailbox
	linkSeen                            bool
	linkPhase                           string
	cameraTrialRequests                 chan uint32
	cameraTrialRun                      cameraTrialRunner
	pendingTrialFPS                     uint32
	audioBackend                        audioRuntime
	workers                             *lifecycle.Group
	hub                                 *sensorhub.Manager
	torchFactory                        func() (torch.Driver, error)
	panelMu                             sync.Mutex
	lightFactory                        func() (ambient.Source, error)
	previewGesture                      bool
	sniperGesture                       sniperGesture
	sniperPress                         sniperPress
	menuMultitouch                      bool
	menuFocus                           int
	menuSlider                          int
	menuLayout                          fb.GlassLayout
	menuLines                           []string
	menuTapBounds                       [4]int
	menuTapLabel                        string
	menuScroll, menuStartScroll         int
	menuNeedsDraw                       bool
	menuParents                         map[string]string
	menuStartX, menuStartY              uint16
	menuStartTime                       time.Time
	menuHoldFired                       bool
	menuDragged                         bool
	menuScrollGesture                   bool
	lastInputCount                      int
	diagnosticStore                     *journal.Store
	inputEpoch                          atomic.Uint64
	inputAwaitAllUp                     bool
	orientationEvents                   chan rotationEvent
	orientationFilter                   orientation.Filter
	sensorFactory                       func() (orientation.SampleSource, error)
	panel                               PanelBrightness
	shutdownAction                      MachineAction
	pendingAction                       MachineAction
	cameraProvider                      camera.Provider
	background                          context.Context
	state                               *State
	screen                              *fb.Buffer
	transport                           *Transport
	device                              string
	keys                                chan uint16
	touchKeyPulse                       chan struct{}
	frames                              chan InputFrame
	powerConfirm                        time.Time
	touchDown                           bool
	touchRow                            int
	report                              map[string]any
	reportMu                            sync.Mutex
	presentationMu                      presentationGate
	decoderFactory                      func(string, []byte, uint64) (DecoderBackend, error)
	thermalError                        error
	previousCPU                         CPUTimes
	haveCPU                             bool
	pad                                 PadMapping
	suppressed                          uint32
	ops                                 chan func() error
	cache                               *CacheStore
	saved                               Preferences
}

func (u *UI) lines() []string {
	_, _, menu := u.state.Current()
	if lines, ok := u.deviceLeafLines(menu); ok {
		return lines
	}
	if lines, ok := u.monitorLeafLines(menu); ok {
		return lines
	}
	if lines, ok := u.audioLeafLines(menu); ok {
		return lines
	}
	if lines, ok := u.touchLeafLines(menu); ok {
		return lines
	}
	u.state.mu.Lock()
	gpuProbe := u.state.GPUProbeStatus
	load := u.state.CPULoad
	cpu, battery, batteryPercent := u.state.CPUMilliC, u.state.BatteryMilliC, u.state.BatteryPercent
	gpuTemperature, ispTemperature := u.state.GPUMilliC, u.state.ISPMilliC
	persistence, thermal := u.state.Persistence, u.state.ThermalStatus
	activeRotation, rotationStatus := u.state.ActiveRotation, u.state.RotationStatus
	lightStatus := u.state.Ambient.Status
	u.state.mu.Unlock()
	switch menu {
	case "PAD_POLLING":
		return u.pollingLines()
	case "INDICATOR_SETTINGS":
		return u.indicatorLines()
	case "CAMERA":
		cam := u.state.CameraCurrent()
		return []string{"CAMERA", fmt.Sprintf("ENABLED: %t", cam.Enabled), "CAMERA: " + cam.Settings.Sensor.String(), "DEVICE SETTINGS", "PICTURE SETTINGS", "PREVIEW", "STATISTICS", "BACK: CLOSE MENU"}
	case "CAMERA_DEVICE":
		return u.cameraDeviceLines()
	case "CAMERA_PICTURE":
		return u.cameraPictureLines()

	case "CAMERA_TRIAL":
		return u.trialLines()
	case "CAMERA_TRIAL_CONFIRM":
		return []string{"START EXCLUSIVE LOCAL CAMERA TRIAL?", fmt.Sprintf("CONFIRM REAR 720P / %d FPS", u.pendingTrialFPS), "NO MFC / NO USB / NO VIDEO SAVED", "STOP WEBCAM AND PIP FIRST", "BACK: CANCEL"}
	case "CAMERA_METRICS":
		return u.cameraMetricsLines()
	case "CAMERA_FOCUS", "CAMERA_EXPOSURE", "CAMERA_WB", "CAMERA_TONE":
		return u.camera3ALines(menu)
	case "CAMERA_TORCH":
		return u.torchLines()
	case "CAMERA_PREVIEW":
		lines := u.previewLines()
		if !u.state.PreviewCurrent().Enabled {
			return []string{lines[0], lines[1], lines[len(lines)-1]}
		}
		return lines
	case "CAMERA_IMAGE":
		c := u.state.CameraCurrent()
		return []string{"IMAGE", fmt.Sprintf("ROTATION: %d DEG", c.Settings.Image.Rotation), fmt.Sprintf("MIRROR: %t", c.Settings.Image.Mirror), "", "", "", fmt.Sprintf("ZOOM: %d%%", c.Settings.Image.Zoom()), "", "BACK: CAMERA"}
	case "CAMERA_MODES":
		lines := []string{"CAMERA MODES / NOT HARDWARE ACCEPTANCE"}
		for _, item := range u.state.CameraCatalog() {
			status := "TARGET ONLY"
			if item.Admitted {
				status = "PROVIDER ADMITS"
			}
			lines = append(lines, item.Sensor.String()+" "+item.Mode.String()+" / "+status)
		}
		return append(lines, "TAP: SELECT TARGET / OPEN ERROR", "BACK: CAMERA")
	case "CAMERA_MODE_INFO":
		c := u.state.CameraCurrent()
		reason := "NOT IN CATALOG"
		for _, v := range u.state.CameraCatalog() {
			if v.Sensor == c.Settings.Sensor && v.Mode == c.Settings.Mode {
				reason = v.Reason
				if v.Admitted {
					reason = "ADMITTED BY PROVIDER / NOT A HARDWARE TEST"
				}
				break
			}
		}
		return []string{"CAMERA / MODE DETAILS", c.Settings.Sensor.String() + " " + c.Settings.Mode.String(), reason, "BACK: MODE LIST"}
	case "PC_VOLUME":
		return u.pcVolumeLines()
	case "SCREEN_ROTATION":
		return u.rotationLines()
	case "DISPLAY_AUTO":
		return u.autoBrightnessLines()
	case "DISPLAY":
		return u.displayLines()
	case "DEVICE_POWER":
		return []string{"POWER", "POWER OFF", "RESTART", "RECOVERY", "BACK: CLOSE MENU"}
	case "POWER_MENU":
		lines := []string{"POWER", "POWER OFF", "REBOOT", "RECOVERY"}
		if u.pendingAction != ActionNone && time.Now().Before(u.powerConfirm) {
			lines[int(u.pendingAction)] = "CONFIRM " + u.pendingAction.String()
		}
		return lines
	case "DEVICE":
		return []string{"DEVICE", "STATISTICS", "", "", "", "", "SETTINGS STORAGE", "", "", "ORIENTATION", "BRIGHTNESS", "POWER", "", "DIAGNOSTICS", "INDICATORS", "CONNECTION", "BACK: CLOSE MENU"}
	case "DEVICE_STAT":
		lines := []string{"Statistics", "FUNCTION: DEVICE"}
		haptics := u.state.HapticSnapshot()
		lines = append(lines, fmt.Sprintf("HAPTIC DRIVER: %t", haptics.Available), fmt.Sprintf("HAPTIC REQUESTS: %d", haptics.Accepted))
		if haptics.LastError != "" {
			lines = append(lines, "HAPTIC ERROR: "+haptics.LastError)
		}
		power := u.state.PowerSnapshot()
		if power.Online != "" {
			lines = append(lines, "CPU ONLINE: "+power.Online, fmt.Sprintf("CPU LITTLE: %d MHZ", power.LittleKHz/1000), fmt.Sprintf("CPU BIG: %d MHZ", power.BigKHz/1000))
		}
		if power.ChargeLimit {
			lines = append(lines, "CHARGE LIMIT: 80% / RESUME 50%")
		}
		if power.BatteryHealth != "" {
			lines = append(lines, "BATTERY HEALTH: "+power.BatteryHealth)
		}
		if power.ChargeError != "" {
			lines = append(lines, "CHARGE CONTROL: "+power.ChargeError)
		}
		if power.CPUError != "" {
			lines = append(lines, "CPU CONTROL: "+power.CPUError)
		}
		if u.transport != nil {
			lines = append(lines, fmt.Sprintf("USB BOUND: %t", u.transport.Bound()))
		}
		if load >= 0 {
			lines = append(lines, fmt.Sprintf("CPU LOAD: %d%%", load))
		}
		if cpu >= 0 {
			lines = append(lines, "CPU TEMPERATURE: "+monitorTemperature(cpu))
		}
		if gpuTemperature >= 0 {
			lines = append(lines, "GPU TEMPERATURE: "+monitorTemperature(gpuTemperature))
		}
		if ispTemperature >= 0 {
			lines = append(lines, "ISP TEMPERATURE: "+monitorTemperature(ispTemperature))
		}
		if battery >= 0 {
			lines = append(lines, "BATTERY TEMPERATURE: "+monitorTemperature(battery))
		}
		if batteryPercent >= 0 {
			lines = append(lines, fmt.Sprintf("BATTERY: %d%%", batteryPercent))
		}
		if thermal != "" {
			lines = append(lines, "THERMAL: "+thermal)
		}
		if gpuProbe != "" {
			lines = append(lines, "GPU: "+gpuProbe)
		}
		lines = append(lines, fmt.Sprintf("ORIENTATION: %d\u00b0", activeRotation), "ROTATION SENSOR: "+rotationStatus, "LIGHT SENSOR: "+lightStatus)
		return append(lines, "BACK: DEVICE")
	case "LIVE_GLASS":
		return u.liveGlassLines()
	case "MONITOR_LINK":
		return u.monitorLinkRows()
	case "DEVICE_WORKERS":
		return u.workerLines()
	case "DIAGNOSTICS":
		return u.diagnosticLines()
	case "DIAGNOSTICS_ENABLE":
		return []string{"ALLOW FIRST-FAULT LOG IN CACHE?", "CONFIRM: STORE FIRST FAULT AND BOOT MARKER", "BACK: CANCEL", "BOUNDED CHECKSUMMED FILES / NO CONTINUOUS LOG"}
	case "DIAGNOSTICS_DISABLE":
		return []string{"STOP PERSISTENT DIAGNOSTICS?", "CONFIRM: DISABLE LOGGING / KEEP OLD RECORD", "BACK: CANCEL", "SETTINGS PERSISTENCE IS UNCHANGED"}
	case "DIAGNOSTICS_CLEAR":
		return []string{"CLEAR SAVED FIRST FAULT?", "CONFIRM: CLEAR ONLY DIAGNOSTIC RECORDS", "BACK: CANCEL", "SETTINGS AND OTHER PARTITIONS UNCHANGED"}
	case "STORAGE":
		return []string{"SETTINGS / STORAGE", "ENABLE CACHE PERSISTENCE", "BOOT / SYSTEM / USERDATA NOT WRITTEN", "CACHE: ONLY /S7-NATIVE DIRECTORY", "NO FORMAT / NO REPAIR / NO JOURNAL REPLAY", "CURRENT: " + persistence, "CHANGES SAVED EVERY 5 SECONDS", "AUDIO ENABLE FLAGS ARE NEVER SAVED", "FIRST-FAULT LOG / SEPARATE OPT-IN", "BACK: DEVICE"}
	case "STORAGE_CONFIRM":
		return []string{"ALLOW WRITES TO CACHE?", "CONFIRM: SAVE SETTINGS IN CACHE", "BACK: CANCEL", "NO FORMAT / NO USERDATA / NO EFS", "ONLY THE OWNED S7-NATIVE DIRECTORY"}
	case "POWER":
		return []string{"CONFIRM WHOLE-DEVICE " + u.pendingAction.String() + "?", "CONFIRM: " + u.pendingAction.String(), "BACK: CANCEL", "CONFIRMATION EXPIRES IN 3 SECONDS"}
	default:
		return nil
	}
}
func (u *UI) draw() {
	if u.menuPaintJobs != nil {
		u.queueMenuPaint()
		return
	}
	u.menuNeedsDraw = false
	if _, _, page := u.state.Current(); page == "" {
		return
	}
	u.presentationMu.Lock()
	defer u.presentationMu.Unlock()
	if u.screen != nil {
		_, gen, _ := u.state.Current()
		u.screen.SetVideoGeneration(gen)
		lines := u.lines()
		if len(lines) > 0 {
			u.state.Error(u.screen.SetMenuStatus(u.currentMenuStatus()))
			var source *media.Image
			if u.presentFrames != nil {
				if latest, ok := u.presentFrames.snapshotLatest(gen); ok {
					source = &latest
				}
			}
			layout, err := u.screen.GlassMenuSource(lines, u.menuScroll, source)
			u.state.Error(err)
			if err == nil {
				u.menuScroll = layout.Scroll
				u.menuLayout = layout
				u.menuLines = append(u.menuLines[:0], lines...)
			}
		} else {
			u.state.Error(u.drawIndicators())
		}
	}
}
func menuCategory(page string) int {
	switch page {
	case "MONITOR", "MONITOR_STAT", "LIVE_GLASS", "MONITOR_LINK":
		return 0
	case "INPUT", "PAD_POLLING", "TOUCH_STAT":
		return 1
	case "CAMERA", "CAMERA_DEVICE", "CAMERA_PICTURE", "CAMERA_TONE", "CAMERA_TRIAL", "CAMERA_TRIAL_CONFIRM", "CAMERA_METRICS", "CAMERA_FOCUS", "CAMERA_EXPOSURE", "CAMERA_WB", "CAMERA_TORCH", "CAMERA_PREVIEW", "CAMERA_IMAGE", "CAMERA_MODES", "CAMERA_MODE_INFO":
		return 2
	case "AUDIO", "SPEAKER", "MICROPHONE", "AUDIO_STAT", "PC_VOLUME":
		return 4
	case "DEVICE", "DEVICE_STAT", "POWER_SETTINGS", "DRIVER_INSTALL", "SCREEN_ROTATION", "DISPLAY_AUTO", "DISPLAY", "INDICATOR_SETTINGS", "DEVICE_WORKERS", "DIAGNOSTICS", "DIAGNOSTICS_ENABLE", "DIAGNOSTICS_DISABLE", "DIAGNOSTICS_CLEAR", "STORAGE", "STORAGE_CONFIRM":
		return 5
	case "DEVICE_POWER", "POWER", "POWER_MENU":
		return -1
	case "SNIPER":
		return 3
	default:
		return -1
	}
}
func (u *UI) currentMenuStatus() fb.MenuStatus {
	settings, _, page := u.state.Current()
	u.state.mu.Lock()
	battery, batteryTemp, cpuTemp, load := u.state.BatteryPercent, u.state.BatteryMilliC, u.state.SoCMilliC, u.state.CPULoad
	autoRotate := u.state.Rotation.Automatic
	u.state.mu.Unlock()
	codec := strings.ToUpper(u.codecSelection.Monitor)
	if codec == "" {
		codec = "PENDING"
	}
	link := u.state.MonitorLink(time.Now())
	detailTitle, detailHint, detailOptions, detailCount, detailSelected, detailHasSelection, detailSlider, detailSliderValue := u.currentDetailStatus(page)
	w, h := settings.Dimensions()
	v := fb.MenuStatus{PowerOnly: page == "POWER_MENU", Selected: menuCategory(page), Focused: u.menuFocus, Statistics: statisticsPage(page), AutoRotate: autoRotate, Mode: fmt.Sprintf("%d X %d", w, h), Rate: fmt.Sprintf("%d HZ", settings.FPS), Codec: codec + " H264", USB: "USB " + strings.ToUpper(link.Phase),
		DetailTitle: detailTitle, DetailHint: detailHint, DetailOptions: detailOptions, DetailCount: detailCount, DetailSelected: detailSelected, DetailHasSelection: detailHasSelection,
		DetailSlider: detailSlider, DetailSliderValue: detailSliderValue}
	v.Notice = u.noticeText
	u.state.mu.Lock()
	installerActive := u.state.InstallerActive
	u.state.mu.Unlock()
	if installerActive {
		v.InstallStatus, v.InstallAction = "INSTALLER MODE", "RETURN TO DEVICES"
	} else if u.usbRecoveryNeeded() {
		v.InstallStatus, v.InstallAction = "USB LINK LOST", "RECONNECT USB"
	} else {
		v.InstallStatus, v.InstallAction = installerHeader(u.state.EndpointSnapshot())
	}
	if fps := u.state.FrameRate(); fps.WindowMS > 0 {
		v.Rate += fmt.Sprintf(" / %.0f FPS", fps.Presented)
	} else {
		v.Rate += " / -- FPS"
	}
	if p := u.state.PreviewForDisplay(); p.Enabled && p.Options.Fullscreen {
		cam := u.state.CameraCurrent()
		v.Mode = fmt.Sprintf("%d X %d", cam.Settings.Mode.Width, cam.Settings.Mode.Height)
		v.Codec = cam.Settings.Sensor.String() + " NV12"
		v.Rate = fmt.Sprintf("PREVIEW %.0f FPS", u.state.FrameRate().Preview)
	}
	detail := u.menuDetail(page, u.menuFocus)
	v.DetailValueText = detail.SliderText
	v.DetailAction = detail.Action
	v.DetailSliderMin, v.DetailSliderMax, v.DetailSliderStep = detail.SliderMin, detail.SliderMax, detail.SliderStep
	bound, touchActive := false, false
	if u.transport != nil {
		bound = u.transport.Bound()
		if u.transport.touch != nil {
			_, _, touchActive = u.transport.touch.status()
		}
	}
	copy(v.Functions[:], u.state.IndicatorValues(time.Now(), bound, touchActive))
	if battery >= 0 {
		v.Battery = fmt.Sprintf("BAT %d%%", battery)
		if batteryTemp >= 0 {
			v.Battery += fmt.Sprintf(" %.1f C", float64(batteryTemp)/1000)
		}
	}
	if cpuTemp >= 0 {
		v.Temperature = fmt.Sprintf("SoC %.1f C", float64(cpuTemp)/1000)
	}
	if load >= 0 {
		v.Load = fmt.Sprintf("LOAD %d%%", load)
	}
	return v
}
func (u *UI) selectCategory(index int) {
	pages := [...]string{"MONITOR", "INPUT", "CAMERA", "SNIPER", "AUDIO", "DEVICE"}
	if index < 0 || index >= len(pages) {
		return
	}
	u.menuParents = nil
	u.menu(pages[index])
}

func (u *UI) rotateFromMenu(hold bool) {
	settings, _ := u.state.RotationCurrent()
	u.state.mu.Lock()
	current := u.state.ActiveRotation
	u.state.mu.Unlock()
	if hold {
		settings.Automatic = !settings.Automatic
		if !settings.Automatic {
			settings.Manual = current
		}
	} else {
		settings.Automatic = false
		settings.Manual = orientation.Degrees((int(current) + 90) % 360)
	}
	if err := u.configureRotation(settings); err != nil {
		u.state.Error(err)
	} else {
		u.feedback()
	}
	u.draw()
}
func (u *UI) menu(page string) {
	u.sniperGesture = sniperGesture{}
	u.sniperPress = sniperPress{}
	u.paintPageStart = u.paintRevision + 1
	u.state.SniperInput(nil)
	if page != "" {
		u.cameraHUDSelection = ""
		u.cameraHUDDragging = false
	}
	if page == "" {
		u.menuParents = nil
	}
	u.menuNeedsDraw = false
	u.menuLayout = fb.GlassLayout{}
	u.menuLines = u.menuLines[:0]
	u.menuSlider = 0
	u.menuFocus = 1
	if page != "POWER" {
		u.powerConfirm = time.Time{}
		u.pendingAction = ActionNone
	}
	if u.transport != nil && u.transport.touch != nil {
		u.transport.touch.blockInput()
	}
	if u.menuPaintJobs == nil {
		u.presentationMu.Lock()
		if page == "" && u.screen != nil {
			_, gen, _ := u.state.Current()
			u.screen.SetVideoGeneration(gen)
			u.state.Error(u.screen.EndGlass())
		}
	}
	u.state.mu.Lock()
	u.state.Menu = page
	u.state.mu.Unlock()
	if u.menuPaintJobs == nil {
		u.presentationMu.Unlock()
	}
	u.menuScroll = 0
	u.menuDragged = false
	u.menuScrollGesture = false
	u.menuMultitouch = false
	if u.lastInputCount > 0 {
		u.inputAwaitAllUp = true
	} else if page == "" && u.transport != nil && u.transport.touch != nil {
		// No evdev all-up event arrives while an already idle finger stays up.
		_ = u.transport.touch.submit(hid.TouchFrame{})
	}
	u.touchDown = false
	u.pad.Reset()
	u.suppressed = 0
	u.draw()
}
func (u *UI) selectRow(row int) {
	_, _, menu := u.state.Current()
	lines := u.lines()
	if statisticsPage(menu) && row > 0 && row < len(lines)-1 {
		return
	}
	if row > 0 && row < len(lines) && u.menuFocus != row {
		u.menuFocus = row
		if !u.middleOnlyRow(menu, row, lines) {
			u.draw()
		}
	}
	if row >= 0 && row < len(lines) && strings.HasPrefix(lines[row], "BACK:") {
		u.menu(u.parentMenu(menu))
		return
	}
	if u.selectDeviceNavigation(menu, row) {
		return
	}
	switch menu {
	case "MONITOR", "MONITOR_STAT":
		u.selectMonitorLeaf(menu, row, -1)
	case "PAD_POLLING":
		// Read-only details of the fixed sensor and software rate.
	case "INPUT":
		if row > 0 && row < len(lines) && lines[row] == "STATISTICS" {
			u.menu("TOUCH_STAT")
			return
		}
		u.state.mu.Lock()
		kind := u.state.TouchKind
		sensitivity, acceleration := u.state.PadSensitivity, u.state.PadAcceleration
		u.state.mu.Unlock()
		switch row {
		case 1:
			u.applyInputEnabled(kind == 0)
		case 2:
			if kind == 0 {
				return
			}
			next := byte(hid.Touchscreen)
			if kind == hid.Touchscreen {
				next = hid.Touchpad
			}
			u.applyInputMode(next)
		case 3:
			if kind != hid.Touchpad {
				return
			}
			gains := []int{25, 50, 75, 100, 125, 150, 200, 250, 300}
			next := gains[0]
			for i, gain := range gains {
				if gain == sensitivity {
					next = gains[(i+1)%len(gains)]
					break
				}
			}
			u.applyPadSensitivity(next)
		case 4:
			if kind == hid.Touchpad {
				u.applyPadAcceleration(!acceleration)
			}
		}
	case "CAMERA_TRIAL":
		if row == 1 || row == 2 {
			u.pendingTrialFPS = 120
			if row == 2 {
				u.pendingTrialFPS = 240
			}
			u.menu("CAMERA_TRIAL_CONFIRM")
		} else if row == 3 {
			u.cancelCameraTrial()
			u.draw()
		}
	case "CAMERA_TRIAL_CONFIRM":
		if row == 1 {
			fps := u.pendingTrialFPS
			u.pendingTrialFPS = 0
			u.state.Error(u.requestCameraTrial(fps))
			u.menu("CAMERA_TRIAL")
		}
	case "CAMERA_FOCUS", "CAMERA_EXPOSURE", "CAMERA_WB":
		u.draw()
	case "CAMERA":
		current := u.state.CameraCurrent()
		v := current.Settings
		switch row {
		case 1:
			if current.Enabled {
				u.state.cameraEnabled(false)
			} else {
				u.async(func() error {
					provider := u.cameraProvider
					if provider == nil {
						provider = camera.HerolteProvider{}
					}
					settings := u.state.CameraCurrent().Settings
					u.state.RefreshCameraCatalog(provider)
					if e := provider.Available(settings); e != nil {
						u.state.cameraStatus("CAMERA START FAILED", e)
						return e
					}
					if u.transport == nil {
						return fmt.Errorf("USB camera transport unavailable")
					}
					if e := u.transport.EnsureCamera(provider, settings); e != nil {
						return e
					}
					return u.state.cameraEnabled(true)
				})
			}
		case 2:
			if v.Sensor == camera.Rear {
				v.Sensor = camera.Front
			} else {
				v.Sensor = camera.Rear
			}
			if !v.Mode.Valid(v.Sensor) {
				v.Mode = camera.Mode{Width: 1920, Height: 1080, FPS: 30}
			}
			u.state.Error(u.configureCamera(v))
		case 3:
			u.menu("CAMERA_DEVICE")
			return
		case 4:
			u.menu("CAMERA_PICTURE")
			u.async(u.refreshCamera3A)
			return
		case 5:
			u.menu("CAMERA_PREVIEW")
			return
		case 6:
			u.menu("CAMERA_METRICS")
			return
		case 18:
			u.menu("CAMERA_TRIAL")
			return
		case 17:
			u.menu("CAMERA_METRICS")
			return
		case 14, 15, 16:
			u.menu([]string{"CAMERA_FOCUS", "CAMERA_EXPOSURE", "CAMERA_WB"}[row-14])
			u.async(u.refreshCamera3A)
			return
		case 12:
			u.menu("CAMERA_PREVIEW")
		case 13:
			u.menu("CAMERA_TORCH")
			return
		case 10:
			u.menu("CAMERA_IMAGE")
			return
		case 11:
			u.menu("CAMERA_MODES")
			return
		}
		u.draw()
	case "CAMERA_DEVICE":
		d := u.menuDetail(menu, row)
		if d.Action && len(d.Options) > 0 {
			u.selectCameraDeviceDetail(row, (d.Selected+1)%len(d.Options))
		}
	case "CAMERA_PICTURE":
		if row > 0 && row < len(lines) && lines[row] == "IMAGE SETTINGS" {
			u.menu("CAMERA_TONE")
			u.async(u.refreshCamera3A)
			return
		}
		switch row {
		case 1, 2, 3:
			u.menu([]string{"CAMERA_FOCUS", "CAMERA_EXPOSURE", "CAMERA_WB"}[row-1])
			u.async(u.refreshCamera3A)
		case 5:
			if u.state.CameraCurrent().Settings.Sensor == camera.Rear {
				u.state.Error(u.state.RequestTorch(!u.state.TorchCurrent().Wanted))
				u.draw()
			}
		}
	case "CAMERA_PREVIEW":
		u.selectPreview(row)
	case "CAMERA_TORCH":
		u.selectTorch(row)
	case "CAMERA_IMAGE":
		v := u.state.CameraCurrent().Settings
		if row == 1 {
			v.Image.Rotation = (v.Image.Rotation + 90) % 360
		} else if row == 2 {
			v.Image.Mirror = !v.Image.Mirror
		} else if row == 6 {
			v.Image.ZoomPercent = uint16(v.Image.Zoom() + 25)
			if v.Image.ZoomPercent > 400 {
				v.Image.ZoomPercent = 100
			}
		} else {
			return
		}
		u.state.Error(u.configureCamera(v))
		u.draw()
	case "CAMERA_MODES":
		modes := u.state.CameraCatalog()
		if row > 0 && row <= len(modes) {
			item := modes[row-1]
			cur := u.state.CameraCurrent()
			if cur.Enabled && !item.Admitted {
				u.state.Error(fmt.Errorf("unsupported mode cannot replace active capture: %s", item.Reason))
				return
			}
			v := cur.Settings
			v.Sensor = item.Sensor
			v.Mode = item.Mode
			u.state.Error(u.configureCamera(v))
			u.menu("CAMERA_MODE_INFO")
		}
	case "AUDIO":
		if row == 1 || row == 2 {
			u.focusRow(row)
		}
		if row == 3 {
			u.menu("AUDIO_STAT")
		}
	case "DIAGNOSTICS":
		if row == 1 {
			u.menu("DIAGNOSTICS_ENABLE")
		} else if row == 2 {
			u.menu("DIAGNOSTICS_CLEAR")
		} else if row == 7 {
			u.menu("DIAGNOSTICS_DISABLE")
		}
	case "DIAGNOSTICS_ENABLE":
		if row == 1 {
			u.async(func() error { return u.openDiagnostics(true) })
			u.menu("DIAGNOSTICS")
		}
	case "DIAGNOSTICS_CLEAR":
		if row == 1 {
			u.async(u.clearDiagnostics)
			u.menu("DIAGNOSTICS")
		}
	case "DIAGNOSTICS_DISABLE":
		if row == 1 {
			u.async(u.disableDiagnostics)
			u.menu("DIAGNOSTICS")
		}
	case "STORAGE":
		if row == 8 {
			u.menu("DIAGNOSTICS")
			return
		}
		if row == 1 {
			u.menu("STORAGE_CONFIRM")
		}
	case "STORAGE_CONFIRM":
		if row == 1 {
			u.async(func() error {
				if u.cache != nil {
					return nil
				}
				c, e := OpenCacheStore(true)
				if e != nil {
					return e
				}
				u.cache = c
				// First opt-in keeps the currently selected preferences, not an old slot.
				_, _ = c.Settings.Load()
				u.state.mu.Lock()
				u.state.Persistence = "CACHE ENABLED"
				u.state.mu.Unlock()
				return u.savePreferences(true)
			})
			u.menu("STORAGE")
		}
	case "SCREEN_ROTATION":
		u.selectRotation(row)
	case "DISPLAY_AUTO":
		u.selectAutoBrightness(row)
	case "DISPLAY":
		u.selectDisplay(row)
	case "DEVICE_POWER":
		if row == 1 {
			u.requestPower(ActionPowerOff)
		} else if row == 2 {
			u.requestPower(ActionReboot)
		} else if row == 3 {
			u.requestPower(ActionRecovery)
		}
	case "POWER":
		if row == 1 {
			u.confirmPower(time.Now())
		}
	case "INDICATOR_SETTINGS":
		u.selectIndicators(row)
	case "DEVICE":
		if row == 1 {
			u.menu("DEVICE_STAT")
			return
		}
		if row == 15 {
			u.menu("MONITOR_LINK")
			return
		}
		if row == 14 {
			u.menu("INDICATOR_SETTINGS")
			return
		}
		if row == 13 {
			u.menu("DIAGNOSTICS")
			return
		}
		if row == 9 {
			u.menu("SCREEN_ROTATION")
			return
		}
		if row == 10 {
			u.menu("DISPLAY")
			return
		}
		if row == 11 {
			u.menu("DEVICE_POWER")
			return
		}
		if row == 6 {
			u.menu("STORAGE")
			return
		}
	}
}
func (u *UI) input(f InputFrame) {
	started := time.Now()
	defer func() { u.inputTiming.observeUI(f, started, time.Now()) }()
	if f.Epoch != u.inputEpoch.Load() {
		u.inputTiming.discarded(inputOldEpoch)
		return
	}
	u.lastInputCount = len(f.Contacts)
	if u.inputAwaitAllUp {
		u.inputTiming.discarded(inputHeldBarrier)
		if !f.Desync && len(f.Contacts) == 0 {
			u.hapticContacts = 0
			u.inputAwaitAllUp = false
			if u.transport != nil {
				_ = u.transport.touch.submit(hid.TouchFrame{})
			}
		}
		return
	}
	// Raw touch remains in natural panel axes until this single UI-owned point.
	// Camera orientation is deliberately unrelated to device display rotation.
	u.state.mu.Lock()
	rotation := u.state.ActiveRotation
	hapticKind := u.state.TouchKind
	u.state.mu.Unlock()
	contacts := append([]hid.Contact(nil), f.Contacts...)
	for i := range contacts {
		if contacts[i].X > 32767 || contacts[i].Y > 32767 {
			f.Desync = true
			break
		}
		contacts[i].X, contacts[i].Y = rotation.NormalizedFromPanel(contacts[i].X, contacts[i].Y)
	}
	f.Contacts = contacts
	if f.Desync {
		u.sniperGesture = sniperGesture{blocked: true}
		u.sniperPress = sniperPress{}
		u.hapticContacts = 0
		u.state.SniperInput(nil)
		if u.transport != nil {
			u.transport.touch.blockInput()
		}
		u.touchDown = false
		u.pad.Reset()
		u.suppressed = 0
		return
	}
	_, _, page := u.state.Current()
	pressed := u.contactPress(f.Contacts)
	feedbackMode := feedbackTouchMode(page, hapticKind, u.state.ActiveView())
	if pressed && feedbackMode {
		u.feedback()
	}
	if page != "" {
		u.state.SniperInput(nil)
		u.menuInput(f.Contacts)
		if len(f.Contacts) == 0 && u.transport != nil && u.transport.touch != nil {
			_ = u.transport.touch.submit(hid.TouchFrame{})
		}
		return
	}
	u.state.mu.Lock()
	kind, sensitivity, acceleration := u.state.TouchKind, u.state.PadSensitivity, u.state.PadAcceleration
	u.state.mu.Unlock()
	if u.previewGesture {
		if len(f.Contacts) == 0 {
			u.previewGesture = false
			u.cameraHUDDragging = false
			if u.transport != nil && u.transport.touch != nil {
				_ = u.transport.touch.submit(hid.TouchFrame{})
			}
		} else if u.cameraHUDDragging && len(f.Contacts) == 1 {
			u.cameraHUDInput(f.Contacts[0].X, f.Contacts[0].Y, true)
		}
		return
	}
	if u.screen != nil {
		if len(f.Contacts) == 1 && u.cameraHUDInput(f.Contacts[0].X, f.Contacts[0].Y, false) {
			if pressed && !feedbackMode {
				u.feedback()
			}
			u.state.SniperInput(nil)
			u.previewGesture = true
			u.pad.Reset()
			if u.transport != nil && u.transport.touch != nil {
				u.transport.touch.blockInput()
			}
			return
		}
		if u.state.ActiveView() == ViewCamera {
			u.pad.Reset()
			if len(f.Contacts) > 0 {
				u.previewGesture = true
				if u.transport != nil && u.transport.touch != nil {
					u.transport.touch.blockInput()
				}
				if len(f.Contacts) == 1 {
					u.focusPreview(f.Contacts[0].X, f.Contacts[0].Y)
				}
			}
			return
		}
		if kind == hid.Touchscreen && u.state.ActiveView() != ViewSniper {
			for _, c := range f.Contacts {
				if u.screen.PreviewHit(c.X, c.Y) {
					u.previewGesture = true
					u.pad.Reset()
					if u.transport != nil && u.transport.touch != nil {
						u.transport.touch.blockInput()
					}
					if len(f.Contacts) == 1 {
						u.focusPreview(c.X, c.Y)
					}
					return
				}
			}
		}
	}
	if u.state.ActiveView() == ViewCamera {
		return
	}
	if u.state.ActiveView() == ViewSniper {
		contacts := f.Contacts
		if len(contacts) > 0 && u.screen != nil {
			width, height, _ := u.screen.Geometry()
			for i := range contacts {
				if mapped, ok := FitContact(contacts[i], width, height); ok {
					contacts[i] = mapped
				} else {
					u.sniperGesture = sniperGesture{blocked: true}
					u.sniperPress = sniperPress{}
					u.state.SniperInput(nil)
					return
				}
			}
		}
		u.sniperInput(contacts)
		return
	}
	if u.transport == nil || u.transport.touch == nil {
		return
	}
	contacts = f.Contacts
	sampleMS, scan := f.sampleTime(started)
	if kind == 2 {
		mapped, e := u.pad.Map(contacts, sampleMS, sensitivity, acceleration)
		if e != nil {
			u.state.Error(e)
			u.transport.touch.blockInput()
			return
		}
		contacts = mapped
	} else {
		u.pad.Reset()
		mask := uint32(0)
		mapped := []hid.Contact{}
		for _, c := range contacts {
			if c.ID > 31 {
				continue
			}
			bit := uint32(1) << c.ID
			mask |= bit
			if u.suppressed&bit != 0 {
				continue
			}
			if kind == 1 && u.screen != nil {
				width, height, _ := u.screen.Geometry()
				v, ok := FitContact(c, width, height)
				if !ok {
					u.suppressed |= bit
					continue
				}
				mapped = append(mapped, v)
			}
		}
		u.suppressed &= mask
		contacts = mapped
	}
	frame := hid.TouchFrame{Kind: kind, Count: byte(len(contacts)), ScanTime: scan}
	if kind == 0 {
		frame.Count = 0
	} else {
		copy(frame.Contacts[:], contacts)
	}
	u.state.Error(u.transport.touch.submit(frame))
}

func (u *UI) key(k uint16) bool {
	if k == 158 || k == 254 {
		u.pulseTouchKey()
	}
	switch k {
	case 172: // Home: main menu, never USB attachment.
		_, _, page := u.state.Current()
		if page == "" || page == "POWER_MENU" {
			u.menu("MONITOR")
		} else {
			u.menu("")
		}
	case 116:
		_, _, page := u.state.Current()
		if page == "POWER_MENU" {
			u.menu("")
		} else {
			u.menuParents = nil
			u.menu("POWER_MENU")
		}
	case 254:
		_, _, page := u.state.Current()
		if page == "" && u.state.ActiveView() == ViewSniper {
			u.state.mu.Lock()
			u.state.Settings.SniperMirror = !u.state.Settings.SniperMirror
			mirror := u.state.Settings.SniperMirror
			u.state.mu.Unlock()
			u.state.SniperInput(nil)
			u.sniperGesture = sniperGesture{blocked: u.lastInputCount > 0}
			u.sniperPress = sniperPress{}
			u.feedback()
			if mirror {
				u.notify("MIRROR ON")
			} else {
				u.notify("MIRROR OFF")
			}
			u.draw()
			break
		}
		if page == "" && u.state.ActiveView() == ViewCamera {
			v := u.state.CameraCurrent().Settings
			if v.Sensor == camera.Front {
				v.Sensor = camera.Rear
			} else {
				v.Sensor = camera.Front
			}
			if !v.Mode.Valid(v.Sensor) {
				v.Mode = camera.Mode{Width: 1920, Height: 1080, FPS: 30}
			}
			u.cameraHUDSelection = ""
			u.state.Error(u.configureCamera(v))
			u.draw()
			break
		}
		// Outside Preview, keep the existing input-mode shortcut.
		u.state.mu.Lock()
		kind := u.state.TouchKind
		u.state.mu.Unlock()
		if kind == hid.Touchpad {
			u.applyInputMode(hid.Touchscreen)
		} else {
			u.applyInputMode(hid.Touchpad)
		}
	case 158:
		_, _, page := u.state.Current()
		if page != "" {
			u.menu(u.parentMenu(page))
		} else if u.state.ActiveView() == ViewCamera && u.cameraHUDSelection != "" {
			u.cameraHUDSelection = ""
			u.draw()
		} else {
			u.cycleView()
		}
	case localPowerHold:
		u.shutdownAction = ActionPowerOff
		return true
	case 114, 115:
		// Media keys are handled on physical edges by volumeInput, never here.
		// No local gain change or synthetic release-only duplicate action.
	}
	return false
}

// async serializes control-plane writes without blocking input or thermal guards.
func (u *UI) async(fn func() error) {
	if u.ops == nil {
		u.state.Error(fmt.Errorf("control worker unavailable"))
		return
	}
	select {
	case u.ops <- fn:
	default:
		u.state.Error(fmt.Errorf("control operation already pending"))
	}
}
func (u *UI) savePreferences(force bool) error {
	// Engineering BOOT cannot replace ordinary saved camera or other preferences.
	if u.usbTrialFPS != 0 {
		return nil
	}
	if u.cache == nil {
		return nil
	}
	current := u.state.Preferences()
	if !force && current == u.saved {
		return nil
	}
	if e := u.cache.Settings.Save(current); e != nil {
		u.state.mu.Lock()
		u.state.Persistence = "CACHE WRITE ERROR"
		u.state.mu.Unlock()
		return e
	}
	u.saved = current
	u.state.mu.Lock()
	u.state.Persistence = "CACHE SAVED"
	u.state.mu.Unlock()
	return nil
}
func (u *UI) controlWorker(ctx context.Context, health *safety.Liveness) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	controlsTick := time.NewTicker(time.Second)
	defer controlsTick.Stop()
	defer func() {
		health.BeginOperation()
		u.state.Error(u.savePreferences(false))
		u.flushFault()
		health.EndOperation()
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-u.state.faults:
			health.BeginOperation()
			u.writeFault(ev)
			health.EndOperation()
		case fn := <-u.ops:
			health.BeginOperation()
			u.state.Error(fn())
			select {
			case u.opsDone <- struct{}{}:
			default:
			}
			u.state.Error(u.savePreferences(false))
			health.EndOperation()
		case <-controlsTick.C:
			_, _, page := u.state.Current()
			if isCamera3APage(page) {
				health.BeginOperation()
				_ = u.refreshCamera3A() // Visible local status; does not clear global errors.
				health.EndOperation()
			}
		case <-tick.C:
			health.BeginOperation()
			u.state.Error(u.savePreferences(false))
			health.EndOperation()
		}
	}
}
func RunNative(parent context.Context, health *safety.Liveness) error {
	if os.Getpid() != 1 || health == nil {
		return fmt.Errorf("native appliance requires PID1 and independent watchdog liveness")
	}
	loadPreviousBootMessages()
	serial, idErr := Identity()
	if idErr != nil {
		return idErr
	}
	if _, err := Thermal(); err != nil {
		return fmt.Errorf("initial native thermal gate: %w", err)
	}
	health.Thermal()
	if cmdline, err := read("/proc/cmdline"); err == nil && chargerBoot(cmdline) {
		return runCharging(parent, health)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	trialFPS, e := loadUSBTrial()
	if e != nil {
		return e
	}
	lab, e := LoadLabPolicy()
	if e != nil {
		return e
	}
	s := NewState()
	backend := camera.NewHerolteProvider()
	if trialFPS != 0 {
		backend, e = camera.NewUSBTrialProvider(trialFPS)
		if e != nil {
			return e
		}
	}
	sharedCamera := camera.NewSharedProvider(ctx, backend)
	u := &UI{usbTrialFPS: trialFPS, presentFrames: newFrameMailbox(), cameraTrialRequests: make(chan uint32, 1), cameraProvider: sharedCamera, background: ctx, state: s, keys: make(chan uint16, 16), frames: make(chan InputFrame, 32), ops: make(chan func() error, 1), opsDone: make(chan struct{}, 1)}
	u.presentFrames.nativeDMA = true
	u.installerDone = make(chan struct{}, 1)
	u.menuPaintJobs = make(chan menuPaintRequest, 1)
	u.menuPaintDone = make(chan menuPaintResult, 1)
	var debugRequests <-chan debugCommand
	if debugUIEnabled("/etc/s7-native.json") {
		u.debug = newDebugUI()
		debugRequests = u.debug.requests
		log.Print("USB UI DIAGNOSTICS ENABLED: on-demand screen capture and menu control")
	}
	u.labMode = lab != nil
	u.hub = sensorhub.New(sensorhub.StartNative)
	u.workers = lifecycle.New(ctx)
	if e := u.startHaptics(); e != nil {
		s.Error(e)
	}
	if e := u.startCameraLED(); e != nil {
		s.Error(e)
	}
	if e := u.startCameraProperties(); e != nil {
		s.Error(e)
	}
	u.orientationEvents = make(chan rotationEvent, 1)
	s.Persistence = "OPENING SETTINGS"
	fatal := make(chan error, 1)
	stop := func(e error) {
		select {
		case fatal <- e:
		default:
		}
		cancel()
		s.Fault("system", e)
	}
	thermalDone := make(chan struct{})
	go func() {
		defer close(thermalDone)
		guard := safety.Guard{}
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			tr, e := Thermal()
			now := time.Now()
			sample := safety.Sample{At: now, Valid: e == nil}
			for _, v := range tr {
				if v.IsSoC() && v.MilliC > sample.CPU {
					sample.CPU = v.MilliC
				}
				if v.Role == "BATTERY" {
					sample.Battery = v.MilliC
				}
			}
			decision := guard.Evaluate(now, sample)
			if decision.Action == safety.Stop {
				stop(fmt.Errorf("native thermal guard: %s (%v)", decision.Reason, e))
				return
			}
			s.SetThermalPaused(decision.Action == safety.Pause, decision.Reason)
			health.Thermal()
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	// User settings are automatic. Only the serial-pinned CACHE is writable;
	// its journal may replay, but no formatting or filesystem repair is attempted.
	if cache, e := OpenCacheStore(trialFPS == 0 && lab == nil); e == nil {
		u.cache = cache
		if prefs, e := cache.Settings.Load(); e == nil {
			s.Error(s.ApplyPreferences(prefs))
			u.saved = prefs
			s.Persistence = "CACHE LOADED"
		} else {
			if !errors.Is(e, os.ErrNotExist) {
				s.Error(e)
			}
			s.Persistence = "CACHE / DEFAULTS"
		}
	} else {
		s.Persistence = "SETTINGS UNAVAILABLE: " + e.Error()
		s.Error(e)
	}
	if e := u.applyUSBTrial(); e != nil {
		return e
	}
	if u.cache != nil {
		_ = u.openDiagnostics(false)
	} else {
		if first, e := ReadCacheDiagnostics(); e == nil && first != nil {
			s.FirstFault = first
			s.DiagnosticStatus = "READ-ONLY RECOVERY / NO CACHE WRITES"
		}
	}
	paths, _ := filepath.Glob("/dev/fb*")
	var framebufferErrors []string
	if len(paths) == 0 {
		paths, _ = filepath.Glob("/dev/graphics/fb*")
	}
	for _, p := range paths {
		b, e := fb.Open(p)
		if e == nil {
			u.screen = b
			break
		}
		framebufferErrors = append(framebufferErrors, fmt.Sprintf("%s: %v", p, e))
		s.Error(e)
	}
	if u.screen != nil {
		s.Error(u.applyRotation(s.Rotation.Manual))
	} else {
		s.Error(fmt.Errorf("no supported framebuffer: USB diagnosis only"))
	}
	u.openPanel()
	u.report = ProbeReport()
	u.report["framebuffer_errors"] = framebufferErrors
	u.report["framebuffer_open"] = u.screen != nil
	_ = SaveReport("/run/boot-probe.json", u.report)
	devices, _ := u.report["v4l2"].([]media.Device)
	device, e := u.selectDecoder(devices)
	u.report["codec_selection"] = u.codecSelection
	u.device = device
	s.Error(e)
	s.SetConsumer(u.monitorBackendReady())
	var diagnosticTouch atomic.Pointer[touchUSB]
	var memory memoryTrace
	var diagnostics diagnosticCache
	snapshot := func() []byte {
		u.reportMu.Lock()
		defer u.reportMu.Unlock()
		report := map[string]any{"native_workers": u.workerSnapshot(), "sensor_hub": u.hub.Snapshot(), "native_media_runtime": mediaruntime.Snapshot(), "boot": u.report, "runtime": s.Snapshot(), "microphone_levels": s.AudioPeakSnapshot(), "kernel_messages": KernelMessages(), "previous_boot_messages": PreviousBootMessages(), "watchdog_failure": health.Failure()}
		report["memory"] = memory.latest.Load()
		report["diagnostic_revision"] = diagnostics.revision.Load()
		report["native_messages"] = RuntimeMessages()
		report["frame_rate"] = s.FrameRate()
		report["synthetic_frame_counter"] = s.SyntheticCounterSnapshot()
		if s.syntheticCounterOn.Load() {
			report["panel_interrupts"] = panelInterruptSnapshot()
		}
		report["windows_endpoints"] = s.EndpointSnapshot()
		report["last_driver_failure"] = s.HostFailureSnapshot()
		report["power_policy"] = s.PowerSnapshot()
		report["haptics"] = s.HapticSnapshot()
		report["camera_led"] = s.CameraLEDSnapshot()
		s.mu.Lock()
		report["installer_status"] = s.InstallerStatus
		report["active_view"] = s.activeViewLocked().String()
		report["menu"] = s.Menu
		s.mu.Unlock()
		report["gpu_probe"] = s.gpuProbeDiagnostic()
		report["ui_diagnostics"] = u.debug != nil
		if u.screen != nil {
			report["live_glass"] = u.screen.LiveGlassStatus()
			report["hardware_presenter"] = u.screen.HardwarePresenterStatus()
		}
		if u.presentFrames != nil {
			report["monitor_presenter"] = u.presentFrames.snapshot()
		}
		if touch := diagnosticTouch.Load(); touch != nil {
			report["pc_volume_keys"] = touch.pcVolumeStatus()
			report["touch_timing"] = touch.touchTiming()
		}
		report["input_timing"] = u.inputTiming.snapshot()
		b, e := json.Marshal(report)
		if e != nil {
			return []byte(`{"error":"marshal"}`)
		}
		return b
	}
	s.Error(sharedCamera.RestoreControls(s.Preferences().Camera3A))
	// SYSTEM also owns the installer payload. Mount it before USB reads the
	// package pin; keep it mounted until renderer and USB users have stopped.
	gpuRoot, closeGPUSystem, gpuErr := mountGPUSystem()
	s.Error(gpuErr)
	if err := u.startGPUMenu(gpuRoot); err != nil {
		u.state.Error(err)
	}
	u.transport, e = NewTransport(ctx, s, serial, diagnostics.read, u.debug)
	startupUSBError := e
	s.Error(e)
	if u.transport != nil {
		diagnosticTouch.Store(u.transport.touch)
		u.transport.prepareCameraControl(sharedCamera)
		if trialFPS == 0 && s.CameraCurrent().Enabled {
			if e = u.transport.EnsureCamera(sharedCamera, s.CameraCurrent().Settings); e != nil {
				s.cameraStatus("CAMERA RESTORE FAILED", e)
				s.Error(e)
			}
		}
		if trialFPS == 0 {
			startupUSBError = u.transport.Toggle()
			s.Error(startupUSBError)
		}
		if trialFPS != 0 {
			e = u.transport.EnsureCamera(sharedCamera, s.CameraCurrent().Settings)
			if e == nil {
				e = s.cameraEnabled(true)
			}
			if e != nil {
				s.cameraStatus("USB TRIAL PREPARATION FAILED", e)
				s.Error(e)
			}
		}
	}
	sensorErr := requestTouchSensor90()
	s.mu.Lock()
	if errors.Is(sensorErr, errTouchSensorRateUnsupported) {
		s.TouchSensorRateStatus = "NATIVE SCAN / 90 HZ HID LIMIT"
		sensorErr = nil
	} else if sensorErr != nil {
		s.TouchSensorRateStatus = "90 HZ REQUEST FAILED"
	} else {
		s.TouchSensorRateStatus = "90 HZ REQUESTED / UNVERIFIED"
	}
	s.mu.Unlock()
	s.Error(sensorErr)
	powerHeld := make(chan struct{}, 1)
	StartInputsWithFeedbackTracked(ctx, false, u.inputEpoch.Load, func(k uint16) {
		if k == localPowerHold {
			select {
			case powerHeld <- struct{}{}:
			default:
			}
			return
		}
		select {
		case u.keys <- k:
		default:
			s.Error(fmt.Errorf("key queue overflow"))
		}
	}, func(v VolumeInput) {
		u.physicalVolumeInput(v)
	}, func(f InputFrame) {
		u.inputTiming.observeRead(f)
		select {
		case u.frames <- f:
		default:
			if u.transport != nil {
				u.transport.touch.blockInput()
			}
			s.Error(fmt.Errorf("evdev queue overflow; contacts released"))
		}
	}, s.Error, func(uint16) { u.feedback() }, u.workers.Start)
	defer func() {
		s.Error(u.saveExitReport("before-worker-stop", nil))
		cancel()
		deadline, endWait := context.WithTimeout(context.Background(), 3*time.Second)
		defer endWait()
		waitErr := u.workers.Stop(deadline)
		if waitErr == nil {
			select {
			case <-thermalDone:
			case <-deadline.Done():
				waitErr = deadline.Err()
			}
		}
		if waitErr != nil {
			s.Fault("shutdown", waitErr)
		}
		s.Error(u.saveExitReport("after-worker-stop", waitErr))
		_ = os.WriteFile("/run/final-status.json", diagnostics.read(), 0600)
		if waitErr != nil {
			log.Printf("workers still own resources until poweroff: %v", waitErr)
			return
		}
		cleanupOK := true
		for _, closeFn := range []func() error{
			u.stopInstallerMedia,
			func() error {
				if u.transport != nil {
					return u.transport.Close()
				}
				return nil
			},
			sharedCamera.Close,
			u.hub.Close,
			func() error {
				if u.screen != nil {
					return u.screen.Close()
				}
				return nil
			},
			func() error {
				if closeGPUSystem != nil && cleanupOK {
					return closeGPUSystem()
				}
				return nil
			},
		} {
			if e := closeFn(); e != nil {
				cleanupOK = false
				s.Fault("shutdown", e)
			}
		}
		u.flushFault()
		s.mu.Lock()
		diagnosticEnabled := s.diagnosticEnabled
		s.mu.Unlock()
		if diagnosticEnabled && cleanupOK && u.diagnosticStore != nil && (u.shutdownAction == ActionPowerOff || u.shutdownAction == ActionReboot) {
			s.Error(u.diagnosticStore.Finish())
		}
		if u.cache != nil {
			s.Error(u.cache.Close())
		}
	}()
	if startupUSBError != nil {
		return fmt.Errorf("native USB startup failed: %w", startupUSBError)
	}
	if err := u.startWorkers(health, stop); err != nil {
		return err
	}
	if err := u.workers.Start("power-policy", func(ctx context.Context) error { u.powerPolicyWorker(ctx); return ctx.Err() }); err != nil {
		return err
	}
	if err := u.workers.Start("memory-trace", func(ctx context.Context) error { return memory.run(ctx, health) }); err != nil {
		return err
	}
	if err := u.startTouchKeyLight(ctx); err != nil {
		u.state.Error(err)
	}
	if err := u.workers.Start("diagnostic-snapshot", func(ctx context.Context) error { return diagnostics.run(ctx, snapshot) }); err != nil {
		return err
	}
	if u.debug != nil && u.cache != nil {
		if err := u.workers.Start("usb-stall-report", u.usbStallReportWorker); err != nil {
			return err
		}
	}
	if lab != nil && u.screen != nil {
		s.Error(u.screen.Clear())
	}
	u.bootReady.Store(true)
	u.draw()
	health.UI()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	drawTick := 0
	var menuTimer *time.Timer
	var menuTick <-chan time.Time
	var sniperTimer *time.Timer
	var sniperTick <-chan time.Time
	var sniperDeadline time.Time
	defer func() {
		if menuTimer != nil {
			menuTimer.Stop()
		}
		if sniperTimer != nil {
			sniperTimer.Stop()
		}
	}()
	u.refreshMonitorLink(time.Now())
	for {
		if !u.debugRecoveryAt.IsZero() && !time.Now().Before(u.debugRecoveryAt) {
			u.shutdownAction = ActionRecovery
		}
		if u.shutdownAction == ActionReboot {
			return ErrRebootRequested
		}
		if u.shutdownAction == ActionRecovery {
			return ErrRecoveryRequested
		}
		if u.shutdownAction == ActionPowerOff {
			return nil
		}
		health.UI()
		select {
		case e := <-fatal:
			return e
		case <-ctx.Done():
			return ctx.Err()
		case <-menuTick:
			menuTimer, menuTick = nil, nil
			u.flushMenuScroll()
		case <-sniperTick:
			sniperTimer, sniperTick = nil, nil
			u.flushSniperPress(time.Now())
		case <-powerHeld:
			return nil
		case k := <-u.keys:
			if u.key(k) {
				return nil
			}
		case command := <-debugRequests:
			u.handleDebugCommand(command)
		case rendered := <-u.menuPaintDone:
			u.acceptMenuPaint(rendered)
		case <-u.opsDone:
			u.refreshNotices(time.Now())
			u.draw()
		case <-u.cameraPropertiesChanged:
			u.draw()
		case <-u.installerDone:
			u.async(u.stopInstallerMedia)
		case f := <-u.frames:
			u.input(f)
		case event := <-u.orientationEvents:
			u.handleRotationEvent(event)
		case <-tick.C:
			u.refreshNotices(time.Now())
			u.refreshMonitorLink(time.Now())
			s.sampleFrameRate(time.Now())
			drawTick++
			u.checkRotationFreshness(time.Now())
			if drawTick%4 == 0 {
				tr, _ := Thermal()
				u.updateTelemetry(tr)
				u.draw()
			}
		}
		u.updateMenuHold(time.Now())
		if u.sniperPress.pending && (sniperTimer == nil || !sniperDeadline.Equal(u.sniperPress.deadline)) {
			if sniperTimer != nil {
				sniperTimer.Stop()
			}
			sniperDeadline = u.sniperPress.deadline
			sniperTimer = time.NewTimer(max(time.Duration(0), time.Until(sniperDeadline)))
			sniperTick = sniperTimer.C
		} else if !u.sniperPress.pending && sniperTimer != nil {
			sniperTimer.Stop()
			sniperTimer, sniperTick = nil, nil
		}
		if u.menuNeedsDraw && menuTimer == nil {
			menuTimer = time.NewTimer(time.Second / 60)
			menuTick = menuTimer.C
		} else if !u.menuNeedsDraw && menuTimer != nil {
			menuTimer.Stop()
			menuTimer, menuTick = nil, nil
		}
	}
}
func (u *UI) decode(ctx context.Context) {
	s := u.state
	var d DecoderBackend
	var codecCtx context.Context
	var cancelCodec context.CancelFunc
	var generation uint32
	var pending *monitor.Frame
	var lastSubmit, lastProgress, retryAt time.Time
	var retrySerial uint64
	budget := safety.RetryBudget{Maximum: 3, Window: time.Minute}
	latched := false
	closeDecoder := func() error {
		defer func() {
			if cancelCodec != nil {
				cancelCodec()
				cancelCodec = nil
			}
			codecCtx = nil
		}()
		if d == nil {
			return nil
		}
		u.presentationMu.Lock()
		if u.presentFrames != nil {
			u.presentFrames.invalidateAll()
		}
		var retireErr error
		if u.screen != nil {
			retireErr = u.screen.ReleaseVideo()
		}
		u.presentationMu.Unlock()
		if retireErr != nil {
			return retireErr
		}
		e := d.Close()
		if e != nil {
			return e
		}
		d = nil
		return nil
	}
	defer func() { s.Error(closeDecoder()) }()
	recoverStream := func(e error) bool {
		s.decoderError(e)
		s.Fault("monitor", e)
		pending = nil
		s.Keyframe()
		if errors.Is(e, media.ErrQuarantined) {
			s.SetConsumer(false)
			return false
		}
		if closeErr := closeDecoder(); closeErr != nil {
			s.Error(closeErr)
			s.SetConsumer(false)
			return false
		}
		delay, ok := budget.Next(time.Now())
		retryAt = time.Now().Add(delay)
		s.mu.Lock()
		s.DecoderRetry++
		s.mu.Unlock()
		if !ok {
			latched = true
			s.SetConsumer(false)
			s.Error(fmt.Errorf("decoder retry limit reached; Monitor menu > RETRY"))
		}
		return true
	}
	// A deliberately replaced/disabled stream is not a decoder fault. Ownership
	// failures remain fatal even if the generation changed during the API call.
	discardStale := func(e error) (stale, safe bool) {
		if s.monitorGenerationCurrent(codecCtx, generation) {
			return false, true
		}
		pending = nil
		if errors.Is(e, media.ErrQuarantined) {
			s.Fault("monitor", e)
			s.SetConsumer(false)
			return true, false
		}
		if closeErr := closeDecoder(); closeErr != nil {
			s.Fault("monitor", closeErr)
			s.SetConsumer(false)
			return true, false
		}
		return true, true
	}
	wait := time.NewTimer(0)
	defer wait.Stop()
	for {
		delay := 5 * time.Millisecond // Compatibility backend without work signals.
		var retired <-chan struct{}
		if native, ok := d.(interface {
			DecodeWork() (bool, <-chan struct{})
		}); ok {
			busy, release := native.DecodeWork()
			retired = release
			delay = 250 * time.Millisecond
			if busy || pending != nil {
				delay = 2 * time.Millisecond
			}
		} else if d == nil && pending == nil {
			delay = 250 * time.Millisecond
		}
		frames := s.frames
		if pending != nil || latched || time.Now().Before(retryAt) {
			frames = nil // Never replace a dependent compressed frame.
		}
		var arrived *monitor.Frame
		changed := s.monitorWake()
		if !wait.Stop() {
			select {
			case <-wait.C:
			default:
			}
		}
		wait.Reset(delay)
		select {
		case <-ctx.Done():
			return
		case frame := <-frames:
			arrived = &frame
		case <-changed:
		case <-retired:
		case <-wait.C:
		}
		cfg, gen, _ := s.Current()
		s.mu.Lock()
		serial := s.RetrySerial
		s.mu.Unlock()
		if serial != retrySerial {
			retrySerial = serial
			latched = false
			budget = safety.RetryBudget{Maximum: 3, Window: time.Minute}
			retryAt = time.Time{}
			if e := closeDecoder(); e != nil {
				s.Error(e)
				s.SetConsumer(false)
				return
			}
			pending = nil
			s.Keyframe()
			s.SetConsumer(u.monitorBackendReady())
		}
		runnable := cfg.Enabled && s.MonitorRunnable() && u.monitorBackendReady()
		if !runnable || gen != generation || (d != nil && !s.monitorGenerationCurrent(codecCtx, generation)) {
			if e := closeDecoder(); e != nil {
				s.Error(e)
				s.SetConsumer(false)
				return
			}
			generation = gen
			pending = nil
		}
		if !runnable || latched || u.device == "" || u.screen == nil || time.Now().Before(retryAt) {
			continue
		}
		if arrived != nil {
			pending = arrived
		}
		if d != nil {
			drain := d.Drain
			latest := false
			// The bounded presenter owns frame replacement and age limits. Keep
			// fresh decode bursts intact instead of discarding them twice.
			if optimized, ok := d.(interface {
				DrainLatest(func(media.Image) error) (int, error)
			}); ok && u.presentFrames == nil {
				drain = optimized.DrainLatest
				latest = true
			}
			n, e := drain(func(im media.Image) error {
				if !s.monitorGenerationCurrent(codecCtx, gen) {
					return nil
				}
				err := u.offerMonitorFrame(im, gen)
				if ctx.Err() != nil && errors.Is(err, os.ErrClosed) {
					return nil
				}
				return err
			})
			if stale, safe := discardStale(e); stale {
				if !safe {
					return
				}
				continue
			}
			if e != nil {
				if !recoverStream(e) {
					return
				}
				continue
			}
			if latest && n > 1 {
				s.markCoalesced(n - 1)
			}
			if n > 0 {
				s.CountDecoded(n)
				lastProgress = time.Now()
			}
		}
		if pending == nil {
			select {
			case f := <-s.frames:
				pending = &f
			default:
			}
		}
		if pending != nil {
			f := pending
			if f.Generation != gen {
				pending = nil
				continue
			}
			if d == nil {
				if !f.Key {
					pending = nil
					s.Keyframe()
					continue
				}
				var e error
				codecCtx, cancelCodec = s.monitorContext(ctx, gen)
				if codecCtx.Err() != nil {
					_ = closeDecoder()
					pending = nil
					continue
				}
				if u.decoderFactory != nil {
					d, e = u.decoderFactory(u.device, f.Payload, f.PTS)
				} else {
					d, e = u.openMonitorDecoder(codecCtx, u.device, f.Payload, f.PTS, cfg.FPS)
				}
				if e == nil && d == nil {
					e = fmt.Errorf("decoder factory returned nil owner")
				}
				if stale, safe := discardStale(e); stale {
					if !safe {
						return
					}
					continue
				}
				if e != nil {
					if !recoverStream(e) {
						return
					}
					continue
				}
				pending = nil
				lastSubmit = time.Now()
				s.markSubmitted(gen, f.PTS, lastSubmit)
				lastProgress = lastSubmit
			} else {
				e := d.Submit(f.Payload, f.PTS)
				if stale, safe := discardStale(e); stale {
					if !safe {
						return
					}
					continue
				}
				if e == nil {
					pending = nil
					lastSubmit = time.Now()
					s.markSubmitted(gen, f.PTS, lastSubmit)
				} else if !media.IsRetry(e) {
					if !recoverStream(e) {
						return
					}
					continue
				}
			}
		}
		if d != nil && time.Since(lastProgress) > 2*time.Second && lastSubmit.After(lastProgress) {
			if !recoverStream(fmt.Errorf("phone H264 decoder made no progress for 2 seconds")) {
				return
			}
		}
	}
}

func (u *UI) updateTelemetry(readings []ThermalReading) {
	hottest, battery, cpu, gpu, isp := int64(-1), int64(-1), int64(-1), int64(-1), int64(-1)
	for _, v := range readings {
		if v.IsSoC() && v.MilliC > hottest {
			hottest = v.MilliC
		}
		switch v.Role {
		case "CPU":
			cpu = max(cpu, v.MilliC)
		case "GPU":
			gpu = max(gpu, v.MilliC)
		case "ISP":
			isp = max(isp, v.MilliC)
		}
		if v.Role == "BATTERY" {
			battery = v.MilliC
		}
	}
	load := -1
	if text, e := read("/proc/stat"); e == nil {
		if now, e := ParseCPU(text); e == nil {
			if u.haveCPU {
				if v, ok := CPUPercent(u.previousCPU, now); ok {
					load = v
				}
			}
			u.previousCPU = now
			u.haveCPU = true
		}
	}
	percent, status, _ := readBattery()
	u.state.mu.Lock()
	defer u.state.mu.Unlock()
	u.state.BatteryPercent = percent
	u.state.BatteryStatus = status
	u.state.CPULoad = load
	u.state.CPUMilliC = cpu
	u.state.GPUMilliC, u.state.ISPMilliC, u.state.SoCMilliC = gpu, isp, hottest
	u.state.BatteryMilliC = battery
}

func menuParent(page string) string {
	switch page {
	case "MONITOR_STAT":
		return "MONITOR"
	case "SPEAKER", "MICROPHONE", "AUDIO_STAT":
		return "AUDIO"
	case "TOUCH_STAT":
		return "INPUT"
	case "LIVE_GLASS":
		return "MONITOR"
	case "PAD_POLLING":
		return "INPUT"
	case "DISPLAY_AUTO", "SCREEN_ROTATION":
		return "DISPLAY"
	case "MONITOR", "INPUT", "CAMERA", "AUDIO", "DEVICE":
		return ""
	case "STORAGE_CONFIRM", "DIAGNOSTICS":
		return "STORAGE"
	case "DIAGNOSTICS_ENABLE", "DIAGNOSTICS_CLEAR", "DIAGNOSTICS_DISABLE":
		return "DIAGNOSTICS"
	case "STORAGE", "DISPLAY", "POWER_SETTINGS", "DRIVER_INSTALL", "PC_VOLUME", "DEVICE_WORKERS", "INDICATOR_SETTINGS", "MONITOR_LINK":
		return "DEVICE"
	case "DEVICE_STAT":
		return "DEVICE"
	case "DEVICE_POWER":
		return ""
	case "POWER":
		return "DEVICE_POWER"
	case "CAMERA_FOCUS", "CAMERA_EXPOSURE", "CAMERA_WB", "CAMERA_TONE":
		return "CAMERA_PICTURE"
	case "CAMERA_TRIAL", "CAMERA_METRICS", "CAMERA_DEVICE", "CAMERA_PICTURE", "CAMERA_IMAGE", "CAMERA_MODES", "CAMERA_PREVIEW", "CAMERA_TORCH":
		return "CAMERA"
	case "CAMERA_TRIAL_CONFIRM":
		return "CAMERA_TRIAL"
	case "CAMERA_MODE_INFO":
		return "CAMERA_MODES"
	default:
		return ""
	}
}

// configureCamera keeps unavailable targets selectable only while capture is off.
// An active producer is not torn down merely because a menu cycles into a mode
// which its provider/immutable USB table cannot produce.
func (u *UI) configureCamera(v camera.Settings) error {
	if err := v.Validate(); err != nil {
		return err
	}
	cam := u.state.CameraCurrent()
	preview := u.state.PreviewCurrent()
	liveCamera := cam.Enabled && cam.LastError == "" && !cam.LastActivity.IsZero() && time.Since(cam.LastActivity) < 2*time.Second
	livePreview := preview.Enabled && preview.LastError == "" && !preview.LastFrame.IsZero() && time.Since(preview.LastFrame) < 2*time.Second
	if liveCamera || livePreview {
		u.async(func() error {
			provider := u.cameraProvider
			if provider == nil {
				provider = camera.HerolteProvider{}
			}
			if err := provider.Available(v); err != nil {
				return fmt.Errorf("active camera mode unchanged: %w", err)
			}
			if u.transport != nil && u.state.CameraCurrent().Enabled {
				if err := u.transport.CameraModeAdmitted(v.Mode); err != nil {
					return err
				}
			}
			return u.state.ConfigureCamera(v)
		})
		return nil
	}
	return u.state.ConfigureCamera(v)
}

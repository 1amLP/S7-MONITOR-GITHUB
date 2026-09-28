package appliance

import "fmt"

func cameraRateLabel(rate uint64) string {
	if rate == 0 {
		return "UNKNOWN"
	}
	return fmt.Sprintf("%d.%03d", rate/1000, rate%1000)
}
func (u *UI) cameraMetricsLines() []string {
	cam := u.state.CameraCurrent()
	s := cam.Counters
	preview := u.state.PreviewCurrent()
	lines := []string{
		"Statistics",
		"FUNCTION: CAMERA",
		fmt.Sprintf("ENABLED: %t", cam.Enabled),
		"STATUS: " + cam.Status,
		"CAMERA: " + cam.Settings.Sensor.String(),
		"SELECTED MODE: " + cam.Settings.Mode.String(),
		"ASPECT RATIO: " + cameraAspect(cam.Settings.Mode),
		fmt.Sprintf("ORIENTATION: %d DEG", cam.Settings.Image.Rotation),
		fmt.Sprintf("CONFIGURED BITRATE: %d MBIT/S", cam.Settings.Bitrate/1_000_000),
		fmt.Sprintf("KEYFRAME: %d S", cam.Settings.GOPSeconds),
		"SENSOR PTS FPS: " + cameraRateLabel(s.SensorFPSMilli),
		"ENCODER PTS FPS: " + cameraRateLabel(s.EncoderFPSMilli),
		fmt.Sprintf("CAPTURED: %d", s.Captured),
		fmt.Sprintf("ENCODED: %d", s.Encoded),
		fmt.Sprintf("USB QUEUED: %d", s.USBQueued),
		fmt.Sprintf("RAW DROPPED: %d", s.RawDropped),
		fmt.Sprintf("AVC DROPPED: %d", s.EncodedDropped),
		fmt.Sprintf("ENCODED BYTES: %d", s.EncodedBytes),
		fmt.Sprintf("USB BYTES: %d", s.USBBytes),
		"PREVIEW: " + preview.Status,
		fmt.Sprintf("PREVIEW FRAMES: %d", preview.Copied),
	}
	if preview.Sensor.Available {
		lines = append(lines, fmt.Sprintf("MEASURED SENSOR ISO: %d", preview.Sensor.ISO), fmt.Sprintf("MEASURED SHUTTER: %.3f MS", float64(preview.Sensor.ExposureNS)/1e6))
	}
	errorText := cam.LastError
	if errorText == "" {
		errorText = "NONE"
	}
	lines = append(lines, "ERROR: "+errorText)
	return append(lines, "BACK: CAMERA")
}

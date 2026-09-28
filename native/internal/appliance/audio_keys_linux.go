//go:build linux && (amd64 || arm64)

package appliance

// Physical volume keys always target Windows, independently of the local
// speaker/microphone level, selected menu row and current touch mode.
func (u *UI) physicalVolumeInput(v VolumeInput) {
	if u.transport != nil && u.transport.touch != nil {
		u.transport.touch.volumeInput(v)
	}
}

package kernelpin

const (
	Baseline     = "3.18.140-ge41817ea9198"
	USBWakeupFix = "3.18.140-gfd26b7e36c45"
	LegacyCamera = "3.18.140-g481bdb278a10"
)

func NativeSupported(release string) bool {
	return release == Baseline || release == USBWakeupFix
}

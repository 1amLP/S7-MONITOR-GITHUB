package fimcshot

// UI selections may predate a PC edit. Preserve unrelated current groups.
// AE/ISO, AF and AWB each form an atomic group because their modes and locks
// constrain one another; Validate still rejects incompatible combinations.
func MergeControls(base, desired, current Controls) Controls {
	if base.AntiBand != desired.AntiBand {
		current.AntiBand = desired.AntiBand
	}
	if base.Brightness != desired.Brightness {
		current.Brightness = desired.Brightness
	}
	if base.Contrast != desired.Contrast {
		current.Contrast = desired.Contrast
	}
	if base.Gamma != desired.Gamma {
		current.Gamma = desired.Gamma
	}
	if base.Sharpness != desired.Sharpness {
		current.Sharpness = desired.Sharpness
	}
	if base.ExposureNS != desired.ExposureNS || base.ISO != desired.ISO || base.Compensation != desired.Compensation || base.AELocked != desired.AELocked || base.AERegion != desired.AERegion {
		current.ExposureNS, current.ISO, current.Compensation = desired.ExposureNS, desired.ISO, desired.Compensation
		current.AELocked, current.AERegion = desired.AELocked, desired.AERegion
	}
	if base.Focus != desired.Focus || base.FocusDioptres != desired.FocusDioptres || base.AFRegion != desired.AFRegion {
		current.Focus, current.FocusDioptres, current.AFRegion = desired.Focus, desired.FocusDioptres, desired.AFRegion
	}
	if base.WhiteBalance != desired.WhiteBalance || base.AWBLocked != desired.AWBLocked || base.WBTemperature != desired.WBTemperature {
		current.WhiteBalance, current.AWBLocked = desired.WhiteBalance, desired.AWBLocked
		current.WBTemperature = desired.WBTemperature
	}
	current.Trigger = FocusIdle
	return current
}

package monitor

// A vendor-specific bulk OUT interface. Full speed permits diagnosis only.
func USBDescriptors() (fs, hs [][]byte) {
	inter := []byte{9, 4, 0, 0, 1, 0xff, 0x53, 0x71, 1}
	return [][]byte{inter, {7, 5, 0x01, 2, 64, 0, 0}}, [][]byte{inter, {7, 5, 0x01, 2, 0, 2, 0}}
}

const InterfaceGUID = "{F4469EB2-7DD9-47F1-AC40-3F10D5477781}"

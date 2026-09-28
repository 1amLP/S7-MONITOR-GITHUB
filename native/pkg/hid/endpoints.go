package hid

const EndpointStateReportID = 10

// Feature-only collection remains present when either digitizer is disabled.
// Appended to native HID; existing collection order/report sizes stay unchanged.
func EndpointStateDescriptor() []byte {
	return []byte{0x06, 0x53, 0xff, 0x09, 3, 0xa1, 1, 0x85, EndpointStateReportID,
		0x15, 0, 0x26, 0xff, 0, 0x75, 8, 0x95, 16, 0x09, 3, 0xb1, 2, 0xc0}
}

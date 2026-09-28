package functionfs

import (
	"encoding/binary"
	"errors"
	"regexp"
)

// Pinned functionfs_s7 ABI: bcdVersion=1, ASCII properties expanded by kernel.
// These are not MS OS wire descriptors, which have different headers/encoding.
func WinUSBDescriptorRecord(fs, hs [][]byte, guid string) ([]byte, error) {
	if !regexp.MustCompile(`^\{[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}\}$`).MatchString(guid) {
		return nil, errors.New("invalid WinUSB interface GUID")
	}
	base, err := descriptorRecord(fs, hs, nil)
	if err != nil {
		return nil, err
	}
	for _, descriptors := range [][][]byte{fs, hs} {
		if err := validateWinUSBEndpoints(descriptors); err != nil {
			return nil, err
		}
	}
	le := binary.LittleEndian
	offset := 12
	if len(fs) > 0 {
		offset += 4
	}
	if len(hs) > 0 {
		offset += 4
	}
	compat := make([]byte, 11+24)
	le.PutUint32(compat[1:], uint32(len(compat)))
	le.PutUint16(compat[5:], 1)
	le.PutUint16(compat[7:], 4)
	compat[9] = 1
	copy(compat[13:], "WINUSB")
	// The S7 composite serializer supports REG_SZ, but not REG_MULTI_SZ.
	name := []byte("DeviceInterfaceGUID\x00")
	value := []byte(guid + "\x00")
	prop := make([]byte, 14+len(name)+len(value))
	le.PutUint32(prop, uint32(len(prop)))
	le.PutUint32(prop[4:], 1)
	le.PutUint16(prop[8:], uint16(len(name)))
	copy(prop[10:], name)
	le.PutUint32(prop[10+len(name):], uint32(len(value)))
	copy(prop[14+len(name):], value)
	ext := make([]byte, 11)
	le.PutUint32(ext[1:], uint32(11+len(prop)))
	le.PutUint16(ext[5:], 1)
	le.PutUint16(ext[7:], 5)
	le.PutUint16(ext[9:], 1)
	ext = append(ext, prop...)
	out := append([]byte{}, base[:offset]...)
	out = append(out, 2, 0, 0, 0)
	out = append(out, base[offset:]...)
	out = append(out, compat...)
	out = append(out, ext...)
	le.PutUint32(out[4:], uint32(len(out)))
	flags := (le.Uint32(out[8:]) | uint32(HasMsOsDesc)) &^ uint32(AllCtrlRecip|Config0Setup)
	le.PutUint32(out[8:], flags)
	return out, nil
}

// This serializer describes one fixed interface, not alternate configurations.
// Reject zero/missing endpoints before the pinned kernel can see the record.
func validateWinUSBEndpoints(ds [][]byte) error {
	if len(ds) == 0 {
		return nil
	}
	if len(ds[0]) != 9 || ds[0][1] != 4 || ds[0][2] != 0 || ds[0][3] != 0 || ds[0][4] == 0 || int(ds[0][4]) != len(ds)-1 {
		return errors.New("S7 WinUSB requires one fixed interface with nonzero matching endpoints")
	}
	seen := map[byte]bool{}
	for _, ep := range ds[1:] {
		if len(ep) != 7 || ep[1] != 5 || ep[2]&0x0f == 0 || ep[2]&0x70 != 0 || ep[3]&3 == 0 || seen[ep[2]] || binary.LittleEndian.Uint16(ep[4:6]) == 0 {
			return errors.New("invalid or duplicate S7 WinUSB endpoint")
		}
		seen[ep[2]] = true
	}
	return nil
}
func (e *Ep0) WriteWinUSBDescriptors(fs, hs [][]byte, guid string) error {
	data, err := WinUSBDescriptorRecord(fs, hs, guid)
	if err != nil {
		return err
	}
	return writeRecord(e.file, data)
}

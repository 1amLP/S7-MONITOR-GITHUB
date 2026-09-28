// Package functionfs provides an interface for interacting with the Linux
// USB FunctionFS for user-space USB gadget implementations.
package functionfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Ep0 struct {
	file *os.File
}

func NewEp0(mountPath string) (*Ep0, error) {
	return openEp0(mountPath, os.O_RDWR)
}

// NewEp0Nonblocking permits canceling an idle event read by closing the file.
func NewEp0Nonblocking(mountPath string) (*Ep0, error) {
	if nonblockFlag == 0 {
		return nil, errors.New("nonblocking FunctionFS requires Linux")
	}
	return openEp0(mountPath, os.O_RDWR|nonblockFlag)
}

func openEp0(mountPath string, flags int) (*Ep0, error) {
	f, err := os.OpenFile(filepath.Join(mountPath, "ep0"), flags, 0)
	if err != nil {
		return nil, err
	}
	return &Ep0{file: f}, nil
}

func (e *Ep0) Close() error {
	return e.file.Close()
}

func (e *Ep0) ReadEvents(buf []Event) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	if len(buf) > 64 {
		return 0, errors.New("at most 64 FunctionFS events may be read at once")
	}
	b := make([]byte, len(buf)*EventWireSize)
	n, err := e.file.Read(b)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	if n%EventWireSize != 0 {
		return 0, errors.New("truncated FunctionFS event")
	}
	for i := 0; i < n/EventWireSize; i++ {
		buf[i], err = decodeEvent(b[i*EventWireSize : (i+1)*EventWireSize])
		if err != nil {
			return 0, err
		}
	}
	return n / EventWireSize, nil
}

func (e *Ep0) ReadEvent() (*Event, error) {
	var events [1]Event
	_, err := e.ReadEvents(events[:])
	if err != nil {
		return nil, err
	}
	return &events[0], nil
}

func decodeEvent(data []byte) (Event, error) {
	if len(data) != EventWireSize || data[8] > byte(EventResume) {
		return Event{}, errors.New("invalid FunctionFS event record")
	}
	return Event{Type: EventType(data[8]), Setup: UsbCtrlRequest{
		RequestType: data[0], Request: data[1],
		Value:  binary.LittleEndian.Uint16(data[2:4]),
		Index:  binary.LittleEndian.Uint16(data[4:6]),
		Length: binary.LittleEndian.Uint16(data[6:8]),
	}}, nil
}

func writeRecord(writer io.Writer, data []byte) error {
	n, err := writer.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func (e *Ep0) Write(data []byte) (int, error) {
	return controlIO(e.file, data, false)
}

func (e *Ep0) WriteDescriptors(fs, hs, ss [][]byte) error {
	data, err := descriptorRecord(fs, hs, ss)
	if err != nil {
		return err
	}
	return writeRecord(e.file, data)
}

func descriptorRecord(fs, hs, ss [][]byte) ([]byte, error) {
	if len(fs) == 0 && len(hs) == 0 && len(ss) == 0 {
		return nil, errors.New("at least one USB speed requires descriptors")
	}
	total := 0
	for _, group := range [][][]byte{fs, hs, ss} {
		if len(group) > 256 {
			return nil, errors.New("too many FunctionFS descriptors")
		}
		for _, descriptor := range group {
			if len(descriptor) < 2 || len(descriptor) > 255 || int(descriptor[0]) != len(descriptor) {
				return nil, errors.New("invalid USB descriptor length")
			}
			total += len(descriptor)
		}
	}
	if total > 65536 {
		return nil, errors.New("FunctionFS descriptor payload exceeds 64 KiB")
	}
	var headerLen uint32 = 12
	var countsLen uint32 = 0
	var dataLen uint32 = 0
	var flags DescsFlags = AllCtrlRecip | Config0Setup

	if len(fs) > 0 {
		flags |= HasFullSpeedDesc
		countsLen += 4
		for _, d := range fs {
			dataLen += uint32(len(d))
		}
	}
	if len(hs) > 0 {
		flags |= HasHighSpeedDesc
		countsLen += 4
		for _, d := range hs {
			dataLen += uint32(len(d))
		}
	}
	if len(ss) > 0 {
		flags |= HasSuperSpeedDesc
		countsLen += 4
		for _, d := range ss {
			dataLen += uint32(len(d))
		}
	}

	totalLen := headerLen + countsLen + dataLen
	buf := make([]byte, totalLen)

	// Header
	binary.LittleEndian.PutUint32(buf[0:4], DescriptorsMagic)
	binary.LittleEndian.PutUint32(buf[4:8], totalLen)
	binary.LittleEndian.PutUint32(buf[8:12], uint32(flags))

	// Counts
	offset := int(headerLen)
	if len(fs) > 0 {
		binary.LittleEndian.PutUint32(buf[offset:offset+4], uint32(len(fs)))
		offset += 4
	}
	if len(hs) > 0 {
		binary.LittleEndian.PutUint32(buf[offset:offset+4], uint32(len(hs)))
		offset += 4
	}
	if len(ss) > 0 {
		binary.LittleEndian.PutUint32(buf[offset:offset+4], uint32(len(ss)))
		offset += 4
	}

	// Data
	writeGroup := func(ds [][]byte) {
		for _, d := range ds {
			copy(buf[offset:], d)
			offset += len(d)
		}
	}

	writeGroup(fs)
	writeGroup(hs)
	writeGroup(ss)

	return buf, nil
}

func (e *Ep0) WriteStrings(lang uint16, strs []string) error {
	data, err := stringRecord(lang, strs)
	if err != nil {
		return err
	}
	return writeRecord(e.file, data)
}

func stringRecord(lang uint16, strs []string) ([]byte, error) {
	if len(strs) > 255 {
		return nil, errors.New("too many USB strings")
	}
	var stringsLen uint32 = 0
	for _, s := range strs {
		if len(s) > 1024 || strings.ContainsRune(s, 0) {
			return nil, fmt.Errorf("invalid USB string length or embedded NUL")
		}
		stringsLen += uint32(len(s)) + 1
	}

	totalLen := uint32(16) + 2 + stringsLen
	buf := make([]byte, totalLen)
	head := StringsHead{
		Magic:     StringsMagic,
		Length:    totalLen,
		StrCount:  uint32(len(strs)),
		LangCount: 1,
	}
	head.Write(buf)

	binary.LittleEndian.PutUint16(buf[16:18], lang)
	offset := 18
	for _, s := range strs {
		copy(buf[offset:], s)
		offset += len(s)
		buf[offset] = 0
		offset++
	}

	return buf, nil
}

func (e *Ep0) ReadExactly(buf []byte) error {
	if len(buf) == 0 {
		// Force a syscall to acknowledge 0-length OUT transfers in FunctionFS.
		// io.ReadFull would skip this.
		return acknowledgeEmptyRead(e.file)
	}
	return readControlRecord(func(data []byte) (int, error) { return controlIO(e.file, data, true) }, buf)
}

// A control transfer is one record. A short read completed that SETUP; reading
// again could consume the next FunctionFS event as if it were payload bytes.
func readControlRecord(read func([]byte) (int, error), data []byte) error {
	n, err := read(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// FunctionFS stalls the current setup when userspace uses its opposite direction.
func (e *Ep0) Stall(in bool) {
	var data [1]byte
	if in {
		_, _ = controlIO(e.file, data[:], true)
	} else {
		_, _ = controlIO(e.file, data[:], false)
	}
}

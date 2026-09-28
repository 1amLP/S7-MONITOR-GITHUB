// Package installermsd serves one immutable installer image, never a block device.
package installermsd

import (
	"encoding/binary"
	"fmt"
)

const BlockSize = 512
const MaxTransfer = 4 << 20

type Command struct {
	Tag, Length uint32
	In          bool
	CDB         []byte
}

func ParseCBW(b []byte) (Command, error) {
	if len(b) != 31 || binary.LittleEndian.Uint32(b) != 0x43425355 || b[12]&0x7f != 0 || b[13] != 0 || b[14] < 1 || b[14] > 16 {
		return Command{}, fmt.Errorf("invalid installer CBW")
	}
	c := Command{Tag: binary.LittleEndian.Uint32(b[4:]), Length: binary.LittleEndian.Uint32(b[8:]), In: b[12] == 0x80, CDB: append([]byte(nil), b[15:15+int(b[14])]...)}
	if c.Length > MaxTransfer {
		return Command{}, fmt.Errorf("installer transfer exceeds bound")
	}
	return c, nil
}

func CSW(c Command, transferred uint32, status byte) [13]byte {
	var b [13]byte
	binary.LittleEndian.PutUint32(b[:], 0x53425355)
	binary.LittleEndian.PutUint32(b[4:], c.Tag)
	if transferred > c.Length {
		transferred = c.Length
		status = 2
	}
	binary.LittleEndian.PutUint32(b[8:], c.Length-transferred)
	b[12] = status
	return b
}

type Reply struct {
	Data      []byte
	Offset    int64
	ReadBytes uint32
	Status    byte
	Eject     bool
}

type Disk struct {
	Bytes    int64
	Serial   string
	key, asc byte
	prevent  bool
}

func New(bytes int64, serial string) (*Disk, error) {
	if bytes < 1<<20 || bytes > 256<<20 || bytes%BlockSize != 0 {
		return nil, fmt.Errorf("invalid installer disk size")
	}
	if len(serial) != 18 {
		return nil, fmt.Errorf("installer serial must be 18 hexadecimal characters")
	}
	for _, c := range serial {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				if c < 'A' || c > 'F' {
					return nil, fmt.Errorf("invalid installer serial")
				}
			}
		}
	}
	return &Disk{Bytes: bytes, Serial: serial}, nil
}
func (d *Disk) Reset()                   { d.key, d.asc, d.prevent = 0, 0, false }
func (d *Disk) fail(key, asc byte) Reply { d.key, d.asc = key, asc; return Reply{Status: 1} }

func (d *Disk) Execute(c Command) Reply {
	if len(c.CDB) == 0 {
		return d.fail(5, 0x24)
	}
	b := c.CDB
	need := 6
	switch b[0] {
	case 0x23, 0x25, 0x28, 0x2a, 0x2f, 0x35, 0x5a:
		need = 10
	case 0xa8, 0xaa:
		need = 12
	case 0x88, 0x8a, 0x9e:
		need = 16
	}
	if len(b) < need {
		return d.fail(5, 0x24)
	}
	metadata := func(data []byte, allocation int) Reply {
		if !c.In && c.Length != 0 {
			return Reply{Status: 2}
		}
		return Reply{Data: data[:min(len(data), allocation, int(c.Length))]}
	}
	switch b[0] {
	case 0x00: // TEST UNIT READY
		if c.Length != 0 {
			return Reply{Status: 2}
		}
		return Reply{}
	case 0x03: // REQUEST SENSE
		data := make([]byte, 18)
		data[0], data[2], data[7], data[12] = 0x70, d.key, 10, d.asc
		d.key, d.asc = 0, 0
		return metadata(data, int(b[4]))
	case 0x12: // INQUIRY and bounded device-identification VPD pages
		if b[1]&2 != 0 {
			return d.fail(5, 0x24)
		}
		var data []byte
		if b[1]&1 != 0 {
			switch b[2] {
			case 0:
				data = []byte{0, 0, 0, 3, 0, 0x80, 0x83}
			case 0x80:
				data = append([]byte{0, 0x80, 0, byte(len(d.Serial))}, []byte(d.Serial)...)
			case 0x83:
				id := []byte("S7-NATIVE-INSTALLER")
				data = append([]byte{0, 0x83, 0, byte(4 + len(id)), 2, 1, 0, byte(len(id))}, id...)
			default:
				return d.fail(5, 0x24)
			}
		} else {
			if b[2] != 0 {
				return d.fail(5, 0x24)
			}
			data = make([]byte, 36)
			data[1], data[2], data[3], data[4] = 0x80, 4, 2, 31
			copy(data[8:16], "S7      ")
			copy(data[16:32], "SETUP DISK      ")
			copy(data[32:36], "0001")
		}
		return metadata(data, int(b[4]))
	case 0x1a, 0x5a: // MODE SENSE: hardware write protection, no block descriptors
		page := b[2] & 0x3f
		if page != 0 && page != 8 && page != 0x3f {
			return d.fail(5, 0x24)
		}
		var pages []byte
		if page != 0 {
			pages = make([]byte, 20)
			pages[0], pages[1] = 8, 18
		}
		if b[0] == 0x1a {
			data := append([]byte{byte(3 + len(pages)), 0, 0x80, 0}, pages...)
			return metadata(data, int(b[4]))
		}
		data := make([]byte, 8)
		data[3] = 0x80
		data = append(data, pages...)
		binary.BigEndian.PutUint16(data, uint16(len(data)-2))
		return metadata(data, int(binary.BigEndian.Uint16(b[7:])))
	case 0x23: // READ FORMAT CAPACITIES
		data := make([]byte, 12)
		data[3] = 8
		binary.BigEndian.PutUint32(data[4:], uint32(d.Bytes/BlockSize))
		data[8], data[10] = 2, 2
		return metadata(data, int(binary.BigEndian.Uint16(b[7:])))
	case 0x25: // READ CAPACITY(10)
		data := make([]byte, 8)
		binary.BigEndian.PutUint32(data, uint32(d.Bytes/BlockSize-1))
		binary.BigEndian.PutUint32(data[4:], BlockSize)
		return metadata(data, 8)
	case 0x9e: // READ CAPACITY(16)
		if b[1]&31 != 0x10 {
			return d.fail(5, 0x24)
		}
		data := make([]byte, 32)
		binary.BigEndian.PutUint64(data, uint64(d.Bytes/BlockSize-1))
		binary.BigEndian.PutUint32(data[8:], BlockSize)
		return metadata(data, min(32, int(binary.BigEndian.Uint32(b[10:]))))
	case 0x28, 0xa8, 0x88: // READ(10/12/16)
		var lba, blocks uint64
		switch b[0] {
		case 0x28:
			lba = uint64(binary.BigEndian.Uint32(b[2:]))
			blocks = uint64(binary.BigEndian.Uint16(b[7:]))
		case 0xa8:
			lba = uint64(binary.BigEndian.Uint32(b[2:]))
			blocks = uint64(binary.BigEndian.Uint32(b[6:]))
		case 0x88:
			lba = binary.BigEndian.Uint64(b[2:])
			blocks = uint64(binary.BigEndian.Uint32(b[10:]))
		}
		capacity := uint64(d.Bytes / BlockSize)
		if lba > capacity || blocks > capacity-lba || blocks > MaxTransfer/BlockSize {
			return d.fail(5, 0x21)
		}
		n := uint32(blocks * BlockSize)
		if !c.In && n != 0 || c.Length != n {
			return Reply{Status: 2}
		}
		return Reply{Offset: int64(lba * BlockSize), ReadBytes: n}
	case 0x1e:
		d.prevent = b[4]&1 != 0
		return Reply{}
	case 0x1b:
		if b[4]&3 == 2 {
			if d.prevent {
				return d.fail(5, 0x53)
			}
			return Reply{Eject: true}
		}
		return Reply{}
	case 0x35:
		return Reply{} // immutable image: nothing to flush
	case 0x0a, 0x2a, 0xaa, 0x8a, 0x15, 0x55, 0x04:
		return d.fail(7, 0x27)
	default:
		return d.fail(5, 0x20)
	}
}

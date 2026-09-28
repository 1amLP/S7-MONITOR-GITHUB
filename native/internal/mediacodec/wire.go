//go:build linux && (amd64 || arm64)

// Package mediacodec talks to an isolated worker using the ORIGINAL Android
// AMediaCodec C API. It is not a renamed direct-MFC backend or software codec.
package mediacodec

import (
	"encoding/binary"
	"fmt"
)

const (
	headerSize    = 64
	maxPayload    = 8 << 20
	opOpen        = 1
	opSubmit      = 2
	opDrain       = 3
	opClose       = 4
	opIDR         = 5
	roleDecode    = 1
	roleEncode    = 2
	storagePixels = 1
)

var le = binary.LittleEndian

type header struct {
	op, serial                              uint32
	status                                  int32
	size, flags                             uint32
	pts                                     uint64
	width, height, fps, bitrate, gop, count uint32
	storage                                 uint32
}

func (h header) marshal() [headerSize]byte {
	var b [headerSize]byte
	copy(b[:4], "S7MC")
	le.PutUint16(b[4:], 3)
	le.PutUint16(b[6:], uint16(h.op))
	le.PutUint32(b[8:], h.serial)
	le.PutUint32(b[12:], uint32(h.status))
	le.PutUint32(b[16:], h.size)
	le.PutUint32(b[20:], h.flags)
	le.PutUint64(b[24:], h.pts)
	for i, v := range []uint32{h.width, h.height, h.fps, h.bitrate, h.gop, h.count} {
		le.PutUint32(b[32+i*4:], v)
	}
	le.PutUint32(b[56:], h.storage)
	return b
}
func response(b []byte, op, serial uint32) (header, error) {
	if len(b) != headerSize || string(b[:4]) != "S7MC" || le.Uint16(b[4:]) != 3 || uint32(le.Uint16(b[6:])) != op|0x8000 || le.Uint32(b[8:]) != serial || le.Uint32(b[60:]) != 0 {
		return header{}, fmt.Errorf("invalid MediaCodec worker response")
	}
	h := header{op: op, serial: serial, status: int32(le.Uint32(b[12:])), size: le.Uint32(b[16:]), flags: le.Uint32(b[20:]), pts: le.Uint64(b[24:]), width: le.Uint32(b[32:]), height: le.Uint32(b[36:]), fps: le.Uint32(b[40:]), bitrate: le.Uint32(b[44:]), gop: le.Uint32(b[48:]), count: le.Uint32(b[52:]), storage: le.Uint32(b[56:])}
	if h.storage > storagePixels || (h.storage != 0 && (op != opDrain || h.size == 0 || h.status != 0 || h.count != 1 || h.flags&2 != 0)) || h.size > maxPayload || h.pts > 1<<63-1 || h.flags&^uint32(3) != 0 || h.count > 1 ||
		(h.status != 0 && (h.size != 0 || h.count != 0 || h.flags != 0)) ||
		(op != opDrain && (h.size != 0 || h.count != 0 || h.flags != 0)) {
		return header{}, fmt.Errorf("invalid MediaCodec worker bounds/status")
	}
	return h, nil
}

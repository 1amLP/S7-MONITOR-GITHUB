//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"fmt"
)

// SplitAnnexB accepts only byte-stream NAL units. It does not guess AVCC lengths.
func SplitAnnexB(data []byte) ([][]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if len(data) > 4<<20 {
		return nil, fmt.Errorf("access unit oversized")
	}
	startCode := func(at int) int {
		if at+3 <= len(data) && data[at] == 0 && data[at+1] == 0 {
			if data[at+2] == 1 {
				return 3
			}
			if at+4 <= len(data) && data[at+2] == 0 && data[at+3] == 1 {
				return 4
			}
		}
		return 0
	}
	pos := 0
	for pos < len(data) && data[pos] == 0 && startCode(pos) == 0 {
		pos++
	}
	if startCode(pos) == 0 {
		return nil, fmt.Errorf("not Annex B")
	}
	out := [][]byte{}
	for pos < len(data) {
		code := startCode(pos)
		if code == 0 {
			return nil, fmt.Errorf("missing NAL start")
		}
		begin := pos + code
		end := begin
		for end < len(data) && startCode(end) == 0 {
			end++
		}
		stop := end
		for stop > begin && data[stop-1] == 0 {
			stop--
		}
		if stop == begin || data[begin]&0x80 != 0 || data[begin]&31 == 0 {
			return nil, fmt.Errorf("invalid NAL")
		}
		out = append(out, data[begin:stop])
		if len(out) > 512 {
			return nil, fmt.Errorf("too many NAL units")
		}
		pos = end
	}
	return out, nil
}

// DecoderStartup separates stream configuration from the first picture. The
// Samsung stateful MFC consumes SPS/PPS to report capture geometry, then needs a
// distinct IDR buffer after capture setup. Both outputs remain Annex B.
func DecoderStartup(data []byte) (config, picture []byte, err error) {
	if _, _, err = MonitorKeyDimensions(data); err != nil {
		return nil, nil, err
	}
	nals, err := SplitAnnexB(data)
	if err != nil {
		return nil, nil, err
	}
	haveSPS, havePPS, haveIDR := false, false, false
	appendNAL := func(dst []byte, n []byte) []byte {
		dst = append(dst, 0, 0, 0, 1)
		return append(dst, n...)
	}
	for _, n := range nals {
		switch n[0] & 31 {
		case 7:
			haveSPS = true
			config = appendNAL(config, n)
		case 8:
			havePPS = true
			config = appendNAL(config, n)
		default:
			haveIDR = haveIDR || n[0]&31 == 5
			picture = appendNAL(picture, n)
		}
	}
	if !haveSPS || !havePPS || !haveIDR || len(config) < 10 || len(config) > 64<<10 || len(picture) < 5 || len(picture) > 1<<20 {
		return nil, nil, fmt.Errorf("invalid H264 decoder startup units")
	}
	return config, picture, nil
}

// AccessUnitAssembler holds codec parameters for exactly one encoder session.
// Separate header buffers are legal and are never counted as captured frames.
// A new instance MUST be used after a mode/sensor/encoder change.
type AccessUnitAssembler struct {
	settings     EncodeSettings
	sps, pps     []byte
	spsID, ppsID uint32
	needKey      bool
}

func NewAccessUnitAssembler(c EncodeSettings) (*AccessUnitAssembler, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &AccessUnitAssembler{settings: c, needKey: true}, nil
}
func parameterIDs(n []byte, pps bool) (uint32, uint32, error) {
	if len(n) < 2 {
		return 0, 0, fmt.Errorf("short PPS/slice")
	}
	var rb []byte
	var err error
	if pps {
		rb, err = RBSP(n[1:])
	} else {
		rb, err = rbspPrefix(n[1:], 64)
	}
	if err != nil {
		return 0, 0, err
	}
	b := bits{p: rb}
	if pps {
		pid, sid := b.ue(), b.ue()
		if b.err != nil || pid > 255 || sid > 31 {
			return 0, 0, fmt.Errorf("invalid PPS ids: %v", b.err)
		}
		return pid, sid, nil
	}
	first, typ, pid := b.ue(), b.ue(), b.ue()
	if b.err != nil || typ > 9 || typ%5 == 1 || pid > 255 {
		return 0, 0, fmt.Errorf("invalid/reordered slice syntax: %v", b.err)
	}
	return first, pid, nil
}
func (a *AccessUnitAssembler) Push(data []byte, pts int64) (Encoded, bool, error) {
	nals, err := SplitAnnexB(data)
	if err != nil {
		return Encoded{}, false, err
	}
	// Keep state transactional: an invalid new PPS must not poison the previous
	// complete header pair. Copies are bounded and occur only when headers arrive.
	sps, pps, sid, pid := a.sps, a.pps, a.spsID, a.ppsID
	needKey := a.needKey
	key, hasVCL, haveFirst := false, false, false
	var lastFirst uint32
	var body []byte
	for _, n := range nals {
		typ := n[0] & 31
		switch typ {
		case 7:
			if hasVCL || len(n) > 32<<10 {
				return Encoded{}, false, fmt.Errorf("late/oversized SPS")
			}
			info, err := InspectSPS(n)
			if err != nil {
				return Encoded{}, false, err
			}
			if err = info.CheckMode(a.settings); err != nil {
				return Encoded{}, false, err
			}
			if !bytes.Equal(n, sps) {
				needKey = true
				pps = nil
				pid = 0
			}
			sps = append([]byte(nil), n...)
			sid = info.ID
		case 8:
			if hasVCL || len(n) > 32<<10 {
				return Encoded{}, false, fmt.Errorf("late/oversized PPS")
			}
			npid, nsid, err := parameterIDs(n, true)
			if err != nil {
				return Encoded{}, false, err
			}
			if len(sps) == 0 || nsid != sid {
				return Encoded{}, false, fmt.Errorf("PPS references unavailable SPS")
			}
			if !bytes.Equal(n, pps) {
				needKey = true
			}
			pps = append([]byte(nil), n...)
			pid = npid
		case 1, 5:
			first, slicePID, err := parameterIDs(n, false)
			if err != nil {
				return Encoded{}, false, err
			}
			if len(sps) == 0 || len(pps) == 0 || slicePID != pid {
				return Encoded{}, false, fmt.Errorf("slice references unavailable PPS")
			}
			if (!haveFirst && first != 0) || (haveFirst && first <= lastFirst) {
				return Encoded{}, false, fmt.Errorf("multiple or unordered pictures in encoder buffer")
			}
			if hasVCL && key != (typ == 5) {
				return Encoded{}, false, fmt.Errorf("mixed IDR/non-IDR picture")
			}
			lastFirst, haveFirst = first, true
			hasVCL, key = true, typ == 5
		case 2, 3, 4, 19, 20, 21:
			return Encoded{}, false, fmt.Errorf("unsupported AVC slice extension %d", typ)
		}
		if typ != 7 && typ != 8 {
			body = append(body, 0, 0, 0, 1)
			body = append(body, n...)
		}
	}
	if hasVCL && pts < 0 {
		return Encoded{}, false, fmt.Errorf("negative encoded timestamp")
	}
	if hasVCL && needKey && !key {
		return Encoded{}, false, fmt.Errorf("new parameter set requires actual IDR")
	}
	a.sps, a.pps, a.spsID, a.ppsID = sps, pps, sid, pid
	a.needKey = needKey && !key
	if !hasVCL {
		return Encoded{}, false, nil
	}
	out := Encoded{PTS: pts, Key: key}
	if key {
		out.Data = append(out.Data, 0, 0, 0, 1)
		out.Data = append(out.Data, sps...)
		out.Data = append(out.Data, 0, 0, 0, 1)
		out.Data = append(out.Data, pps...)
	}
	out.Data = append(out.Data, body...)
	if len(out.Data) > 4<<20 {
		return Encoded{}, false, fmt.Errorf("encoded packet oversized")
	}
	return out, true, nil
}
func (e *Encoder) packet(data []byte, pts int64) (Encoded, bool, error) {
	if e.assembler == nil {
		var err error
		e.assembler, err = NewAccessUnitAssembler(e.settings)
		if err != nil {
			return Encoded{}, false, err
		}
	}
	return e.assembler.Push(data, pts)
}

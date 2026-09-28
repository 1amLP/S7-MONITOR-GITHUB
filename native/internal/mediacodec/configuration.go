//go:build linux && (amd64 || arm64)

package mediacodec

import (
	"bytes"
	"fmt"
	"perimode/native/internal/media"
)

// decoderConfiguration sends SPS/PPS through MediaFormat, BEFORE start(). The
// first AU then contains only picture/SEI/AUD data; CSD is not submitted twice.
func decoderConfiguration(first []byte) (config, picture []byte, err error) {
	if err = media.Validate720pKey(first); err != nil {
		return nil, nil, err
	}
	nals, err := media.SplitAnnexB(first)
	if err != nil {
		return nil, nil, err
	}
	var sps, pps []byte
	for _, n := range nals {
		var target *[]byte
		switch n[0] & 31 {
		case 7:
			target = &sps
		case 8:
			target = &pps
		default:
			picture = append(picture, 0, 0, 0, 1)
			picture = append(picture, n...)
			continue
		}
		// One stream configuration; exact repeated copies in the startup AU are safe.
		if *target != nil {
			if !bytes.Equal((*target)[4:], n) {
				return nil, nil, fmt.Errorf("conflicting initial H264 parameter sets")
			}
			continue
		}
		*target = append([]byte{0, 0, 0, 1}, n...)
	}
	if len(sps) < 5 || len(pps) < 5 || len(sps)+len(pps)+8 > 65536 || len(picture) < 5 || len(picture) > 1<<20 {
		return nil, nil, fmt.Errorf("invalid MediaCodec initial SPS/PPS/picture bounds")
	}
	config = make([]byte, 8, 8+len(sps)+len(pps))
	le.PutUint32(config, uint32(len(sps)))
	le.PutUint32(config[4:], uint32(len(pps)))
	config = append(config, sps...)
	config = append(config, pps...)
	return config, picture, nil
}

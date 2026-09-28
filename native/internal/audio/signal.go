package audio

import (
	"encoding/binary"
	"fmt"
)

// Five milliseconds of ramp after startup/XRUN avoids a discontinuous jump
// from the priming silence into an arbitrary waveform phase.
func fadeInPCM(data []byte, channels, remaining int) int {
	for off := 0; remaining > 0 && off+channels*2 <= len(data); off += channels * 2 {
		gain := 241 - remaining
		for ch := 0; ch < channels; ch++ {
			i := off + ch*2
			sample := int32(int16(binary.LittleEndian.Uint16(data[i:])))
			binary.LittleEndian.PutUint16(data[i:], uint16(int16(sample*int32(gain)/240)))
		}
		remaining--
	}
	return remaining
}

// ConvertPCM applies software attenuation only (0..100%). A physical mono speaker
// is fed equal channels after averaging; never sum two full-scale samples.
func ConvertPCM(src []byte, inChannels, outChannels, volume int, monoSpeaker bool) ([]byte, error) {
	return ConvertPCMInto(nil, src, inChannels, outChannels, volume, monoSpeaker)
}

// ConvertPCMInto reuses caller-owned storage. Every returned byte is written;
// old audio cannot survive a shorter block or an XRUN reset.
func ConvertPCMInto(dst, src []byte, inChannels, outChannels, volume int, monoSpeaker bool) ([]byte, error) {
	if (inChannels != 1 && inChannels != 2) || (outChannels != 1 && outChannels != 2) || volume < 0 || volume > 100 || len(src)%(inChannels*2) != 0 {
		return nil, fmt.Errorf("invalid audio conversion")
	}
	n := len(src) / (inChannels * 2)
	if n > 48000 {
		return nil, fmt.Errorf("audio block too large")
	}
	bytes := n * outChannels * 2
	if cap(dst) < bytes {
		dst = make([]byte, bytes)
	} else {
		dst = dst[:bytes]
	}
	for i := 0; i < n; i++ {
		l := int32(int16(binary.LittleEndian.Uint16(src[i*inChannels*2:])))
		r := l
		if inChannels == 2 {
			r = int32(int16(binary.LittleEndian.Uint16(src[i*4+2:])))
		}
		if monoSpeaker || outChannels == 1 {
			l = (l + r) / 2
			r = l
		}
		l = l * int32(volume) / 100
		r = r * int32(volume) / 100
		binary.LittleEndian.PutUint16(dst[i*outChannels*2:], uint16(int16(l)))
		if outChannels == 2 {
			binary.LittleEndian.PutUint16(dst[i*4+2:], uint16(int16(r)))
		}
	}
	return dst, nil
}

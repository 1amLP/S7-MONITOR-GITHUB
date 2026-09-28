package fimcshot

// A capture-node camera2_stream is NOT a camera2_shot_ext. Only the prefix
// read by the supplied HAL is decoded here. Its full extent and image extents
// must be supplied by G_FMT; the undocumented tail is deliberately opaque.
import "fmt"

const (
	StreamHeaderSize        = 0x14
	StreamFrameCountOffset  = 0x4
	StreamValidOffset       = 0x10
	DynamicFrameCountOffset = 0x29a0
)

type Stream struct {
	FrameCount uint32
	Valid      bool
}

// ReadStream does not accept a shot marker in place of stream validity. A zero
// frame count or invalid result is a completed but unusable capture, not DMA
// ownership loss. The queue may recycle it after successful DQBUF.
func ReadStream(data []byte) (Stream, error) {
	if len(data) < StreamHeaderSize {
		return Stream{}, fmt.Errorf("short camera2_stream prefix")
	}
	s := Stream{FrameCount: le.Uint32(data[StreamFrameCountOffset:]), Valid: le.Uint32(data[StreamValidOffset:]) != 0}
	if s.FrameCount == 0 || !s.Valid {
		return s, fmt.Errorf("invalid camera2_stream result")
	}
	return s, nil
}

// DynamicFrameCount is the completed sensor request number, not the vb2 slot
// index, USB sequence, or a locally generated timestamp.
func (v View) DynamicFrameCount() (uint32, error) {
	if e := v.valid(); e != nil {
		return 0, e
	}
	n := le.Uint32(v.data[DynamicFrameCountOffset:])
	if n == 0 {
		return 0, fmt.Errorf("missing dynamic frame count")
	}
	return n, nil
}

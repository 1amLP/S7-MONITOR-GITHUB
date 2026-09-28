//go:build linux && (amd64 || arm64)

package fimcdma

import (
	"fmt"
	"perimode/native/internal/media"
	"unsafe"
)

// Observed in the supplied ExynosCameraMCPipe::m_setSetfile at 0x7e788/0x7e790.
// A profile must explicitly choose the node-control path; other modes carry the
// merged setfile/YUV-range word in shot metadata. Never try arbitrary private CIDs.
const SetfileCID uint32 = 0x009a1033

func (n *PreparedNode) Setfile(value uint32) error {
	if n == nil || n.expected == nil || n.released || n.importsRequested || n.kind != media.Output || n.role != ShotMetadata || value > 0xffff {
		return fmt.Errorf("setfile requires a configured output leader and 16-bit selector")
	}
	c := media.Control{ID: SetfileCID, Value: int32(value)}
	if e := n.ioctl(media.SetControl, unsafe.Pointer(&c)); e != nil {
		return fmt.Errorf("FIMC setfile: %w", e)
	}
	if c.ID != SetfileCID || c.Value != int32(value) {
		return fmt.Errorf("FIMC altered setfile selector")
	}
	return nil
}

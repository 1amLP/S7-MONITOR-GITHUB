//go:build linux && (amd64 || arm64)

package media

import (
	"fmt"
	"syscall"
	"time"
)

func (s *RGBScaler) queueDMAInput() error {
	l := s.sourceLease
	if !l.Live() {
		return fmt.Errorf("scaler source lease missing")
	}
	p := make([]Plane, len(l.Planes))
	for i, v := range l.Planes {
		rows := l.Height
		if i == 1 {
			rows /= 2
		}
		p[i] = Plane{Memory: uint64(v.FD), Length: v.Length, Offset: v.Offset, Used: v.Offset + v.Stride*uint32(rows)}
	}
	b := Buffer{Type: Output, Memory: MemoryDMABuf, Index: 0}
	return s.bufferCall(QueueBuffer, &b, p)
}
func (s *RGBScaler) dequeueDMAInput(deadline time.Time) error {
	for {
		p := make([]Plane, 2)
		b := Buffer{Type: Output, Memory: MemoryDMABuf}
		err := s.bufferCall(DequeueBuffer, &b, p)
		if err == syscall.EAGAIN && time.Now().Before(deadline) {
			if e := waitVideo(int(s.file.Fd()), 4, deadline); e != nil {
				return e
			}
			continue
		}
		if err != nil {
			return err
		}
		if b.Index != 0 || b.Flags&0x40 != 0 || len(p) != len(s.sourceLease.Planes) {
			return fmt.Errorf("invalid scaler DMA input completion")
		}
		for i, v := range s.sourceLease.Planes {
			if p[i].Memory != uint64(v.FD) || p[i].Length != v.Length {
				return fmt.Errorf("scaler DMA input identity changed")
			}
		}
		return nil
	}
}

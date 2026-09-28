package media

import (
	"errors"
	"fmt"
	"syscall"
)

type decodedBuffer struct {
	Index int
	Image Image
	Empty bool
}

// drainDecoded never drops compressed pictures or references. With latest=true
// it recycles obsolete, already DECODED capture buffers and presents the newest
// completed buffer once. The held buffer remains owned until the synchronous
// consumer returns. On errors the decoder's existing STREAMOFF/quarantine path
// retains responsibility for any outstanding DMA mappings.
func drainDecoded(limit int, latest bool, next func() (decodedBuffer, error), release func(int) error, present func(Image) error) (count int, err error) {
	if limit < 1 || limit > 32 || next == nil || release == nil || present == nil {
		return 0, fmt.Errorf("invalid decoded batch contract")
	}
	count = 0
	held := -1
	defer func() {
		if held >= 0 {
			err = errors.Join(err, release(held))
		}
	}()
	var image Image
	for j := 0; j < limit; j++ {
		b, e := next()
		if errors.Is(e, syscall.EAGAIN) {
			break
		}
		if e != nil {
			return count, e
		}
		if b.Index < 0 || b.Index >= limit || b.Index == held {
			held = -1 // Corrupt queue ownership is reclaimed by STREAMOFF, never QBUF.
			return count, fmt.Errorf("invalid/reused held capture buffer")
		}
		if b.Empty {
			if e = release(b.Index); e != nil {
				return count, e
			}
			continue
		}
		count++
		if latest {
			if held >= 0 {
				retired := held
				held = -1
				if e = release(retired); e != nil {
					return count, e
				}
			}
			held, image = b.Index, b.Image
		} else {
			if e = present(b.Image); e != nil {
				return count, errors.Join(e, release(b.Index))
			}
			if e = release(b.Index); e != nil {
				return count, e
			}
		}
	}
	if held >= 0 {
		if e := present(image); e != nil {
			return count, e
		}
		if e := release(held); e != nil {
			held = -1
			return count, e
		}
		held = -1
	}
	return count, nil
}

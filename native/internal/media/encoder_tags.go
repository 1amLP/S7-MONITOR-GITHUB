package media

import (
	"fmt"
	"syscall"
)

// Samsung's encoder returns E_RET_PICTURE_TAG, not the input timeval, on its
// CAPTURE buffer. Keep actual submitted PTS until that exact hardware tag returns.
type encoderTags struct {
	next    int32
	entries [16]struct {
		tag int32
		pts int64
	}
}

func (t *encoderTags) reserve(pts int64) (int32, error) {
	if pts < 0 || t.next == 0x7fffffff {
		return 0, fmt.Errorf("invalid/exhausted MFC frame tag")
	}
	for i := range t.entries {
		if t.entries[i].tag == 0 {
			t.next++
			t.entries[i].tag, t.entries[i].pts = t.next, pts
			return t.next, nil
		}
	}
	return 0, syscall.EAGAIN
}

func (t *encoderTags) take(tag int32) (int64, error) {
	if tag > 0 {
		for i := range t.entries {
			if t.entries[i].tag == tag {
				pts := t.entries[i].pts
				t.entries[i].tag, t.entries[i].pts = 0, 0
				return pts, nil
			}
		}
	}
	return 0, fmt.Errorf("MFC returned unknown/completed frame tag %d", tag)
}

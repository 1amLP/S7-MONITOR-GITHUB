package media

import "fmt"

// nv12Upload binds one dequeued MMAP slot to its negotiated layout. It is made
// once at encoder open, not for every frame. Only copyFrame touches pixel bytes;
// construction/validation never partially overwrite a malformed allocation.
// The caller must own the slot (not queued to the driver).
type nv12Upload struct {
	width, height, yStride, uvStride int
	y, uv, tail                      []byte
	used, lengths                    [2]uint32
	planes                           [2]Plane
	count                            int
}

func prepareNV12Upload(dst [][]byte, f Format, c EncodeSettings) (nv12Upload, error) {
	var u nv12Upload
	if err := c.Validate(); err != nil {
		return u, err
	}
	if err := validateRawFormat(f, c); err != nil {
		return u, err
	}
	if len(dst) != int(f.Planes()) {
		return u, fmt.Errorf("encoder plane count mismatch")
	}
	u.width, u.height = int(c.Width), int(c.Height)
	u.yStride, u.uvStride = int(f.Stride(0)), int(f.Stride(1))
	if u.uvStride == 0 || len(dst) == 1 {
		u.uvStride = u.yStride
	}
	yn, un := u.yStride*u.height, u.uvStride*(u.height/2)
	u.count = len(dst)
	for i, p := range dst {
		// Matches Decoder.allocate; never truncate a mapping length into uint32.
		if len(p) == 0 || len(p) > 16<<20 {
			return nv12Upload{}, fmt.Errorf("invalid encoder mapping size")
		}
		u.lengths[i] = uint32(len(p))
	}
	if u.count == 1 {
		n := int(f.PlaneSize(0))
		if n == 0 {
			n = yn + un
		}
		if n < yn+un || len(dst[0]) < n {
			return nv12Upload{}, fmt.Errorf("short encoder NV12 allocation")
		}
		// Standard contiguous NV12: UV immediately follows height*bytesperline.
		// No guessed vendor alignment is inserted. The negotiated format must be
		// linear NV12; formats with independent planes use the NM12 path below.
		u.y, u.uv, u.tail = dst[0][:yn], dst[0][yn:yn+un], dst[0][yn+un:n]
		u.used[0] = uint32(n)
	} else {
		ynAlloc, unAlloc := int(f.PlaneSize(0)), int(f.PlaneSize(1))
		if ynAlloc == 0 {
			ynAlloc = yn
		}
		if unAlloc == 0 {
			unAlloc = un
		}
		if ynAlloc < yn || unAlloc < un || len(dst[0]) < ynAlloc || len(dst[1]) < unAlloc {
			return nv12Upload{}, fmt.Errorf("short encoder NV12M allocation")
		}
		u.y, u.uv = dst[0][:ynAlloc], dst[1][:unAlloc]
		u.used[0], u.used[1] = uint32(ynAlloc), uint32(unAlloc)
	}
	return u, nil
}

// fillByte uses bulk copies for padding. Visible image bytes are never cleared
// and then overwritten a second time; padding is still reset on EVERY reuse.
func fillByte(dst []byte, value byte) {
	if len(dst) == 0 {
		return
	}
	if value == 0 {
		clear(dst)
		return
	}
	dst[0] = value
	for n := 1; n < len(dst); {
		n += copy(dst[n:], dst[:n])
	}
}

func uploadPlane(dst, src []byte, width, rows, dstStride, srcStride int, black byte) {
	if srcStride == width && dstStride == width {
		copy(dst[:width*rows], src[:width*rows])
	} else {
		for row := 0; row < rows; row++ {
			d, s := row*dstStride, row*srcStride
			copy(dst[d:d+width], src[s:s+width])
			fillByte(dst[d+width:d+dstStride], black)
		}
	}
	fillByte(dst[rows*dstStride:], black)
}

func (u *nv12Upload) copyFrame(im Image) ([]Plane, error) {
	w, h := u.width, u.height
	if u.count < 1 || u.count > 2 || w <= 0 || h <= 0 ||
		im.Width != w || im.Height != h ||
		im.StrideY < w || im.StrideUV < w || im.StrideY > 16384 || im.StrideUV > 16384 ||
		len(im.Y) < (h-1)*im.StrideY+w || len(im.UV) < (h/2-1)*im.StrideUV+w {
		return nil, fmt.Errorf("raw camera buffer does not match native mode")
	}
	// All source/destination bounds are checked before the first write.
	uploadPlane(u.y, im.Y, w, h, u.yStride, im.StrideY, 16)
	uploadPlane(u.uv, im.UV, w, h/2, u.uvStride, im.StrideUV, 128)
	clear(u.tail)
	for i := 0; i < u.count; i++ {
		u.planes[i] = Plane{Used: u.used[i], Length: u.lengths[i]}
	}
	return u.planes[:u.count], nil
}

// CopyNV12 remains the one-shot entry point for diagnostics. Encoder.Submit uses
// its prebuilt per-slot plan and therefore does not allocate a plan per frame.
func CopyNV12(dst [][]byte, f Format, c EncodeSettings, im Image) ([]Plane, error) {
	u, err := prepareNV12Upload(dst, f, c)
	if err != nil {
		return nil, err
	}
	return u.copyFrame(im)
}

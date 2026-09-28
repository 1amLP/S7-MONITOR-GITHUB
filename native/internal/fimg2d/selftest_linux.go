//go:build linux && (amd64 || arm64)

package fimg2d

import (
	"context"
	"encoding/binary"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

const (
	testSide  = 64
	testBytes = testSide * testSide * 4
)

type SelfTestResult struct {
	CopyRGB  [4]uint32
	BlendRGB [4]uint32
	CopyOK   bool
	BlendOK  bool
	CopyUS   int64
	BlendUS  int64
}

type selfTestBuffers struct {
	opaqueCopy []byte
	opaqueBlue []byte
	halfRed    []byte
}

func makeTestPattern(color uint32) ([]byte, error) {
	b, err := syscall.Mmap(-1, 0, testBytes, syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(b); i += 4 {
		binary.LittleEndian.PutUint32(b[i:], color)
	}
	return b, nil
}

func newSelfTestBuffers() (_ *selfTestBuffers, err error) {
	buffers := &selfTestBuffers{}
	defer func() {
		if err != nil {
			_ = buffers.close()
		}
	}()
	if buffers.opaqueCopy, err = makeTestPattern(0xff2468ac); err != nil {
		return nil, err
	}
	if buffers.opaqueBlue, err = makeTestPattern(0xff0000ff); err != nil {
		return nil, err
	}
	if buffers.halfRed, err = makeTestPattern(0x80800000); err != nil {
		return nil, err
	}
	return buffers, nil
}

func (b *selfTestBuffers) close() error {
	if b == nil {
		return nil
	}
	var err error
	for _, item := range []*[]byte{&b.opaqueCopy, &b.opaqueBlue, &b.halfRed} {
		if len(*item) == 0 {
			continue
		}
		if e := syscall.Munmap(*item); e != nil {
			err = e
		} else {
			*item = nil
		}
	}
	return err
}

func testImage(data []byte, flags uint32, composite uint16) image {
	r := Rect{W: testSide, H: testSide}
	return userImage(data, testSide, testSide, r, r, flags, composite, 0)
}

func checkSelfTestColor(samples *[4]uint32, target []byte, expectCopy bool) bool {
	all := true
	for off := 0; off < len(target); off += 4 {
		p := binary.LittleEndian.Uint32(target[off:])
		if expectCopy {
			all = all && p == 0xff2468ac
		} else {
			a, r, g, bl := p>>24, (p>>16)&255, (p>>8)&255, p&255
			all = all && a == 255 && r >= 126 && r <= 130 && g <= 2 && bl >= 125 && bl <= 129
		}
	}
	for i, off := range []int{0, (testSide - 1) * 4, (testSide - 1) * testSide * 4,
		(testSide-1)*testSide*4 + (testSide-1)*4} {
		samples[i] = binary.LittleEndian.Uint32(target[off:])
	}
	return all
}

// runSelfTest never addresses the visible half. An uncertain ioctl keeps every
// source and target mapping alive until reboot; a completed mismatch may close.
func runSelfTest(ctx context.Context, mapped []byte, b *selfTestBuffers, process processTask) (result SelfTestResult, uncertain bool, err error) {
	if ctx == nil || process == nil || b == nil || len(mapped) != 2*frameBytes ||
		len(b.opaqueCopy) != testBytes || len(b.opaqueBlue) != testBytes || len(b.halfRed) != testBytes {
		return result, false, fmt.Errorf("invalid G2D offscreen self-test input")
	}
	if err := ctx.Err(); err != nil {
		return result, false, err
	}
	targetCopy := mapped[frameBytes : frameBytes+testBytes]
	targetBlend := mapped[frameBytes+testBytes : frameBytes+2*testBytes]
	if err := checkCommandRanges(b.opaqueCopy, targetCopy, nil); err != nil {
		return result, false, err
	}
	if err := checkCommandRanges(b.opaqueBlue, targetBlend, b.halfRed); err != nil {
		return result, false, err
	}
	clear(targetCopy)
	clear(targetBlend)
	copySources := new([2]image)
	copySources[0] = testImage(b.opaqueCopy, 0, blendSource)
	copyTask := &task{Sources: uintptr(unsafe.Pointer(&copySources[0])),
		Target: testImage(targetCopy, 0, 0), NumSources: 1}
	started := time.Now()
	if err := process(copyTask, copySources); err != nil {
		return result, true, fmt.Errorf("G2D source-copy self-test: %w", err)
	}
	result.CopyUS = time.Since(started).Microseconds()
	if err := completeTask(copyTask, testBytes); err != nil {
		return result, true, err
	}
	runtime.KeepAlive(copySources)
	result.CopyOK = checkSelfTestColor(&result.CopyRGB, targetCopy, true)
	if !result.CopyOK {
		return result, false, fmt.Errorf("G2D ARGB32 source copy changed pixels: %x", result.CopyRGB)
	}
	if err := ctx.Err(); err != nil {
		return result, false, err
	}
	blendSources := new([2]image)
	blendSources[0] = testImage(b.opaqueBlue, 0, blendSource)
	blendSources[1] = testImage(b.halfRed, premultAlpha, blendSourceOver)
	blendSources[1].Extra.Alpha = 255
	blendTask := &task{Sources: uintptr(unsafe.Pointer(&blendSources[0])),
		Target: testImage(targetBlend, 0, 0), NumSources: 2}
	started = time.Now()
	if err := process(blendTask, blendSources); err != nil {
		return result, true, fmt.Errorf("G2D SRCOVER self-test: %w", err)
	}
	result.BlendUS = time.Since(started).Microseconds()
	if err := completeTask(blendTask, testBytes); err != nil {
		return result, true, err
	}
	runtime.KeepAlive(blendSources)
	result.BlendOK = checkSelfTestColor(&result.BlendRGB, targetBlend, false)
	if !result.BlendOK {
		return result, false, fmt.Errorf("G2D premultiplied SRCOVER output mismatch: %x", result.BlendRGB)
	}
	return result, false, ctx.Err()
}

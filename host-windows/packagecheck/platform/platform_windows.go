//go:build windows && (amd64 || arm64)

package platform

import (
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"s7.local/packagecheck/audit"
	"strings"
	"syscall"
	"unsafe"
)

// Load only system DLLs by absolute path; never search the package/current directory.
func systemDLL(name string) *syscall.LazyDLL {
	// kernel32 is a Windows KnownDLL. Resolve the system directory from the API.
	k := syscall.NewLazyDLL("kernel32.dll")
	buffer := make([]uint16, 32768)
	n, _, _ := k.NewProc("GetSystemDirectoryW").Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	if n == 0 || n >= uintptr(len(buffer)) {
		panic("GetSystemDirectoryW failed")
	}
	return syscall.NewLazyDLL(syscall.UTF16ToString(buffer[:n]) + `\` + name)
}

var trustDLL = systemDLL("wintrust.dll")
var verify = trustDLL.NewProc("WinVerifyTrust")
var acquire = trustDLL.NewProc("CryptCATAdminAcquireContext2")
var release = trustDLL.NewProc("CryptCATAdminReleaseContext")
var calc = trustDLL.NewProc("CryptCATAdminCalcHashFromFileHandle2")
var kernel = systemDLL("kernel32.dll")
var wow64 = kernel.NewProc("IsWow64Process2")
var version = systemDLL("ntdll.dll").NewProc("RtlGetVersion")

type guid struct {
	A    uint32
	B, C uint16
	D    [8]byte
}

var generic = guid{0x00aac56b, 0xcd44, 0x11d0, [8]byte{0x8c, 0xc2, 0x00, 0xc0, 0x4f, 0xc2, 0x95, 0xee}}
var driver = guid{0xf750e6c3, 0x38ee, 0x11d1, [8]byte{0x85, 0xe5, 0x00, 0xc0, 0x4f, 0xc2, 0x95, 0xee}}

type trustData struct {
	Size                   uint32
	Policy, SIP            uintptr
	UI, Revocation, Choice uint32
	Info                   unsafe.Pointer
	StateAction            uint32
	State                  uintptr
	URL                    *uint16
	Flags, Context         uint32
	Settings               uintptr
}
type fileInfo struct {
	Size    uint32
	Path    *uint16
	File    uintptr
	Subject uintptr
}
type catInfo struct {
	Size, Version      uint32
	Catalog, Tag, Path *uint16
	File               uintptr
	Hash               *byte
	HashLength         uint32
	Context, Admin     uintptr
}

// Win64 ABI, checked at compile time for both target architectures.
var _ [88 - unsafe.Sizeof(trustData{})]byte
var _ [unsafe.Sizeof(trustData{}) - 88]byte
var _ [32 - unsafe.Sizeof(fileInfo{})]byte
var _ [unsafe.Sizeof(fileInfo{}) - 32]byte
var _ [72 - unsafe.Sizeof(catInfo{})]byte
var _ [unsafe.Sizeof(catInfo{}) - 72]byte

func Environment() (audit.Environment, error) {
	var e audit.Environment
	if err := wow64.Find(); err != nil {
		return e, err
	}
	if err := version.Find(); err != nil {
		return e, err
	}
	var process, native uint16
	r, _, err := wow64.Call(^uintptr(0), uintptr(unsafe.Pointer(&process)), uintptr(unsafe.Pointer(&native)))
	if r == 0 {
		return e, fmt.Errorf("IsWow64Process2: %w", err)
	}
	switch native {
	case 0x8664:
		e.Architecture = "x64"
	case 0xaa64:
		e.Architecture = "arm64"
	default:
		return e, fmt.Errorf("unsupported native machine 0x%x", native)
	}
	v := struct {
		Size, Major, Minor, Build, Platform uint32
		ServicePack                         [128]uint16
	}{}
	v.Size = uint32(unsafe.Sizeof(v))
	r, _, _ = version.Call(uintptr(unsafe.Pointer(&v)))
	if uint32(r) != 0 || v.Major != 10 {
		return e, fmt.Errorf("RtlGetVersion failed/unsupported: 0x%x", r)
	}
	e.Build = v.Build
	return e, nil
}

type nativeTrust struct{}

func Trust() audit.Trust { return nativeTrust{} }
func runTrust(choice uint32, info unsafe.Pointer) error {
	if err := verify.Find(); err != nil {
		return err
	}
	d := trustData{UI: 2, Revocation: 1, Choice: choice, Info: info, StateAction: 1, Flags: 0x80 | 0x2000}
	d.Size = uint32(unsafe.Sizeof(d))
	r, _, _ := verify.Call(0, uintptr(unsafe.Pointer(&generic)), uintptr(unsafe.Pointer(&d)))
	// CLOSE must run even on failed verification, to release provider state.
	d.StateAction = 2
	verify.Call(0, uintptr(unsafe.Pointer(&generic)), uintptr(unsafe.Pointer(&d)))
	runtime.KeepAlive(info)
	if uint32(r) != 0 {
		return fmt.Errorf("WinVerifyTrust HRESULT=0x%08x", uint32(r))
	}
	return nil
}
func (nativeTrust) File(path string) error {
	p, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		return e
	}
	f := fileInfo{Path: p}
	f.Size = uint32(unsafe.Sizeof(f))
	e = runTrust(1, unsafe.Pointer(&f))
	runtime.KeepAlive(p)
	return e
}
func (nativeTrust) Member(catalog, path string) error {
	for _, p := range []*syscall.LazyProc{acquire, release, calc} {
		if e := p.Find(); e != nil {
			return e
		}
	}
	algorithm, _ := syscall.UTF16PtrFromString("SHA256")
	var admin uintptr
	ok, _, e := acquire.Call(uintptr(unsafe.Pointer(&admin)), uintptr(unsafe.Pointer(&driver)), uintptr(unsafe.Pointer(algorithm)), 0, 0)
	if ok == 0 {
		return fmt.Errorf("CryptCATAdminAcquireContext2: %w", e)
	}
	defer release.Call(admin, 0)
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	var size uint32
	ok, _, e = calc.Call(admin, f.Fd(), uintptr(unsafe.Pointer(&size)), 0, 0)
	if ok == 0 || size != 32 {
		return fmt.Errorf("SHA256 catalog size rejected: size=%d, %v", size, e)
	}
	hash := make([]byte, size)
	ok, _, e = calc.Call(admin, f.Fd(), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&hash[0])), 0)
	if ok == 0 || size != 32 {
		return fmt.Errorf("catalog hash failed: %v", e)
	}
	cp, e := syscall.UTF16PtrFromString(catalog)
	if e != nil {
		return e
	}
	mp, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		return e
	}
	tag, _ := syscall.UTF16PtrFromString(strings.ToUpper(hex.EncodeToString(hash)))
	c := catInfo{Catalog: cp, Tag: tag, Path: mp, File: f.Fd(), Hash: &hash[0], HashLength: size, Admin: admin}
	c.Size = uint32(unsafe.Sizeof(c))
	e = runTrust(2, unsafe.Pointer(&c))
	runtime.KeepAlive(hash)
	runtime.KeepAlive(cp)
	runtime.KeepAlive(mp)
	runtime.KeepAlive(tag)
	return e
}

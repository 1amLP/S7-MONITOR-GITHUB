// Package audit checks a Windows delivery without installing or executing its files.
package audit

import (
	"bytes"
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const Schema = "S7_WINDOWS_CANDIDATE_1"
const BackendSchema = "S7_WINDOWS_CANDIDATE_2"
const MaxFileBytes int64 = 256 << 20
const MaxManifestBytes = 64 << 10

var Payload = []string{"monitor/S7Monitor.inf", "monitor/S7Monitor.cat", "monitor/S7Monitor.dll", "camera/S7Camera.dll", "camera/S7CameraManage.exe", "diagnostics/camera-fps.exe"}

var LegacyPayload = []string{"monitor/S7Monitor.inf", "monitor/S7Monitor.cat", "monitor/S7Monitor.dll", "camera/S7CameraDevice.dll", "camera/S7CameraLegacy.dll", "camera/S7CameraLegacy.inf", "camera/S7CameraLegacy.cat", "diagnostics/camera-fps.exe"}

// Exactly one camera backend per immutable package, including monitor-only checks.
func (m Manifest) PayloadPaths() []string {
	if m.CameraBackend == "legacy" {
		return append([]string(nil), LegacyPayload...)
	}
	return append([]string(nil), Payload...)
}

type Entry struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}
type Manifest struct {
	Schema        string  `json:"schema"`
	CameraBackend string  `json:"camera_backend,omitempty"`
	Architecture  string  `json:"architecture"`
	Files         []Entry `json:"files"`
}
type Environment struct {
	Architecture string `json:"architecture"`
	Build        uint32 `json:"windows_build"`
	Component    string `json:"component"`
	Static       bool   `json:"static_only"`
}
type Result struct {
	Schema             string      `json:"schema"`
	Environment        Environment `json:"environment"`
	Passed             bool        `json:"preflight_pass"`
	TrustChecked       bool        `json:"trust_checked"`
	FilesChecked       int         `json:"files_checked"`
	CameraBackend      string      `json:"camera_backend"`
	Issues             []string    `json:"issues"`
	Installed          bool        `json:"installed"`
	DriverLoadApproved bool        `json:"driver_load_approved"`
	HardwareTested     bool        `json:"hardware_tested"`
}

// Trust verifies embedded Authenticode or membership in the explicitly supplied CAT.
// It does not assert Microsoft driver-signing approval or device compatibility.
type Trust interface {
	File(string) error
	Member(catalog, member string) error
}

func uniqueJSON(d *json.Decoder) error {
	t, e := d.Token()
	if e != nil {
		return e
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return e
			}
			k, ok := key.(string)
			if !ok || seen[strings.ToLower(k)] {
				return errors.New("duplicate JSON key")
			}
			seen[strings.ToLower(k)] = true
			if e = uniqueJSON(d); e != nil {
				return e
			}
		}
		_, e = d.Token()
		return e
	case '[':
		for d.More() {
			if e = uniqueJSON(d); e != nil {
				return e
			}
		}
		_, e = d.Token()
		return e
	default:
		return errors.New("unexpected JSON delimiter")
	}
}
func Parse(data []byte) (Manifest, error) {
	var m Manifest
	if len(data) > MaxManifestBytes {
		return m, errors.New("manifest too large")
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}) // PowerShell 5.1 UTF-8 BOM.
	d := json.NewDecoder(bytes.NewReader(data))
	if e := uniqueJSON(d); e != nil {
		return m, e
	}
	if _, e := d.Token(); e != io.EOF {
		return m, errors.New("trailing JSON data")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e := d.Decode(&m); e != nil {
		return m, e
	}
	if (m.Schema != Schema && m.Schema != BackendSchema) || (m.Architecture != "x64" && m.Architecture != "arm64") {
		return m, errors.New("unsupported manifest schema/architecture")
	}
	if m.Schema == Schema {
		if m.CameraBackend != "" {
			return m, errors.New("v1 cannot select a camera backend")
		}
		m.CameraBackend = "modern"
	} else if m.CameraBackend != "modern" && m.CameraBackend != "legacy" {
		return m, errors.New("v2 requires exactly one camera backend")
	}
	payload := m.PayloadPaths()
	if len(m.Files) != len(payload) {
		return m, errors.New("incomplete payload")
	}
	allowed := map[string]bool{}
	for _, p := range payload {
		allowed[p] = true
	}
	for _, v := range m.Files {
		if !allowed[v.Path] {
			return m, fmt.Errorf("unexpected or duplicate path: %q", v.Path)
		}
		delete(allowed, v.Path)
		h, e := hex.DecodeString(v.SHA256)
		if e != nil || len(h) != 32 || strings.ToLower(v.SHA256) != v.SHA256 || v.Bytes <= 0 || v.Bytes > MaxFileBytes {
			return m, fmt.Errorf("invalid length/hash: %s", v.Path)
		}
	}
	return m, nil
}
func noLinks(root, path string) error {
	rel, e := filepath.Rel(root, path)
	if e != nil {
		return e
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("outside package")
	}
	p := root
	parts := []string{""}
	if rel != "." {
		parts = append(parts, strings.Split(rel, string(filepath.Separator))...)
	}
	for _, part := range parts {
		p = filepath.Join(p, part)
		s, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if s.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return fmt.Errorf("link/nonregular path: %s", p)
		}
	}
	return nil
}
func readBounded(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !s.Mode().IsRegular() || s.Size() > limit {
		return nil, errors.New("nonregular or oversized file")
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("file grew beyond limit")
	}
	return b, e
}
func machine(arch string) uint16 {
	if arch == "arm64" {
		return pe.IMAGE_FILE_MACHINE_ARM64
	}
	return pe.IMAGE_FILE_MACHINE_AMD64
}
func checkPE(path, arch string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return e
	}
	var dos [64]byte
	if _, e = f.ReadAt(dos[:], 0); e != nil {
		return e
	}
	if dos[0] != 'M' || dos[1] != 'Z' {
		return errors.New("DOS signature required")
	}
	offset := int64(binary.LittleEndian.Uint32(dos[60:]))
	if offset < 64 || offset > st.Size()-96 {
		return errors.New("PE header extent invalid")
	}
	var h [96]byte
	if _, e = f.ReadAt(h[:], offset); e != nil {
		return e
	}
	if string(h[:4]) != "PE\x00\x00" {
		return errors.New("PE signature required")
	}
	if binary.LittleEndian.Uint16(h[4:]) != machine(arch) {
		return fmt.Errorf("PE machine does not match %s", arch)
	}
	optSize := int64(binary.LittleEndian.Uint16(h[20:]))
	sections := int64(binary.LittleEndian.Uint16(h[6:]))
	if optSize < 112 || optSize > 4096 || sections > 96 || offset+24+optSize+sections*40 > st.Size() {
		return errors.New("PE optional/section headers out of bounds")
	}
	if binary.LittleEndian.Uint16(h[24:]) != 0x20b {
		return errors.New("PE32+ required")
	}
	if binary.LittleEndian.Uint16(h[94:])&0x140 != 0x140 {
		return errors.New("ASLR and DEP required")
	}
	if (binary.LittleEndian.Uint16(h[22:])&pe.IMAGE_FILE_DLL != 0) != (filepath.Ext(path) == ".dll") {
		return errors.New("PE DLL/executable role mismatch")
	}
	return nil
}

func Verify(root string, env Environment, trust Trust) Result {
	r := Result{Schema: "S7_WINDOWS_PREFLIGHT_1", Environment: env, Issues: []string{}}
	issue := func(e error) Result { r.Issues = append(r.Issues, e.Error()); return r }
	if env.Architecture != "x64" && env.Architecture != "arm64" {
		return issue(errors.New("native Windows architecture must be x64 or arm64"))
	}
	if env.Component != "all" && env.Component != "monitor" && env.Component != "camera" {
		return issue(errors.New("component must be all, monitor or camera"))
	}
	if env.Build < 19041 {
		return issue(errors.New("monitor source target requires Windows build 19041+"))
	}
	root, e := filepath.Abs(root)
	if e != nil {
		return issue(e)
	}
	manifestPath := filepath.Join(root, "package.json")
	if e = noLinks(root, manifestPath); e != nil {
		return issue(e)
	}
	data, e := readBounded(manifestPath, MaxManifestBytes)
	if e != nil {
		return issue(e)
	}
	m, e := Parse(data)
	if e != nil {
		return issue(e)
	}
	r.CameraBackend = m.CameraBackend
	if env.Component != "monitor" && m.CameraBackend == "modern" && env.Build < 22000 {
		return issue(errors.New("modern camera requires Windows build 22000+; provide an explicit legacy package for Windows 10"))
	}
	if m.Architecture != env.Architecture {
		return issue(errors.New("package/native OS architecture mismatch; emulation is not a driver fallback"))
	}
	allowed := map[string]bool{"package.json": true}
	for _, v := range m.Files {
		allowed[v.Path] = true
		p := filepath.Join(root, filepath.FromSlash(v.Path))
		if e = noLinks(root, p); e != nil {
			return issue(e)
		}
		f, e := os.Open(p)
		if e != nil {
			return issue(e)
		}
		s, e := f.Stat()
		if e != nil {
			f.Close()
			return issue(e)
		}
		if !s.Mode().IsRegular() || s.Size() != v.Bytes {
			f.Close()
			return issue(fmt.Errorf("length/regular-file mismatch: %s", v.Path))
		}
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(f, v.Bytes+1))
		f.Close()
		if e != nil || n != v.Bytes || hex.EncodeToString(h.Sum(nil)) != v.SHA256 {
			return issue(fmt.Errorf("SHA256/length mismatch: %s", v.Path))
		}
		if strings.HasSuffix(p, ".dll") || strings.HasSuffix(p, ".exe") {
			if e = checkPE(p, m.Architecture); e != nil {
				return issue(fmt.Errorf("%s: %w", v.Path, e))
			}
		}
		if v.Path == "monitor/S7Monitor.inf" {
			if e = checkINF(p, m.Architecture); e != nil {
				return issue(e)
			}
		}
		if v.Path == "camera/S7CameraLegacy.inf" {
			if e = checkLegacyINF(p, m.Architecture); e != nil {
				return issue(e)
			}
		}
		r.FilesChecked++
	}
	e = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return errors.New("unlisted link/nonregular file")
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		if !allowed[filepath.ToSlash(rel)] {
			return fmt.Errorf("unlisted file: %s", rel)
		}
		return nil
	})
	if e != nil {
		return issue(e)
	}
	if env.Static {
		r.Passed = true
		return r
	}
	if trust == nil {
		return issue(errors.New("Windows trust verifier unavailable; no success fallback"))
	}
	if env.Component != "camera" {
		cat := filepath.Join(root, "monitor/S7Monitor.cat")
		if e = trust.File(cat); e != nil {
			return issue(fmt.Errorf("catalog trust: %w", e))
		}
		for _, p := range []string{"monitor/S7Monitor.inf", "monitor/S7Monitor.dll"} {
			if e = trust.Member(cat, filepath.Join(root, p)); e != nil {
				return issue(fmt.Errorf("catalog membership %s: %w", p, e))
			}
		}
	}
	if env.Component != "monitor" {
		signed := []string{"camera/S7Camera.dll", "camera/S7CameraManage.exe", "diagnostics/camera-fps.exe"}
		if m.CameraBackend == "legacy" {
			cat := filepath.Join(root, "camera/S7CameraLegacy.cat")
			if e = trust.File(cat); e != nil {
				return issue(fmt.Errorf("camera catalog trust: %w", e))
			}
			for _, p := range []string{"camera/S7CameraLegacy.inf", "camera/S7CameraDevice.dll", "camera/S7CameraLegacy.dll"} {
				if e = trust.Member(cat, filepath.Join(root, p)); e != nil {
					return issue(fmt.Errorf("camera catalog membership %s: %w", p, e))
				}
			}
			signed = []string{"camera/S7CameraDevice.dll", "camera/S7CameraLegacy.dll", "diagnostics/camera-fps.exe"}
		}
		for _, p := range signed {
			if e = trust.File(filepath.Join(root, p)); e != nil {
				return issue(fmt.Errorf("signature %s: %w", p, e))
			}
		}
	}
	// Rehash after potentially slow trust calls; a preflight is not an install lock.
	again := env
	again.Static = true
	if check := Verify(root, again, nil); !check.Passed {
		return issue(errors.New("package changed during trust verification"))
	}
	after, e := readBounded(manifestPath, MaxManifestBytes)
	if e != nil || !bytes.Equal(data, after) {
		return issue(errors.New("manifest changed during verification"))
	}
	r.TrustChecked = true
	r.Passed = true
	return r
}

func checkINF(path, arch string) error {
	return checkDriverINF(path, arch, "s7monitor.cat", map[string]bool{"%devicename%=install,root\\s7h264monitor": true})
}
func checkLegacyINF(path, arch string) error {
	return checkDriverINF(path, arch, "s7cameralegacy.cat", map[string]bool{"%rearname%=rear,root\\s7rearcamera10": true, "%frontname%=front,root\\s7frontcamera10": true})
}
func checkDriverINF(path, arch, catalog string, expected map[string]bool) error {
	seen := map[string]bool{}
	b, e := readBounded(path, 128<<10)
	if e != nil {
		return e
	}
	b = bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})
	// This project ships UTF-8/ASCII INFs, not a lossy ANSI/UTF-16 conversion.
	if bytes.IndexByte(b, 0) >= 0 {
		return errors.New("unexpected INF encoding")
	}
	section := ""
	models := 0
	catalogs := 0
	signatures := 0
	manufacturer := 0
	decoration := "ntamd64.10.0...19041"
	if arch == "arm64" {
		decoration = "ntarm64.10.0...19041"
	}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(strings.SplitN(raw, ";", 2)[0])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(line[1 : len(line)-1])
			continue
		}
		lower := strings.ToLower(strings.ReplaceAll(line, " ", ""))
		if section == "version" && strings.HasPrefix(lower, "catalogfile") {
			catalogs++
			if lower != "catalogfile="+catalog {
				return errors.New("unexpected driver catalog")
			}
		}
		if section == "version" && strings.HasPrefix(lower, "signature=") {
			signatures++
			if lower != "signature=\"$windowsnt$\"" {
				return errors.New("invalid driver INF signature marker")
			}
		}
		if section == "manufacturer" {
			manufacturer++
			if lower != "%provider%=models,"+decoration {
				return errors.New("INF manufacturer architecture mismatch")
			}
		}
		if strings.HasPrefix(section, "models") {
			models++
			if section != "models."+decoration || !expected[lower] || seen[lower] {
				return errors.New("unexpected INF model/architecture/hardware ID")
			}
			seen[lower] = true
		}
	}
	if catalogs != 1 || models != len(expected) || manufacturer != 1 || signatures != 1 {
		return errors.New("missing or duplicate INF identity")
	}
	return nil
}

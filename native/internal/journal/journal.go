// Package journal stores bounded first-fault diagnostics in an explicitly opted-in
// directory. It does not discover, mount, repair or format any device.
package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const maxFile = 32768
const consentName = "diagnostics-consent"

type Event struct {
	BootID    string          `json:"boot_id"`
	Subsystem string          `json:"subsystem"`
	Kind      string          `json:"kind"`
	Message   string          `json:"message"`
	AtUnixMS  int64           `json:"at_unix_ms"`
	Runtime   json.RawMessage `json:"runtime,omitempty"`
}
type Record struct {
	Version int    `json:"version"`
	Device  string `json:"device"`
	Event   Event  `json:"event"`
}
type marker struct {
	Version int    `json:"version"`
	Device  string `json:"device"`
	BootID  string `json:"boot_id"`
	Clean   bool   `json:"clean_shutdown"`
}
type envelope struct {
	Payload json.RawMessage `json:"payload"`
	SHA256  string          `json:"sha256"`
}
type Store struct {
	directory, device, bootID string
	first                     *Record
	write                     func(string, []byte) error
}

func ident(s string) bool {
	if len(s) < 1 || len(s) > 80 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func checkDir(d string) error {
	if !filepath.IsAbs(d) || filepath.Clean(d) != d {
		return fmt.Errorf("journal directory must be canonical absolute path")
	}
	st, e := os.Lstat(d)
	if e != nil {
		return e
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("unsafe journal directory")
	}
	return nil
}
func checkedRead(path string) ([]byte, error) {
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() || st.Size() < 1 || st.Size() > maxFile {
		return nil, fmt.Errorf("unsafe/oversized journal record")
	}
	return io.ReadAll(io.LimitReader(f, maxFile+1))
}
func pack(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(b)
	b, e = json.Marshal(envelope{b, hex.EncodeToString(sum[:])})
	if len(b) > maxFile {
		return nil, fmt.Errorf("journal record exceeds bound")
	}
	return b, e
}
func unpack(b []byte, v any) error {
	if len(b) > maxFile {
		return fmt.Errorf("journal record too large")
	}
	var p envelope
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(&p); e != nil {
		return e
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("trailing journal data")
	}
	hash := sha256.Sum256(p.Payload)
	if p.SHA256 != hex.EncodeToString(hash[:]) {
		return fmt.Errorf("journal checksum mismatch")
	}
	d = json.NewDecoder(bytes.NewReader(p.Payload))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
func syncDirectory(dir string) error {
	f, e := os.Open(dir)
	if e != nil {
		return e
	}
	return errors.Join(f.Sync(), f.Close())
}
func atomicWrite(path string, b []byte) error {
	dir := filepath.Dir(path)
	if e := checkDir(dir); e != nil {
		return e
	}
	if st, e := os.Lstat(path); e == nil && !st.Mode().IsRegular() {
		return fmt.Errorf("unsafe journal destination")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	// A fixed owned temporary name bounds disk usage after repeated power loss.
	// Never follow a symlink or truncate an unrelated non-regular destination.
	temp := path + ".pending"
	if st, e := os.Lstat(temp); e == nil {
		if !st.Mode().IsRegular() || st.Size() > maxFile {
			return fmt.Errorf("unsafe pending record")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	fd, e := syscall.Open(temp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return e
	}
	f := os.NewFile(uintptr(fd), temp)
	defer os.Remove(temp)
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(temp, path); e != nil {
		return e
	}
	return syncDirectory(dir)
}
func consent(device string) []byte { return []byte("S7-NATIVE-DIAGNOSTICS-1\n" + device + "\n") }

// Open requires a diagnostic-specific consent; old settings-only consent does
// not authorize collecting logs. explicit is set by the confirmation menu only.
func Open(dir, device, bootID string, explicit bool) (*Store, error) {
	if !ident(device) || !ident(bootID) {
		return nil, fmt.Errorf("invalid journal device/boot identity")
	}
	if e := checkDir(dir); e != nil {
		return nil, e
	}
	s := &Store{directory: dir, device: device, bootID: bootID, write: atomicWrite}
	path := filepath.Join(dir, consentName)
	if explicit {
		if e := s.write(path, consent(device)); e != nil {
			return nil, e
		}
	} else {
		b, e := checkedRead(path)
		if e != nil {
			return nil, e
		}
		if !bytes.Equal(b, consent(device)) {
			return nil, fmt.Errorf("journal has no consent for this device")
		}
	}
	return s, nil
}
func (s *Store) path(name string) string { return filepath.Join(s.directory, name) }
func (s *Store) writeValue(name string, v any) error {
	if e := checkDir(s.directory); e != nil {
		return e
	}
	b, e := pack(v)
	if e != nil {
		return e
	}
	return s.write(s.path(name), b)
}
func (s *Store) validate(r Record) error {
	if r.Version != 1 || r.Device != s.device || !ident(r.Event.BootID) || r.Event.Kind == "" || r.Event.Subsystem == "" || len(r.Event.Message) > 4096 || len(r.Event.Runtime) > 16384 {
		return fmt.Errorf("invalid first-fault record")
	}
	return nil
}

// Begin recovers either independently checksummed copy. An unfinished previous
// boot is reported as unknown, never invented as a camera/thermal root cause.
func (s *Store) loadFirst() (*Record, error) {
	var valid []*Record
	var bad error
	for _, name := range []string{"first-fault-a.json", "first-fault-b.json", "first-fault-a.json.pending", "first-fault-b.json.pending"} {
		b, e := checkedRead(s.path(name))
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		var r Record
		if e == nil {
			e = unpack(b, &r)
		}
		if e == nil {
			e = s.validate(r)
		}
		if e != nil {
			bad = errors.Join(bad, e)
			continue
		}
		valid = append(valid, &r)
	}
	if len(valid) > 1 {
		a, _ := json.Marshal(valid[0])
		for _, other := range valid[1:] {
			b, _ := json.Marshal(other)
			if !bytes.Equal(a, b) {
				return nil, fmt.Errorf("conflicting first-fault copies; explicit clear required")
			}
		}
	}
	if len(valid) > 0 {
		return valid[0], nil
	} else if bad != nil {
		return nil, fmt.Errorf("first-fault copies corrupt; explicit clear required: %w", bad)
	}
	return nil, nil
}
func (s *Store) Begin(now time.Time) error {
	first, e := s.loadFirst()
	if e != nil {
		return e
	}
	s.first = first
	b, e := checkedRead(s.path("boot-marker.json"))
	if e == nil {
		var m marker
		if e = unpack(b, &m); e != nil {
			return fmt.Errorf("boot marker corrupt: %w", e)
		}
		if m.Version != 1 || m.Device != s.device || !ident(m.BootID) {
			return fmt.Errorf("boot marker identity mismatch")
		}
		if !m.Clean && m.BootID != s.bootID && s.first == nil {
			_, e = s.RecordFirst(Event{BootID: m.BootID, Subsystem: "system", Kind: "unclean-exit-unknown", Message: "Previous boot did not record an orderly shutdown; reset, power loss or a blocked shutdown is possible. Root cause unknown.", AtUnixMS: now.UnixMilli()})
			if e != nil {
				return e
			}
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	return s.writeValue("boot-marker.json", marker{1, s.device, s.bootID, false})
}
func (s *Store) RecordFirst(e Event) (bool, error) {
	if s.first != nil {
		return false, nil
	}
	if e.BootID == "" {
		e.BootID = s.bootID
	}
	e.Message = strings.TrimSpace(e.Message)
	if len(e.Message) > 4096 {
		e.Message = e.Message[:4096]
	}
	r := Record{1, s.device, e}
	if err := s.validate(r); err != nil {
		return false, err
	}
	if err := s.writeValue("first-fault-a.json", r); err != nil {
		return false, err
	}
	// Once copy A is durable, never overwrite it with a later failure, even if B
	// could not be written. Begin can recover A after interruption at this point.
	s.first = &r
	return true, s.writeValue("first-fault-b.json", r)
}
func (s *Store) First() *Record {
	if s.first == nil {
		return nil
	}
	r := *s.first
	r.Event.Runtime = append(json.RawMessage(nil), r.Event.Runtime...)
	return &r
}
func (s *Store) Finish() error {
	return s.writeValue("boot-marker.json", marker{1, s.device, s.bootID, true})
}

// Clear needs a distinct explicit UI confirmation. It removes only fixed owned
// file names and syncs the directory; no recursive deletion or partition writes.
func (s *Store) Clear() error {
	if e := checkDir(s.directory); e != nil {
		return e
	}
	for _, n := range []string{"first-fault-a.json", "first-fault-b.json", "boot-marker.json", "first-fault-a.json.pending", "first-fault-b.json.pending", "boot-marker.json.pending"} {
		st, e := os.Lstat(s.path(n))
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("unsafe record to clear")
		}
		if e = os.Remove(s.path(n)); e != nil {
			return e
		}
	}
	if e := syncDirectory(s.directory); e != nil {
		return e
	}
	s.first = nil
	return nil
}

// Inspect reads existing diagnostics without creating a marker or a file. It is
// suitable for a read-only noload mount after an unclean reset. Missing/torn data
// is an explicit error, not permission to replay a filesystem journal.
func Inspect(dir, device string) (*Record, error) {
	s, e := Open(dir, device, "read-only", false)
	if e != nil {
		return nil, e
	}
	r, e := s.loadFirst()
	if e != nil || r != nil {
		return r, e
	}
	b, e := checkedRead(s.path("boot-marker.json"))
	if e != nil {
		return nil, e
	}
	var m marker
	if e = unpack(b, &m); e != nil {
		return nil, e
	}
	if m.Version != 1 || m.Device != device || !ident(m.BootID) {
		return nil, fmt.Errorf("read-only marker identity mismatch")
	}
	if !m.Clean {
		return &Record{1, device, Event{BootID: m.BootID, Subsystem: "system", Kind: "unclean-exit-unknown", Message: "Read-only recovery: previous boot has no clean shutdown marker. Root cause unknown."}}, nil
	}
	return nil, nil
}

// Disable revokes logging consent. Existing evidence is kept until a separate
// explicit Clear; settings and all other files remain untouched.
func (s *Store) Disable() error {
	if e := checkDir(s.directory); e != nil {
		return e
	}
	st, e := os.Lstat(s.path(consentName))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("unsafe consent file")
	}
	if e = os.Remove(s.path(consentName)); e != nil {
		return e
	}
	return syncDirectory(s.directory)
}

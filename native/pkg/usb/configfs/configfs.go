// Package configfs manages only USB gadget objects created by the caller.
package configfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const DefaultGadgetsBasePath = "/sys/kernel/config/usb_gadget"

type fileOps interface {
	Mkdir(string, os.FileMode) error
	Remove(string) error
	Symlink(string, string) error
	ReadFile(string) ([]byte, error)
	WriteAttr(string, []byte) error
}

type systemFiles struct{}

func (systemFiles) Mkdir(p string, mode os.FileMode) error { return os.Mkdir(p, mode) }
func (systemFiles) Remove(p string) error                  { return os.Remove(p) }
func (systemFiles) Symlink(target, p string) error         { return os.Symlink(target, p) }
func (systemFiles) ReadFile(p string) ([]byte, error)      { return os.ReadFile(p) }
func (systemFiles) WriteAttr(p string, data []byte) error {
	// ConfigFS creates attributes itself. Never create an ordinary file on a typo.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return errors.Join(err, f.Close())
}

type Gadget struct {
	Path     string
	files    fileOps
	owned    bool
	closed   bool
	boundUDC string
	created  []string
}

func NewGadget(name string) *Gadget {
	return &Gadget{Path: filepath.Join(DefaultGadgetsBasePath, name)}
}

func (g *Gadget) fs() fileOps {
	if g.files == nil {
		g.files = systemFiles{}
	}
	return g.files
}

func (g *Gadget) Exists() bool {
	_, err := g.fs().ReadFile(filepath.Join(g.Path, "UDC"))
	return err == nil
}

func (g *Gadget) Create() error {
	if g.owned || g.closed || !filepath.IsAbs(g.Path) {
		return fmt.Errorf("invalid or already used gadget path %q", g.Path)
	}
	if err := g.fs().Mkdir(g.Path, 0755); err != nil {
		return err
	}
	g.owned = true
	g.created = append(g.created, g.Path)
	return nil
}

func (g *Gadget) requireOwner() error {
	if !g.owned || g.closed || len(g.created) == 0 || g.created[0] != g.Path {
		return errors.New("refusing to change a gadget not created by this instance")
	}
	return nil
}

// Delete unbinds first, then removes exact owned objects in reverse order.
// Kernel-provided attribute directories must never be recursively deleted.
func (g *Gadget) Delete() error {
	if g.closed {
		return nil
	}
	if err := g.requireOwner(); err != nil {
		return err
	}
	current, err := g.GetUDC()
	if err != nil {
		return err
	}
	if current != "" {
		if current != g.boundUDC {
			return fmt.Errorf("gadget was bound by another owner to %q; cleanup stopped", current)
		}
		if err := g.SetUDC(""); err != nil {
			return fmt.Errorf("unbind before cleanup: %w", err)
		}
	}
	for len(g.created) != 0 {
		last := len(g.created) - 1
		path := g.created[last]
		if err := g.fs().Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove owned object %s: %w", path, err)
		}
		g.created = g.created[:last]
	}
	g.closed, g.owned = true, false
	return nil
}

func validName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (g *Gadget) createObject(path string) error {
	if err := g.requireOwner(); err != nil {
		return err
	}
	relative, err := filepath.Rel(g.Path, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("object path leaves the owned gadget: %q", path)
	}
	if err := g.fs().Mkdir(path, 0755); err != nil {
		return err
	}
	g.created = append(g.created, path)
	return nil
}

func (g *Gadget) write(path, name string, data []byte) error {
	if err := g.requireOwner(); err != nil {
		return err
	}
	if !validName(name) {
		return fmt.Errorf("invalid attribute name %q", name)
	}
	if g.ownsObject(path) {
		return g.fs().WriteAttr(filepath.Join(path, name), data)
	}
	return fmt.Errorf("attribute path is not an owned object: %q", path)
}

func (g *Gadget) ownsObject(path string) bool {
	for _, owned := range g.created {
		if path == owned {
			return true
		}
	}
	return false
}

func (g *Gadget) writeAttr(name, value string) error {
	return g.write(g.Path, name, []byte(value+"\n"))
}

func (g *Gadget) writeUintHex(name string, value uint64, bits int) error {
	format := "0x%0" + strconv.Itoa(bits/4) + "x"
	return g.writeAttr(name, fmt.Sprintf(format, value))
}

func (g *Gadget) SetVendor(id uint16) error  { return g.writeUintHex("idVendor", uint64(id), 16) }
func (g *Gadget) SetProduct(id uint16) error { return g.writeUintHex("idProduct", uint64(id), 16) }
func (g *Gadget) SetDevice(bcd uint16) error { return g.writeUintHex("bcdDevice", uint64(bcd), 16) }
func (g *Gadget) SetUSB(bcd uint16) error    { return g.writeUintHex("bcdUSB", uint64(bcd), 16) }
func (g *Gadget) SetClass(cls byte) error    { return g.writeUintHex("bDeviceClass", uint64(cls), 8) }
func (g *Gadget) SetSubClass(sub byte) error {
	return g.writeUintHex("bDeviceSubClass", uint64(sub), 8)
}
func (g *Gadget) SetProtocol(proto byte) error {
	return g.writeUintHex("bDeviceProtocol", uint64(proto), 8)
}

func (g *Gadget) SetUDC(name string) error {
	if err := g.requireOwner(); err != nil {
		return err
	}
	if name != "" && !validName(name) {
		return fmt.Errorf("invalid UDC name %q", name)
	}
	current, err := g.GetUDC()
	if err != nil {
		return err
	}
	if current != "" && (name != "" || current != g.boundUDC) {
		return fmt.Errorf("refusing to replace existing UDC binding %q", current)
	}
	if current == "" && name == "" {
		return nil
	}
	if err := g.writeAttr("UDC", name); err != nil {
		return err
	}
	g.boundUDC = name
	return nil
}

func (g *Gadget) GetUDC() (string, error) {
	data, err := g.fs().ReadFile(filepath.Join(g.Path, "UDC"))
	return strings.TrimSpace(string(data)), err
}

type Strings struct {
	Path   string
	gadget *Gadget
}

func (g *Gadget) Strings(lang uint16) *Strings {
	return &Strings{Path: filepath.Join(g.Path, "strings", fmt.Sprintf("0x%x", lang)), gadget: g}
}
func (s *Strings) Create() error { return s.gadget.createObject(s.Path) }
func (s *Strings) writeAttr(name, value string) error {
	return s.gadget.write(s.Path, name, []byte(value+"\n"))
}
func (s *Strings) SetManufacturer(value string) error { return s.writeAttr("manufacturer", value) }
func (s *Strings) SetProduct(value string) error      { return s.writeAttr("product", value) }
func (s *Strings) SetSerialNumber(value string) error { return s.writeAttr("serialnumber", value) }

type Config struct {
	Path   string
	Name   string
	gadget *Gadget
}

func (g *Gadget) Config(name string) *Config {
	return &Config{Path: filepath.Join(g.Path, "configs", name), Name: name, gadget: g}
}
func (c *Config) Create() error {
	if !validName(c.Name) {
		return fmt.Errorf("invalid configuration name %q", c.Name)
	}
	return c.gadget.createObject(c.Path)
}
func (c *Config) Strings(lang uint16) *Strings {
	return &Strings{Path: filepath.Join(c.Path, "strings", fmt.Sprintf("0x%x", lang)), gadget: c.gadget}
}
func (c *Config) SetMaxPower(milliamps int) error {
	if milliamps < 0 || milliamps > 500 {
		return fmt.Errorf("USB 2 maximum current out of range: %d", milliamps)
	}
	return c.gadget.write(c.Path, "MaxPower", []byte(fmt.Sprintf("%d\n", milliamps)))
}
func (c *Config) AddFunction(name string) error {
	if !validName(c.Name) || !validName(name) {
		return errors.New("invalid configuration or function name")
	}
	g := c.gadget
	if err := g.requireOwner(); err != nil {
		return err
	}
	target := filepath.Join(g.Path, "functions", name)
	link := filepath.Join(c.Path, name)
	if !g.ownsObject(c.Path) || !g.ownsObject(target) {
		return errors.New("function link requires an owned configuration and function")
	}
	if err := g.fs().Symlink(target, link); err != nil {
		return err
	}
	g.created = append(g.created, link)
	return nil
}

type Function struct {
	Path   string
	gadget *Gadget
}

func (g *Gadget) CreateFunction(name string) (*Function, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid function name %q", name)
	}
	path := filepath.Join(g.Path, "functions", name)
	if err := g.createObject(path); err != nil {
		return nil, err
	}
	return &Function{Path: path, gadget: g}, nil
}
func (f *Function) SetProperty(name, value string) error {
	return f.gadget.write(f.Path, name, []byte(value+"\n"))
}
func (f *Function) SetBinaryProperty(name string, value []byte) error {
	return f.gadget.write(f.Path, name, value)
}

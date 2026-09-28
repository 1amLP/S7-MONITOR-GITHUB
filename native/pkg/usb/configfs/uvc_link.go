package configfs

import (
	"errors"
	"path/filepath"
)

// AddUVCFunction links a function owned by the dedicated UVC builder. AddFunction
// only accepts objects created by Gadget itself; accepting an arbitrary existing
// path there would weaken its ownership rule. Keep the two owners explicit.
func (c *Config) AddUVCFunction(u *UVCFunction) error {
	g := c.gadget
	if e := g.requireOwner(); e != nil {
		return e
	}
	if u == nil || u.gadget != g.Path || u.Path != filepath.Join(g.Path, "functions", UVCFunctionName) ||
		len(u.created) == 0 || u.created[0] != u.Path || !g.ownsObject(c.Path) {
		return errors.New("UVC link requires a live function owned by this gadget")
	}
	if e := u.unbound(); e != nil {
		return e
	}
	link := filepath.Join(c.Path, UVCFunctionName)
	if e := g.fs().Symlink(u.Path, link); e != nil {
		return e
	}
	g.created = append(g.created, link)
	return nil
}

// Package guest builds the pieces of the small Linux guest Orchard boots in
// QEMU on hosts that can't run Apple's Android daemon themselves (macOS,
// Windows): an initramfs made of a base layer (busybox, kernel modules, an init
// script) plus the daemon's Android root filesystem.
package guest

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CPIO writes an archive in the "newc" format the Linux kernel unpacks as an
// initramfs. It is a handful of lines because the format is: a fixed ASCII
// header, the name, the data, each padded to four bytes.
type CPIO struct {
	w   io.Writer
	n   int64
	ino uint32
}

// NewCPIO returns a writer; call Close to write the trailer.
func NewCPIO(w io.Writer) *CPIO { return &CPIO{w: w} }

func (c *CPIO) write(b []byte) error {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return err
}

func (c *CPIO) pad() error {
	if r := c.n % 4; r != 0 {
		return c.write(make([]byte, 4-r))
	}
	return nil
}

func (c *CPIO) header(name string, mode uint32, size int64, rdevMajor, rdevMinor uint32) error {
	c.ino++
	nlink := uint32(1)
	if mode&uint32(fs.ModeDir) != 0 || mode&0o170000 == 0o040000 {
		nlink = 2
	}
	h := fmt.Sprintf("070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		c.ino, mode, 0, 0, nlink, 0, uint32(size), 0, 0, rdevMajor, rdevMinor, len(name)+1, 0)
	if err := c.write([]byte(h)); err != nil {
		return err
	}
	if err := c.write([]byte(name + "\x00")); err != nil {
		return err
	}
	return c.pad()
}

const (
	modeDir  = 0o040000
	modeFile = 0o100000
	modeLink = 0o120000
	modeChr  = 0o020000
)

// Dir adds a directory.
func (c *CPIO) Dir(name string, perm uint32) error {
	return c.header(clean(name), modeDir|perm, 0, 0, 0)
}

// File adds a regular file of exactly size bytes read from r.
func (c *CPIO) File(name string, perm uint32, size int64, r io.Reader) error {
	if err := c.header(clean(name), modeFile|perm, size, 0, 0); err != nil {
		return err
	}
	n, err := io.Copy(countWriter{c}, io.LimitReader(r, size))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("cpio: %s: wrote %d of %d bytes", name, n, size)
	}
	return c.pad()
}

// Bytes adds a regular file from memory.
func (c *CPIO) Bytes(name string, perm uint32, data []byte) error {
	return c.File(name, perm, int64(len(data)), strings.NewReader(string(data)))
}

// Symlink adds a symbolic link.
func (c *CPIO) Symlink(name, target string) error {
	if err := c.header(clean(name), modeLink|0o777, int64(len(target)), 0, 0); err != nil {
		return err
	}
	if err := c.write([]byte(target)); err != nil {
		return err
	}
	return c.pad()
}

// CharDev adds a character device node.
func (c *CPIO) CharDev(name string, perm, major, minor uint32) error {
	return c.header(clean(name), modeChr|perm, 0, major, minor)
}

// Close writes the trailer.
func (c *CPIO) Close() error {
	if err := c.header("TRAILER!!!", 0, 0, 0, 0); err != nil {
		return err
	}
	return c.pad()
}

type countWriter struct{ c *CPIO }

func (w countWriter) Write(p []byte) (int, error) {
	if err := w.c.write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func clean(name string) string { return strings.TrimPrefix(filepath.ToSlash(name), "/") }

// Tree adds everything under root, skipping any path skip reports. Paths in the
// archive are relative to root. Directories come before their contents, and
// the order is stable so the same tree always yields the same bytes.
func (c *CPIO) Tree(root string, skip func(rel string) bool) error {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, rel := range paths {
		full := filepath.Join(root, filepath.FromSlash(rel))
		fi, err := os.Lstat(full)
		if err != nil {
			return err
		}
		perm := uint32(fi.Mode().Perm())
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			err = c.Symlink(rel, target)
			if err != nil {
				return err
			}
		case fi.IsDir():
			if err := c.Dir(rel, perm); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			f, err := os.Open(full)
			if err != nil {
				return err
			}
			err = c.File(rel, perm, fi.Size(), f)
			f.Close()
			if err != nil {
				return err
			}
		default:
			// Sockets, devices and the like have no place in a root filesystem
			// built from a downloaded archive.
		}
	}
	return nil
}

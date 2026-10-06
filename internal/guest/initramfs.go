package guest

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// BuildInitramfs writes the guest's initramfs to out: the base layer from
// baseGz followed by the Android root filesystem in rootfsDir.
//
// The kernel unpacks an initramfs that is several compressed cpio archives
// back to back, so the base layer is copied through as it is. The session
// (rootfs/data) and the mount points a running system recreates are left out:
// the session lives on the data disk instead.
func BuildInitramfs(baseGz, rootfsDir, out string) error {
	if _, err := os.Stat(filepath.Join(rootfsDir, "system", "bin", "main")); err != nil {
		return fmt.Errorf("%s is not the daemon's root filesystem: %w", rootfsDir, err)
	}
	tmp := out + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	base, err := os.Open(baseGz)
	if err != nil {
		f.Close()
		return err
	}
	_, err = io.Copy(f, base)
	base.Close()
	if err != nil {
		f.Close()
		return err
	}

	// The Android tree compresses well and is written once per daemon version,
	// so favour speed over size.
	zw, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		f.Close()
		return err
	}
	c := NewCPIO(zw)
	skip := func(rel string) bool {
		for _, top := range []string{"data", "dev", "proc", "sys"} {
			if rel == top || strings.HasPrefix(rel, top+"/") {
				return true
			}
		}
		return false
	}
	// system/ is the only part of the tree that is ever used; its parent
	// directory entries come from Tree itself.
	if err := c.Tree(rootfsDir, skip); err != nil {
		f.Close()
		return err
	}
	if err := c.Close(); err != nil {
		f.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, out); err != nil {
		return err
	}
	return nil
}

// InstallDataImage unpacks the empty data disk to dst unless one is there: an
// existing disk holds the Apple session and must never be replaced.
func InstallDataImage(templateGz, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	in, err := os.Open(templateGz)
	if err != nil {
		return err
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, zr); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

package guest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:embed init.sh
var initScript []byte

// Package is one pinned Alpine package.
type Package struct {
	URL    string
	SHA256 string
}

// Pins are the exact Alpine v3.24 packages the guest is built from. Alpine
// keeps a release's packages until it is end-of-life, and the hashes make a
// changed or tampered file fail loudly instead of booting something else.
var Pins = map[string]map[string]Package{
	"x86_64": {
		"kernel": {
			URL:    "https://dl-cdn.alpinelinux.org/alpine/v3.24/main/x86_64/linux-virt-6.18.55-r0.apk",
			SHA256: "a941c15fc5db26b6692fd0140fa0970da76cb12aadf3dc8306c419f21bd39c93",
		},
		"busybox": {
			URL:    "https://dl-cdn.alpinelinux.org/alpine/v3.24/main/x86_64/busybox-static-1.37.0-r31.apk",
			SHA256: "b0d6cfc585d7fdd12df359cc5d38d5e45894e9e9e78c4059eb41577d022ec734",
		},
	},
	"aarch64": {
		"kernel": {
			URL:    "https://dl-cdn.alpinelinux.org/alpine/v3.24/main/aarch64/linux-virt-6.18.55-r0.apk",
			SHA256: "c7fb892408d7fe163a18671e5c7816752976d1c67fd17794dfba794aa0d6c1ac",
		},
		"busybox": {
			URL:    "https://dl-cdn.alpinelinux.org/alpine/v3.24/main/aarch64/busybox-static-1.37.0-r31.apk",
			SHA256: "965777e06b94bf11981d5f4ecdfcd577879f4b0dda544294d1dd3f72b217bc75",
		},
	},
}

// wantedModules are what the guest needs that the Alpine kernel builds as
// modules (the virtio core and PCI transport are built in): the disk, the
// network card, the file system on the disk, and the fw_cfg interface the host
// passes arguments through.
var wantedModules = []string{"virtio_blk", "virtio_net", "ext4", "qemu_fw_cfg"}

// Fetch downloads p into dir and checks its hash, returning the path.
func Fetch(p Package, dir string) (string, error) {
	dst := filepath.Join(dir, path.Base(p.URL))
	if b, err := os.ReadFile(dst); err == nil && sum(b) == p.SHA256 {
		return dst, nil
	}
	hc := &http.Client{Timeout: 5 * time.Minute}
	resp, err := hc.Get(p.URL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("%s: %s", p.URL, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if got := sum(b); got != p.SHA256 {
		return "", fmt.Errorf("%s: sha256 %s, want %s", p.URL, got, p.SHA256)
	}
	return dst, os.WriteFile(dst, b, 0o644)
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// walkAPK calls fn for every file in an Alpine package. A package is three
// gzip streams back to back (signature, control, data), each a tar; reading
// them as one multistream gzip yields all the entries in turn.
func walkAPK(file string, fn func(h *tar.Header, r io.Reader) error) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(h, tr); err != nil {
			return err
		}
	}
}

// BuildBase writes the guest's fixed parts into outDir: vmlinuz, base.cpio.gz
// (busybox, the modules above and the init script) and data.img.gz (an empty
// ext4 disk for the Apple session). It needs mke2fs on the machine that builds
// it (any Linux CI runner), never on the user's.
func BuildBase(arch, outDir, cacheDir string) error {
	pins, ok := Pins[arch]
	if !ok {
		return fmt.Errorf("no guest for %q", arch)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	kernelAPK, err := Fetch(pins["kernel"], cacheDir)
	if err != nil {
		return err
	}
	busyAPK, err := Fetch(pins["busybox"], cacheDir)
	if err != nil {
		return err
	}

	// Pass 1: the module dependency table and the kernel image.
	var dep []byte
	var kver string
	var vmlinuz []byte
	err = walkAPK(kernelAPK, func(h *tar.Header, r io.Reader) error {
		switch {
		case strings.HasPrefix(h.Name, "boot/vmlinuz-"):
			b, err := io.ReadAll(r)
			vmlinuz = b
			return err
		case strings.HasSuffix(h.Name, "/modules.dep") && strings.HasPrefix(h.Name, "lib/modules/"):
			kver = strings.Split(h.Name, "/")[2]
			b, err := io.ReadAll(r)
			dep = b
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if vmlinuz == nil || dep == nil {
		return errors.New("kernel package has no kernel image or modules.dep")
	}
	order, err := loadOrder(string(dep), wantedModules)
	if err != nil {
		return err
	}

	// Pass 2: those modules, decompressed (insmod in a static busybox may not
	// read .gz).
	need := map[string]bool{}
	for _, m := range order {
		need["lib/modules/"+kver+"/"+m] = true
	}
	mods := map[string][]byte{}
	err = walkAPK(kernelAPK, func(h *tar.Header, r io.Reader) error {
		if !need[h.Name] {
			return nil
		}
		zr, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		b, err := io.ReadAll(zr)
		mods[h.Name] = b
		return err
	})
	if err != nil {
		return err
	}

	var busybox []byte
	err = walkAPK(busyAPK, func(h *tar.Header, r io.Reader) error {
		if h.Name == "bin/busybox.static" {
			b, err := io.ReadAll(r)
			busybox = b
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if busybox == nil {
		return errors.New("busybox package has no bin/busybox.static")
	}

	// The base layer.
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	c := NewCPIO(zw)
	for _, d := range []string{"bin", "sbin", "dev", "proc", "sys", "etc", "tmp", "lib", "lib/modules"} {
		if err := c.Dir(d, 0o755); err != nil {
			return err
		}
	}
	if err := c.CharDev("dev/console", 0o600, 5, 1); err != nil {
		return err
	}
	if err := c.CharDev("dev/null", 0o666, 1, 3); err != nil {
		return err
	}
	if err := c.Bytes("bin/busybox", 0o755, busybox); err != nil {
		return err
	}
	if err := c.Bytes("init", 0o755, initScript); err != nil {
		return err
	}
	var list []string
	made := map[string]bool{}
	for _, m := range order {
		list = append(list, kver+"/"+m)
		// Modules keep their directory layout under lib/modules/<ver>/.
		for dir := path.Dir("lib/modules/" + kver + "/" + m); dir != "lib/modules" && !made[dir]; dir = path.Dir(dir) {
			made[dir] = true
		}
	}
	dirs := make([]string, 0, len(made))
	for d := range made {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		if err := c.Dir(d, 0o755); err != nil {
			return err
		}
	}
	for _, m := range order {
		name := "lib/modules/" + kver + "/" + m
		name = strings.TrimSuffix(name, ".gz")
		if err := c.Bytes(name, 0o644, mods["lib/modules/"+kver+"/"+m]); err != nil {
			return err
		}
	}
	for i, l := range list {
		list[i] = strings.TrimSuffix(l, ".gz")
	}
	if err := c.Bytes("modules.list", 0o644, []byte(strings.Join(list, "\n")+"\n")); err != nil {
		return err
	}
	if err := c.Close(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(outDir, "vmlinuz"), vmlinuz, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "base.cpio.gz"), buf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := writeDataImage(filepath.Join(outDir, "data.img.gz")); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "ARCH"), []byte(arch+"\n"), 0o644)
}

// loadOrder resolves modules.dep into the order to insmod want and everything
// they need: dependencies first.
func loadOrder(dep string, want []string) ([]string, error) {
	deps := map[string][]string{}
	byBase := map[string]string{}
	for _, line := range strings.Split(dep, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		deps[name] = strings.Fields(rest)
		base := path.Base(name)
		base = strings.TrimSuffix(strings.TrimSuffix(base, ".gz"), ".ko")
		byBase[base] = name
	}
	var order []string
	seen := map[string]bool{}
	var visit func(string) error
	visit = func(m string) error {
		if seen[m] {
			return nil
		}
		seen[m] = true
		for _, d := range deps[m] {
			if err := visit(d); err != nil {
				return err
			}
		}
		order = append(order, m)
		return nil
	}
	for _, w := range want {
		m, ok := byBase[w]
		if !ok {
			return nil, fmt.Errorf("module %s is not in this kernel", w)
		}
		if err := visit(m); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// writeDataImage makes the empty ext4 disk the Apple session is kept on.
func writeDataImage(dst string) error {
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		return errors.New("mkfs.ext4 (e2fsprogs) is needed to build the data disk")
	}
	tmp, err := os.MkdirTemp("", "orchard-data-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "src/data/com.apple.android.music/files"), 0o755); err != nil {
		return err
	}
	img := filepath.Join(tmp, "data.img")
	if out, err := exec.Command(mkfs, "-q", "-F", "-L", "orchard-data", "-d", filepath.Join(tmp, "src"), img, "64M").CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4: %w: %s", err, out)
	}
	raw, err := os.ReadFile(img)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(raw); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(dst, buf.Bytes(), 0o644)
}

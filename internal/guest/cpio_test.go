package guest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The kernel's own reader is the real judge; cpio(1) implements the same format.
func TestCPIOCharDevListing(t *testing.T) {
	if _, err := exec.LookPath("cpio"); err != nil {
		t.Skip("cpio not installed")
	}
	var buf bytes.Buffer
	c := NewCPIO(&buf)
	if err := c.CharDev("dev/console", 0o600, 5, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cpio", "-tv", "--quiet")
	cmd.Stdin = &buf
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "dev/console") || !strings.HasPrefix(string(out), "c") {
		t.Fatalf("listing = %q (%v)", out, err)
	}
}

func TestCPIOIsReadableByCpio(t *testing.T) {
	if _, err := exec.LookPath("cpio"); err != nil {
		t.Skip("cpio not installed")
	}
	src := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "system/bin"), 0o755))
	must(os.WriteFile(filepath.Join(src, "system/bin/main"), []byte("#!daemon\n"), 0o755))
	must(os.WriteFile(filepath.Join(src, "odd.txt"), []byte("12345"), 0o644)) // not a multiple of 4
	must(os.Symlink("bin/main", filepath.Join(src, "system/run")))
	must(os.MkdirAll(filepath.Join(src, "data/secret"), 0o700))
	must(os.WriteFile(filepath.Join(src, "data/secret/token"), []byte("x"), 0o600))

	var buf bytes.Buffer
	c := NewCPIO(&buf)
	must(c.Dir("dev", 0o755))
	must(c.Bytes("init", 0o755, []byte("#!/bin/sh\n")))
	must(c.Tree(src, func(rel string) bool { return rel == "data" || strings.HasPrefix(rel, "data/") }))
	must(c.Close())

	out := t.TempDir()
	cmd := exec.Command("cpio", "-idm", "--quiet")
	cmd.Dir = out
	cmd.Stdin = &buf
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cpio: %v\n%s", err, b)
	}
	if got, _ := os.ReadFile(filepath.Join(out, "odd.txt")); string(got) != "12345" {
		t.Errorf("odd.txt = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(out, "system/bin/main")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("main: %v %v", fi, err)
	}
	if l, err := os.Readlink(filepath.Join(out, "system/run")); err != nil || l != "bin/main" {
		t.Errorf("symlink: %q %v", l, err)
	}
	if _, err := os.Stat(filepath.Join(out, "data")); err == nil {
		t.Error("skipped directory was archived")
	}
	if got, _ := os.ReadFile(filepath.Join(out, "init")); string(got) != "#!/bin/sh\n" {
		t.Errorf("init = %q", got)
	}
}

package wrapper

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeGuest(t *testing.T) (qemu, guestDir string) {
	t.Helper()
	root := t.TempDir()
	guestDir = filepath.Join(root, "guest")
	if err := os.MkdirAll(guestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"vmlinuz", "base.cpio.gz", "data.img.gz"} {
		if err := os.WriteFile(filepath.Join(guestDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(guestDir, "ARCH"), []byte("x86_64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	qemu = filepath.Join(root, "qemu-system-x86_64")
	if err := os.WriteFile(qemu, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return qemu, guestDir
}

func TestQEMUCommandForwardsPortsAndKeepsSecretsOffTheCommandLine(t *testing.T) {
	qemu, guestDir := fakeGuest(t)
	t.Setenv(qemuEnv, qemu)
	t.Setenv(guestEnv, guestDir)

	dir := filepath.Join(t.TempDir(), "wrapper")
	if err := os.MkdirAll(vmDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd, err := qemuCommand(dir, []string{
		"-H", "127.0.0.1", "-D", "10020", "-M", "20020", "-A", "30020", "-K", "40020",
		"-B", "/data/data/com.apple.android.music/files",
		"-L", "someone@example.com:hunter2 pass", "-F",
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")

	for _, p := range []string{"10020", "20020", "30020", "40020"} {
		if !strings.Contains(joined, "hostfwd=tcp:127.0.0.1:"+p+"-:"+p) {
			t.Errorf("port %s is not forwarded: %s", p, joined)
		}
	}
	if strings.Contains(joined, "hunter2") || strings.Contains(joined, "someone@example.com") {
		t.Errorf("credentials are on the command line: %s", joined)
	}
	if !strings.Contains(joined, "-fw_cfg name=opt/orchard/args,file=") {
		t.Errorf("arguments are not passed through fw_cfg: %s", joined)
	}

	// The guest gets the arguments, with the daemon listening on every interface.
	var argsFile string
	for _, a := range cmd.Args {
		if v, ok := strings.CutPrefix(a, "name=opt/orchard/args,file="); ok {
			argsFile = v
		}
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	want := []string{"-H", "0.0.0.0", "-D", "10020", "-M", "20020", "-A", "30020", "-K", "40020",
		"-B", "/data/data/com.apple.android.music/files", "-L", "someone@example.com:hunter2 pass", "-F"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("guest args = %q, want %q", got, want)
	}
	if fi, err := os.Stat(argsFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("args file mode: %v %v", fi, err)
	}
}

func TestQEMUCommandRejectsLineBreaksInArguments(t *testing.T) {
	qemu, guestDir := fakeGuest(t)
	t.Setenv(qemuEnv, qemu)
	t.Setenv(guestEnv, guestDir)
	dir := filepath.Join(t.TempDir(), "wrapper")
	if err := os.MkdirAll(vmDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := qemuCommand(dir, []string{"-L", "a\nb:c"}); err == nil {
		t.Fatal("a line break would split one argument into two in the guest")
	}
}

func TestQEMUFromEnvNamesWhatIsMissing(t *testing.T) {
	t.Setenv(qemuEnv, "")
	t.Setenv(guestEnv, "")
	if _, err := qemuFromEnv(); err == nil {
		t.Fatal("expected an error with nothing configured")
	}
	qemu, guestDir := fakeGuest(t)
	t.Setenv(qemuEnv, qemu)
	t.Setenv(guestEnv, guestDir)
	if err := os.Remove(filepath.Join(guestDir, "vmlinuz")); err != nil {
		t.Fatal(err)
	}
	if _, err := qemuFromEnv(); err == nil || !strings.Contains(err.Error(), "vmlinuz") {
		t.Fatalf("err = %v, want one naming vmlinuz", err)
	}
}

func TestVMSessionKeepsItsOwnMarkerAndForgetsTheDisk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wrapper")
	v := newVMSession(dir)
	if v.LoggedIn() {
		t.Fatal("logged in before anything happened")
	}
	if err := v.MarkLoggedIn(); err != nil {
		t.Fatal(err)
	}
	if !v.LoggedIn() {
		t.Fatal("marker not seen")
	}
	if err := os.WriteFile(filepath.Join(vmDir(dir), "data.img"), []byte("session"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := v.Forget(); err != nil {
		t.Fatal(err)
	}
	if v.LoggedIn() {
		t.Error("still logged in after Forget")
	}
	if _, err := os.Stat(filepath.Join(vmDir(dir), "data.img")); err == nil {
		t.Error("the data disk survived Forget")
	}
	if err := v.Send2FA("123456"); err == nil {
		t.Error("a code was accepted with no daemon running")
	}
}

func TestSeedSessionCarriesAHostLoginIntoTheGuest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wrapper")
	if err := os.MkdirAll(vmDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := seedSession(dir); err != nil || got != "" {
		t.Fatalf("no login yet: seed = %q, err = %v", got, err)
	}
	if v := newVMSession(dir); v.LoggedIn() {
		t.Fatal("logged in with nothing on disk")
	}

	base := filepath.Join(dir, "rootfs", "data", "data", "com.apple.android.music", "files")
	if err := os.MkdirAll(filepath.Join(base, "mpl_db"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"STOREFRONT_ID": "143441-1", "mpl_db/kvs.sqlitedb": "db", "MUSIC_TOKEN": "tok"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if v := newVMSession(dir); !v.LoggedIn() {
		t.Fatal("a login waiting to be carried in must count as logged in, or the daemon never starts")
	}

	seed, err := seedSession(dir)
	if err != nil || seed == "" {
		t.Fatalf("seed = %q, err = %v", seed, err)
	}
	f, err := os.Open(seed)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := map[string]string{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	for name, want := range map[string]string{
		"data/com.apple.android.music/files/STOREFRONT_ID":       "143441-1",
		"data/com.apple.android.music/files/mpl_db/kvs.sqlitedb": "db",
		"data/com.apple.android.music/files/MUSIC_TOKEN":         "tok",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q (have %v)", name, got[name], want, keys(got))
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

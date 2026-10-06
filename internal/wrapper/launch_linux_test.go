//go:build linux && !android

package wrapper

import "testing"

// The stock launcher locates rootfs/ from its own argv[0] and silently exits 0
// when that is a long absolute path, so the daemon must be started as ./wrapper
// from its own directory whatever BinPath looks like.
func TestDaemonCommandRunsLauncherAsRelativeBinary(t *testing.T) {
	dir := "/a/very/long/path/orchard/data/wrapper"
	cmd, err := daemonCommand(dir, ProvisionInfo{BinPath: dir + "/wrapper"}, []string{"-H", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Args[0] != "./wrapper" {
		t.Errorf("argv[0] = %q, want ./wrapper", cmd.Args[0])
	}
	if cmd.Dir != dir {
		t.Errorf("Dir = %q, want %q", cmd.Dir, dir)
	}
	if cmd.Path != dir+"/wrapper" {
		t.Errorf("Path = %q, want the real binary", cmd.Path)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Pdeathsig == 0 {
		t.Error("the daemon must die with orchard (Pdeathsig)")
	}
}

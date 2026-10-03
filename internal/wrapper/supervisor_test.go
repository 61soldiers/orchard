package wrapper

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWrapperHasKeyPort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake wrapper is a shell script")
	}

	dir := t.TempDir()

	// A stand-in for the wrapper binary: prints a usage block to stderr (which
	// is where the real daemon writes it) and exits, like `wrapper -h`.
	write := func(name, usage string) string {
		p := filepath.Join(dir, name)
		script := "#!/bin/sh\ncat >&2 <<'EOF'\n" + usage + "\nEOF\n"
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	withKey := write("wrapper-new", `Usage: wrapper [OPTION]...
  -A, --account-port=INT      (default=`+"`30020'"+`)
  -K, --key-port=INT          (default=`+"`40020'"+`)
  -L, --login=STRING        username:password`)

	withoutKey := write("wrapper-old", `Usage: wrapper [OPTION]...
  -A, --account-port=INT      (default=`+"`30020'"+`)
  -L, --login=STRING        username:password`)

	if !wrapperHasKeyPort(withKey) {
		t.Errorf("wrapperHasKeyPort() = false for a build that lists --key-port")
	}
	if wrapperHasKeyPort(withoutKey) {
		t.Errorf("wrapperHasKeyPort() = true for a build with no --key-port")
	}
	if wrapperHasKeyPort(filepath.Join(dir, "does-not-exist")) {
		t.Errorf("wrapperHasKeyPort() = true for a missing binary")
	}
}

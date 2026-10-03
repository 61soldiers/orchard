package apple

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocateSessionDB(t *testing.T) {
	write := func(t *testing.T, path string, empty bool) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		body := []byte("x")
		if empty {
			body = nil
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("none present", func(t *testing.T) {
		base := t.TempDir()
		got, ok := locateSessionDB(base)
		if ok {
			t.Errorf("ok = true with no db on disk")
		}
		if want := filepath.Join(base, "mpl_db", "kvs.sqlitedb"); got != want {
			t.Errorf("path = %q, want preferred %q", got, want)
		}
	})

	t.Run("legacy flat layout", func(t *testing.T) {
		base := t.TempDir()
		flat := filepath.Join(base, "kvs.sqlitedb")
		write(t, flat, false)
		got, ok := locateSessionDB(base)
		if !ok || got != flat {
			t.Errorf("locateSessionDB() = %q, %v; want %q, true", got, ok, flat)
		}
	})

	t.Run("mpl_db wins over flat", func(t *testing.T) {
		base := t.TempDir()
		nested := filepath.Join(base, "mpl_db", "kvs.sqlitedb")
		write(t, filepath.Join(base, "kvs.sqlitedb"), false)
		write(t, nested, false)
		got, ok := locateSessionDB(base)
		if !ok || got != nested {
			t.Errorf("locateSessionDB() = %q, %v; want %q, true", got, ok, nested)
		}
	})

	t.Run("empty db does not count", func(t *testing.T) {
		base := t.TempDir()
		write(t, filepath.Join(base, "kvs.sqlitedb"), true)
		if _, ok := locateSessionDB(base); ok {
			t.Errorf("ok = true for a zero-byte db")
		}
	})
}

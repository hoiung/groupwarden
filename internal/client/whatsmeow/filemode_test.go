package whatsmeow

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestOpenMakesTheSessionOwnerOnly: the session store (the bot's device keys)
// is owner-only whatever umask the caller has (`pair` run by hand gets 022),
// and a store an older run left 0644, with a -wal that still holds pages (an
// unclean stop), is made owner-only too.
func TestOpenMakesTheSessionOwnerOnly(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fresh, left := t.TempDir(), t.TempDir()
	// The left store is a copy of a store still open in fresh, taken before
	// its WAL is checkpointed: the copy's -wal holds pages SQLite must recover,
	// and every side file OwnerOnly names is left 0644.
	a, err := Open(context.Background(), Options{DataDir: fresh, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"whatsmeow.db", "whatsmeow.db-wal", "whatsmeow.db-shm"} {
		// The first Open alone (what `pair` does) already leaves the device
		// keys and the side files SQLite makes for them owner-only.
		st, err := os.Stat(filepath.Join(fresh, f))
		if err != nil {
			t.Fatal(err)
		}
		if mode := st.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s after the first Open: mode %o, want 600", f, mode)
		}
		copyMode(t, filepath.Join(fresh, f), filepath.Join(left, f), 0o644)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{fresh, left} {
		a, err := Open(context.Background(), Options{DataDir: dir, Log: log})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"whatsmeow.db", "whatsmeow.db-wal", "whatsmeow.db-shm"} {
			st, err := os.Stat(filepath.Join(dir, f))
			if os.IsNotExist(err) && f != "whatsmeow.db" && dir == fresh {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode := st.Mode().Perm(); mode != 0o600 {
				t.Errorf("%s in %s: mode %o, want 600", f, dir, mode)
			}
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// copyMode copies src to dst and gives dst mode.
func copyMode(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(src) // #nosec G304 -- a test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatalf("%s is empty", src)
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, mode); err != nil {
		t.Fatal(err)
	}
}

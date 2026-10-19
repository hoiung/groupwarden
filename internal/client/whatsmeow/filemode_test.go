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
// and a store an older run left 0644, with its -wal, is made owner-only too.
func TestOpenMakesTheSessionOwnerOnly(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fresh, left := t.TempDir(), t.TempDir()
	for _, f := range []string{"whatsmeow.db", "whatsmeow.db-wal"} {
		if err := os.WriteFile(filepath.Join(left, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(left, f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{fresh, left} {
		a, err := Open(context.Background(), Options{DataDir: dir, Log: log})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"whatsmeow.db", "whatsmeow.db-wal", "whatsmeow.db-shm"} {
			st, err := os.Stat(filepath.Join(dir, f))
			if os.IsNotExist(err) && f != "whatsmeow.db" {
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

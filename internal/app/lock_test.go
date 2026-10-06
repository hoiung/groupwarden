package app

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLockHolderNamesTheCommand: while a command holds the data dir the lock
// file names its process and command (the config sync signals only `run`);
// once released, or before any lock, nobody holds it.
func TestLockHolderNamesTheCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if _, _, held, err := LockHolder(dir); held || err != nil {
		t.Fatalf("no lock file: held %v err %v", held, err)
	}
	l, err := AcquireLock(dir, "run")
	if err != nil {
		t.Fatal(err)
	}
	pid, cmd, held, err := LockHolder(dir)
	if err != nil || !held || pid != os.Getpid() || cmd != "run" {
		t.Fatalf("held by run: pid %d cmd %q held %v err %v", pid, cmd, held, err)
	}
	if _, err := AcquireLock(dir, "groups"); err != ErrAlreadyRunning {
		t.Fatalf("second lock: %v", err)
	}
	l.Release()
	if _, _, held, err := LockHolder(dir); held || err != nil {
		t.Fatalf("after release: held %v err %v", held, err)
	}
	// A later holder rewrites the file whole, also when its line is shorter.
	for _, c := range []string{"groups", "run"} {
		l2, err := AcquireLock(dir, c)
		if err != nil {
			t.Fatal(err)
		}
		_, cmd, held, err := LockHolder(dir)
		l2.Release()
		if err != nil || !held || cmd != c {
			t.Fatalf("held by %s: cmd %q held %v err %v", c, cmd, held, err)
		}
	}
	l2, err := AcquireLock(dir, "groups")
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Release()
	// A held lock whose file is not "<pid> <command>" is an error, never a guess.
	for _, bad := range []string{"garbage\n", "1234\n", "1234 run extra\n", "0 run\n", "x run\n"} {
		if err := os.WriteFile(filepath.Join(dir, lockName), []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, held, err := LockHolder(dir); !held || err == nil {
			t.Errorf("lock file %q: held %v err %v", bad, held, err)
		}
	}
}

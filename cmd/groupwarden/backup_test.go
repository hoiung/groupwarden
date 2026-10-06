package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/store"
)

// TestBackupAndRestoreCommands: `backup-dir` prints the target; `backup`
// refuses without a database, records each run's result for /status, alerts
// the admins once per failure episode, and writes a backup that `restore`
// puts back (refusing beside a running bot or over an existing database), so
// that the bot then reports every action paused.
func TestBackupAndRestoreCommands(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	var tick atomic.Int64
	te.now = func() time.Time { return time.Date(2026, 10, 6, 3, 15, int(tick.Add(1)), 0, time.UTC) }
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	te.cfgPath = te.writeConfigAge(t, id.Recipient().String())
	te.writeFile(t, "backup-key.txt", id.String()+"\n", 0o600)
	target := filepath.Join(te.dir, "backup")
	dataDir := filepath.Join(te.dir, "data")

	if code := te.cmd("backup-dir"); code != exitOK || strings.TrimSpace(te.out.String()) != target {
		t.Fatalf("backup-dir: %d %q; want %s", code, te.out.String(), target)
	}
	if code := te.cmd("backup"); code != exitFail || !strings.Contains(te.out.String(), "FAILED: no groupwarden.db") {
		t.Fatalf("backup without a database: %d %q", code, te.out.String())
	}

	st := setStatus(t, te, map[string]string{"test_marker": "kept"}, time.Now())
	alerts := func() int {
		t.Helper()
		reps, err := st.UnsentReportsOf(context.Background(), string(alert.BackupFailed), 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range reps {
			if !r.Priority {
				t.Fatalf("a backup failure report is not priority: %+v", r)
			}
		}
		return len(reps)
	}
	result := func() string {
		t.Helper()
		status, err := st.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status[store.StatusBackupLastRun].Value == "" {
			t.Fatal("the run time was not recorded")
		}
		return status[store.StatusBackupResult].Value
	}
	backupOK := func() {
		t.Helper()
		if code := te.cmd("backup"); code != exitOK || !strings.HasPrefix(te.out.String(), "ok groupwarden-") {
			t.Fatalf("backup: %d %q %s", code, te.out.String(), te.errb.String())
		}
		if r := result(); !strings.HasPrefix(r, "ok groupwarden-") {
			t.Fatalf("recorded result %q", r)
		}
	}
	backupFails := func(wantAlerts int) {
		t.Helper()
		if code := te.cmd("backup"); code != exitFail || !strings.HasPrefix(te.out.String(), "FAILED: ") {
			t.Fatalf("backup to a missing directory: %d %q", code, te.out.String())
		}
		if r := result(); !strings.HasPrefix(r, "FAILED: ") {
			t.Fatalf("recorded result %q", r)
		}
		if n := alerts(); n != wantAlerts {
			t.Fatalf("%d backup failure alerts; want %d", n, wantAlerts)
		}
	}
	// One alert per failure episode: two failures, one alert; a success
	// ends the episode.
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	backupFails(1)
	backupFails(1)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	backupOK()
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	backupFails(2)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	backupOK()
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 1 {
		t.Fatalf("backup directory: %v %v", entries, err)
	}
	backupFile := filepath.Join(target, entries[0].Name())

	restore := func() int {
		return te.cmd("restore", "--identity", filepath.Join(te.dir, "backup-key.txt"), backupFile)
	}
	lock, err := app.AcquireLock(dataDir, "run")
	if err != nil {
		t.Fatal(err)
	}
	if code := restore(); code != exitFail || !strings.Contains(te.errb.String(), "stop groupwarden.service first") {
		t.Fatalf("restore beside a running bot: %d %q", code, te.errb.String())
	}
	lock.Release()
	if code := restore(); code != exitFail || !strings.Contains(te.errb.String(), "aside first") {
		t.Fatalf("restore over a database: %d %q", code, te.errb.String())
	}
	_ = st.Close()
	aside := filepath.Join(te.dir, "aside")
	if err := os.MkdirAll(aside, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"groupwarden.db", "groupwarden.db-wal", "groupwarden.db-shm"} {
		if err := os.Rename(filepath.Join(dataDir, f), filepath.Join(aside, f)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if code := restore(); code != exitOK || !strings.Contains(te.out.String(), "PAUSED") {
		t.Fatalf("restore: %d %q %s", code, te.out.String(), te.errb.String())
	}
	te.cmd("healthcheck")
	if !strings.Contains(te.out.String(), "paused: every action since") || !strings.Contains(te.out.String(), "(restore)") {
		t.Fatalf("healthcheck after a restore:\n%s", te.out.String())
	}
	restored, err := store.Open(context.Background(), filepath.Join(dataDir, "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	status, err := restored.Status(context.Background())
	if err != nil || status["test_marker"].Value != "kept" {
		t.Fatalf("the restored database lost its data: %v %v", status["test_marker"], err)
	}
}

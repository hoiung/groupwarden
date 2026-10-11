package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
)

// TestBackupRefusesATargetOnTheDataDisk: a network share that is not mounted
// leaves its mount point an empty folder on the local disk. `backup` checks
// the target first, so it writes nothing there: the run fails, says why, is
// recorded for /status and alerts the admins.
func TestBackupRefusesATargetOnTheDataDisk(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	st := setStatus(t, te, map[string]string{"test_marker": "kept"}, time.Now())
	te.deviceOf = func(string) (uint64, error) { return 1, nil } // the share is not mounted: same disk as the data dir
	if code := te.cmd("backup"); code != exitFail ||
		!strings.HasPrefix(te.out.String(), "FAILED: backup.target_dir cannot take the backup: ") ||
		!strings.Contains(te.out.String(), "same disk as the data dir") {
		t.Fatalf("backup to the data disk: %d %q", code, te.out.String())
	}
	entries, err := os.ReadDir(filepath.Join(te.dir, "backup"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("the target holds %v (%v), want nothing written", entries, err)
	}
	status, err := st.Status(context.Background())
	if err != nil || !strings.HasPrefix(status[store.StatusBackupResult].Value, "FAILED: backup.target_dir cannot take") {
		t.Fatalf("recorded result %q (%v)", status[store.StatusBackupResult].Value, err)
	}
	reps, err := st.UnsentReportsOf(context.Background(), string(alert.BackupFailed), 10)
	if err != nil || len(reps) != 1 || !strings.Contains(reps[0].Text, "same disk as the data dir") {
		t.Fatalf("backup failure alerts %+v (%v), want one naming the reason", reps, err)
	}
}

// TestBackupUnitFailureRecorded: when the backup unit fails before the
// backup records anything (systemd could not open a network share it must
// make writable, status 226), its failure handler records the failure, naming
// the share, and reports it once per episode; a failed run that recorded its
// own result is left as it is, also under a systemd too old to say which run
// failed.
func TestBackupUnitFailureRecorded(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	st := setStatus(t, te, map[string]string{store.StatusBackupResult: "ok groupwarden-1.age (1 bytes, 0 older deleted)",
		store.StatusBackupInvocation: "run-1"}, time.Now())
	monitor := map[string]string{"MONITOR_INVOCATION_ID": "run-2", "MONITOR_SERVICE_RESULT": "exit-code",
		"MONITOR_EXIT_CODE": "exited", "MONITOR_EXIT_STATUS": "226"}
	te.getenv = func(k string) string { return monitor[k] }
	for night := 2; night <= 3; night++ {
		monitor["MONITOR_INVOCATION_ID"] = "run-" + strconv.Itoa(night)
		if code := te.cmd("backup-failed"); code != exitOK || !strings.Contains(te.out.String(), "is the network share reachable") {
			t.Fatalf("night %d: %d %q %q", night, code, te.out.String(), te.errb.String())
		}
	}
	status, err := st.Status(context.Background())
	if err != nil || status[store.StatusBackupResult].Value != "FAILED: the backup unit stopped before the backup recorded "+
		"a result (systemd: exit-code, exited 226); systemd could not open "+filepath.Join(te.dir, "backup")+
		" to start it: is the network share reachable (the NAS on, the network up, the password current)?" {
		t.Fatalf("recorded result %q (%v)", status[store.StatusBackupResult].Value, err)
	}
	reps, err := st.UnsentReportsOf(context.Background(), string(alert.BackupFailed), 10)
	if err != nil || len(reps) != 1 || !strings.Contains(reps[0].Text, "The nightly backup FAILED: the backup unit stopped") {
		t.Fatalf("backup failure alerts %+v (%v), want one", reps, err)
	}

	// The backup ran, failed and said why: the handler adds nothing.
	te.deviceOf = func(string) (uint64, error) { return 1, nil }
	te.getenv = func(k string) string { return map[string]string{"INVOCATION_ID": "run-4"}[k] }
	if code := te.cmd("backup"); code != exitFail {
		t.Fatalf("backup to the data disk: %d %q", code, te.out.String())
	}
	monitor["MONITOR_INVOCATION_ID"], monitor["MONITOR_EXIT_STATUS"] = "run-4", "1"
	te.getenv = func(k string) string { return monitor[k] }
	if code := te.cmd("backup-failed"); code != exitOK || !strings.HasPrefix(te.out.String(), "the failed backup recorded its own result") {
		t.Fatalf("handler after a recorded failure: %d %q", code, te.out.String())
	}
	if status, err := st.Status(context.Background()); err != nil ||
		!strings.Contains(status[store.StatusBackupResult].Value, "same disk as the data dir") {
		t.Fatalf("recorded result %q (%v), want the backup's own reason kept", status[store.StatusBackupResult].Value, err)
	}

	// systemd before 251 passes no MONITOR_*: a recorded failure is kept,
	// with no second report, but each night still counts as a run, so the
	// overdue check never blames the timer for a share that stays down.
	te.getenv = func(string) string { return "" }
	for night := 5; night <= 7; night++ {
		at := time.Date(2026, 10, night, 3, 20, 0, 0, time.UTC)
		te.now = func() time.Time { return at }
		if code := te.cmd("backup-failed"); code != exitOK || !strings.HasPrefix(te.out.String(), "a failed backup is already recorded") {
			t.Fatalf("night %d: handler without MONITOR_* after a recorded failure: %d %q", night, code, te.out.String())
		}
		status, err := st.Status(context.Background())
		if err != nil || !strings.Contains(status[store.StatusBackupResult].Value, "same disk as the data dir") {
			t.Fatalf("night %d: recorded result %q (%v), want the backup's own reason kept", night, status[store.StatusBackupResult].Value, err)
		}
		if got := status[store.StatusBackupLastRun].Value; got != store.StatusTime(at) {
			t.Fatalf("night %d: last run %q, want %q", night, got, store.StatusTime(at))
		}
	}
	if reps, err := st.UnsentReportsOf(context.Background(), string(alert.BackupFailed), 10); err != nil || len(reps) != 1 {
		t.Fatalf("backup failure alerts %+v (%v), want still the one", reps, err)
	}
	// The last good run was by hand: no invocation ID, as none reached the handler.
	if err := st.SetStatus(context.Background(), map[string]string{store.StatusBackupResult: "ok groupwarden-2.age (1 bytes, 0 older deleted)",
		store.StatusBackupInvocation: ""}); err != nil {
		t.Fatal(err)
	}
	if code := te.cmd("backup-failed"); code != exitOK {
		t.Fatalf("handler without MONITOR_* after a good night: %d %q", code, te.out.String())
	}
	if status, err := st.Status(context.Background()); err != nil || status[store.StatusBackupResult].Value !=
		"FAILED: the backup unit stopped before the backup recorded a result (systemd did not say how: see journalctl -u groupwarden-backup.service)" {
		t.Fatalf("recorded result %q (%v)", status[store.StatusBackupResult].Value, err)
	}
}

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
	// The alert and the status recording it are written together: while the
	// status cannot be written, a failed backup raises no alert, so the next
	// run's alert is the only one.
	lift := modtest.FailWritesInFile(t, st, "status", "")
	if code := te.cmd("backup"); code != exitFail {
		t.Fatalf("backup while the status cannot be written: %d %q", code, te.out.String())
	}
	if n := alerts(); n != 0 {
		t.Fatalf("%d backup failure alerts while their status could not be written, want 0", n)
	}
	lift()
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

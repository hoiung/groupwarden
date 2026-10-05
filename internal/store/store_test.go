package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func openTemp(t *testing.T, opts Options) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "groupwarden.db")
	s, err := Open(context.Background(), path, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestMigrationsFromEmpty(t *testing.T) {
	ctx := context.Background()
	s, path := openTemp(t, Options{})

	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Fatalf("schema version %d, want %d", version, len(migrations))
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	_ = rows.Close()
	want := []string{"counters", "inbox", "pause", "schema_version", "seen", "status"}
	sort.Strings(want)
	if strings.Join(tables, ",") != strings.Join(want, ",") {
		t.Fatalf("tables = %v, want %v", tables, want)
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q (%v), want wal", mode, err)
	}

	// Re-opening applies nothing twice.
	_ = s.Close()
	s2, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	// A database from a newer binary is refused, never downgraded.
	if _, err := s2.db.ExecContext(ctx, `UPDATE schema_version SET version = ?`, len(migrations)+1); err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
	if _, err := Open(ctx, path, Options{}); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("newer schema: err = %v, want refusal", err)
	}
}

const mountTable = `22 1 8:48 / / rw,relatime - ext4 /dev/sdd rw,discard
626 22 0:88 / /mnt/c rw,noatime - 9p C:\134 rw,aname=drvfs;path=C:\;uid=1000
627 22 0:89 / /mnt/d rw,noatime - drvfs D:\134 rw
628 22 0:90 / /mnt/e rw,noatime - virtiofs drvfs rw,aname=drvfs;path=E:\
629 22 8:64 / /mnt/backup rw,relatime - ext4 /dev/sde rw
630 22 0:91 / /mnt/c\040drive rw,relatime - ext4 /dev/sdf rw
`

func TestRefusesWindowsFilesystem(t *testing.T) {
	refused := []string{"/mnt/c/groupwarden/data", "/mnt/c", "/mnt/d/x", "/mnt/e/data"}
	for _, p := range refused {
		err := checkFS(p, strings.NewReader(mountTable))
		if err == nil || !strings.Contains(err.Error(), "Windows-mounted") {
			t.Errorf("%s: err = %v, want refusal", p, err)
		}
	}
	// The verdict comes from the mount's type, not the path: a Linux disk
	// mounted under /mnt (even one whose name starts like a drive) is fine.
	allowed := []string{"/var/lib/groupwarden", "/mnt/backup/groupwarden", "/mnt/c drive/data", "/mnt/cx"}
	for _, p := range allowed {
		if err := checkFS(p, strings.NewReader(mountTable)); err != nil {
			t.Errorf("%s: %v, want allowed", p, err)
		}
	}
	// The real table on this machine accepts the test's temp dir.
	if err := CheckLinuxFS(t.TempDir()); err != nil {
		t.Errorf("temp dir refused: %v", err)
	}
}

func TestWriteFailurePausesActions(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	s, _ := openTemp(t, Options{OnWriteFailure: func(error) { calls.Add(1) }})

	if paused, why := s.PausedFor(ctx, ScopeAll); paused {
		t.Fatalf("fresh store paused: %s", why)
	}
	// Make every write fail the way a full or read-only disk does.
	if _, err := s.db.ExecContext(ctx, `PRAGMA query_only = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InboxPut(ctx, "msg|x|1", "message", []byte("{}")); err == nil {
		t.Fatal("write on a read-only database succeeded")
	}
	if _, err := s.InboxPut(ctx, "msg|x|2", "message", []byte("{}")); err == nil {
		t.Fatal("second write succeeded")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("OnWriteFailure called %d times, want once", n)
	}
	for _, scope := range []Scope{ScopeAll, ScopeRemoveBan} {
		paused, why := s.PausedFor(ctx, scope)
		if !paused || !strings.HasPrefix(why, SourceStorage+":") {
			t.Fatalf("scope %s after write failure: paused=%v why=%q, want storage pause", scope, paused, why)
		}
	}
	// A non-storage error (a bad statement) does not pause anything.
	s.ClearStorageFailure()
	if _, err := s.db.ExecContext(ctx, `PRAGMA query_only = 0`); err != nil {
		t.Fatal(err)
	}
	_ = s.Write(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, `SELECT nope FROM nowhere`); return err })
	if paused, why := s.PausedFor(ctx, ScopeAll); paused {
		t.Fatalf("logic error paused the store: %s", why)
	}
}

func TestPauseScopes(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t, Options{})
	if err := s.SetPause(ctx, Pause{Source: SourceExtraCompanion, Scope: ScopeRemoveBan, Reason: "1 other linked device", Since: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if paused, _ := s.PausedFor(ctx, ScopeAll); paused {
		t.Fatal("a remove/ban pause stopped deletes")
	}
	if paused, _ := s.PausedFor(ctx, ScopeRemoveBan); !paused {
		t.Fatal("a remove/ban pause did not stop removals")
	}
	if err := s.ClearPause(ctx, SourceExtraCompanion); err != nil {
		t.Fatal(err)
	}
	if paused, why := s.PausedFor(ctx, ScopeRemoveBan); paused {
		t.Fatalf("still paused after clear: %s", why)
	}
	// Pause state that cannot be read counts as paused.
	_ = s.Close()
	if paused, why := s.PausedFor(ctx, ScopeAll); !paused || !strings.Contains(why, "unreadable") {
		t.Fatalf("closed store: paused=%v why=%q", paused, why)
	}
}

func TestSeenPurge(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	clock := now
	s, _ := openTemp(t, Options{Now: func() time.Time { return clock }})
	for _, id := range []string{"OLD", "NEW"} {
		if _, err := s.InboxPut(ctx, "msg|g|"+id, "message", []byte("{}")); err != nil {
			t.Fatal(err)
		}
		rows, err := s.InboxOldest(ctx, 1)
		if err != nil || len(rows) != 1 {
			t.Fatal(rows, err)
		}
		if err := s.Decide(ctx, rows[0], Seen{Chat: "g", MsgID: id, ServerTime: clock}, func(*sql.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(48 * time.Hour)
	}
	n, err := s.PurgeSeen(ctx, now.Add(24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purged %d (%v), want 1", n, err)
	}
	if _, found, _ := s.SeenTime(ctx, "g", "OLD"); found {
		t.Fatal("old dedupe record survived the purge")
	}
	if at, found, _ := s.SeenTime(ctx, "g", "NEW"); !found || !at.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("new record: %v %v", at, found)
	}
	// Within its retention a decided delivery is still deduped.
	if ok, _ := s.InboxPut(ctx, "msg|g|NEW", "message", []byte("{}")); ok {
		t.Fatal("a delivery decided within the retention was queued again")
	}
}

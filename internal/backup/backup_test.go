package backup

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/store/sessiontest"
)

var t0 = time.Date(2026, 10, 6, 3, 15, 0, 0, time.UTC)

type kit struct {
	t         *testing.T
	data, dst string
	st        *store.Store
	id        *age.X25519Identity
	keyFile   string
}

func newKit(t *testing.T) *kit {
	t.Helper()
	k := &kit{t: t, data: t.TempDir(), dst: t.TempDir()}
	st, err := store.Open(context.Background(), filepath.Join(k.data, "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	k.st = st
	if k.id, err = age.GenerateX25519Identity(); err != nil {
		t.Fatal(err)
	}
	k.keyFile = filepath.Join(t.TempDir(), "backup-key.txt")
	if err := os.WriteFile(k.keyFile, []byte(k.id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return k
}

func (k *kit) backup(at time.Time, keep int) Result {
	k.t.Helper()
	res, err := Run(context.Background(), k.st, Options{TargetDir: k.dst, Recipient: k.id.Recipient().String(),
		Keep: keep, ScratchDir: k.data, Now: at, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		k.t.Fatal(err)
	}
	return res
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// TestBackupEncryptedAndRestorable: a backup is one age-encrypted file named
// by its UTC time, readable only by the owner, holding no plaintext; it
// restores (with the private key) to a groupwarden.db holding the data, with
// every action paused (AC 3.5); no plaintext copy is left behind; the newest
// backup.keep are kept, other files in the directory are never touched, and a
// crash's .partial is cleared. A restore refuses an existing database, the
// wrong key and a file that is not a groupwarden backup, leaving nothing.
func TestBackupEncryptedAndRestorable(t *testing.T) {
	k := newKit(t)
	ctx := context.Background()
	const marker = "ban-list-marker-7f3e"
	if err := k.st.SetStatus(ctx, map[string]string{"test_marker": marker}); err != nil {
		t.Fatal(err)
	}
	res := k.backup(t0, 14)
	if want := filepath.Join(k.dst, "groupwarden-20261006T031500Z.db.age"); res.File != want {
		t.Fatalf("backup file %s; want %s", res.File, want)
	}
	info, err := os.Stat(res.File)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != res.Bytes {
		t.Fatalf("mode %v size %d; want 0600 and %d", info.Mode().Perm(), info.Size(), res.Bytes)
	}
	raw, err := os.ReadFile(res.File)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("age-encryption.org/v1\n")) || bytes.Contains(raw, []byte(marker)) ||
		bytes.Contains(raw, []byte("SQLite format 3")) {
		t.Fatal("the backup is not an age file, or holds plaintext")
	}
	if got := names(t, k.data); len(got) != 3 || got[0] != "groupwarden.db" { // + its -shm and -wal
		t.Fatalf("data dir after a backup: %v (a plaintext copy left?)", got)
	}

	restored := filepath.Join(t.TempDir(), "groupwarden.db")
	if err := Restore(ctx, res.File, k.keyFile, restored); err != nil {
		t.Fatal(err)
	}
	if got := names(t, filepath.Dir(restored)); len(got) != 1 {
		t.Fatalf("restore left %v; want only groupwarden.db", got)
	}
	st, err := store.Open(ctx, restored, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	status, err := st.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pauses, err := st.Pauses(ctx)
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	if status["test_marker"].Value != marker {
		t.Fatal("the restored database lost its data")
	}
	if len(pauses) != 1 || pauses[0].Source != store.SourceRestore || pauses[0].Scope != store.ScopeAll {
		t.Fatalf("restored pauses %+v; want every action paused by the restore", pauses)
	}

	// Refusals leave nothing behind.
	if err := Restore(ctx, res.File, k.keyFile, restored); err == nil || !strings.Contains(err.Error(), "aside first") {
		t.Fatalf("restore over an existing database: %v", err)
	}
	other, _ := age.GenerateX25519Identity()
	wrongKey := filepath.Join(t.TempDir(), "other.txt")
	if err := os.WriteFile(wrongKey, []byte(other.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(t.TempDir(), "groupwarden.db")
	if err := Restore(ctx, res.File, wrongKey, fresh); err == nil {
		t.Fatal("restored with the wrong key")
	}
	notOurs := filepath.Join(t.TempDir(), "plain.db.age")
	encryptFile(t, filepath.Join(k.data, "groupwarden.db"), notOurs, k.id.Recipient()) // no restore pause
	if err := Restore(ctx, notOurs, k.keyFile, fresh); err == nil || !strings.Contains(err.Error(), "no restore pause") {
		t.Fatalf("restored a database that is not a backup: %v", err)
	}
	if got := names(t, filepath.Dir(fresh)); len(got) != 0 {
		t.Fatalf("failed restores left %v", got)
	}

	// Retention: the newest keep stay; a crash's .partial and nothing else
	// goes.
	for _, f := range []string{"notes.txt", "groupwarden-20261001T031500Z.db.age.partial"} {
		if err := os.WriteFile(filepath.Join(k.dst, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for day := 1; day <= 4; day++ {
		k.backup(t0.AddDate(0, 0, day), 3)
	}
	want := []string{"groupwarden-20261008T031500Z.db.age", "groupwarden-20261009T031500Z.db.age",
		"groupwarden-20261010T031500Z.db.age", "notes.txt"}
	if got := names(t, k.dst); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("after 5 backups with keep 3: %v; want %v", got, want)
	}
	// A second backup in the same second is refused, not overwritten.
	if _, err := Run(ctx, k.st, Options{TargetDir: k.dst, Recipient: k.id.Recipient().String(), Keep: 3,
		ScratchDir: k.data, Now: t0.AddDate(0, 0, 4), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err == nil {
		t.Fatal("overwrote an existing backup")
	}
	// keep 0 would prune every backup, the new one included: refused before
	// anything is written.
	_, err = Run(ctx, k.st, Options{TargetDir: k.dst, Recipient: k.id.Recipient().String(), Keep: 0,
		ScratchDir: k.data, Now: t0.AddDate(0, 0, 5), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil || !strings.Contains(err.Error(), "backup.keep is 0") {
		t.Fatalf("keep 0: %v", err)
	}
	if got := names(t, k.dst); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("keep 0 changed the target: %v; want %v", got, want)
	}
}

func encryptFile(t *testing.T, src, dst string, r age.Recipient) {
	t.Helper()
	plain, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSessionStoreNotBackedUp: the WhatsApp session store (whatsmeow.db, the
// bot's device keys) beside groupwarden.db never goes into a backup.
func TestSessionStoreNotBackedUp(t *testing.T) {
	k := newKit(t)
	ctx := context.Background()
	const key = "device-key-marker-55a1"
	db := sessiontest.Create(t, filepath.Join(k.data, "whatsmeow.db"))
	sessiontest.Exec(t, db, `INSERT INTO whatsmeow_contacts (our_jid, their_jid, push_name) VALUES ('a', 'b', ?)`, key)
	res := k.backup(t0, 14)
	if got := names(t, k.dst); len(got) != 1 {
		t.Fatalf("the backup directory holds %v; want one backup", got)
	}
	plain := filepath.Join(t.TempDir(), "groupwarden.db")
	if err := Restore(ctx, res.File, k.keyFile, plain); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(key)) || bytes.Contains(raw, []byte("whatsmeow_")) {
		t.Fatal("the backup holds the WhatsApp session store")
	}
	// The premise: the restored file is groupwarden.db itself.
	rdb, err := sql.Open("sqlite", "file:"+plain+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()
	var n int
	if err := rdb.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'pause'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the restored file is not groupwarden.db (pause table: %d, %v)", n, err)
	}
}

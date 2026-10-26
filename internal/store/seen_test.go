package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestSeenSenderMigration: migration 12 gives every seen row a sender, empty
// on the rows written before it, so PostsBy lists only the posts decided
// after it, and never one a delete is queued or sent for.
func TestSeenSenderMigration(t *testing.T) {
	ctx := context.Background()
	const (
		group  = "99999000000111@g.us" // synthetic IDs, listed in .secret-pii-allowlist
		sender = "99999000000444@lid"
		phone  = "447700900123@s.whatsapp.net"
	)
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "g.db")
	all := migrations
	migrations = all[:11]
	s, err := Open(ctx, path, Options{Now: func() time.Time { return at }})
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO seen (dedupe_key, chat, msg_id, server_time, decided_at)
VALUES ('msg|old', ?, 'OLD', ?, ?)`, group, at.UnixMilli(), at.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	posts := func() []Post {
		t.Helper()
		var out []Post
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			var err error
			out, err = PostsBy(ctx, tx, []string{sender, ""}, []string{group})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := posts(); len(got) != 0 {
		t.Fatalf("posts before any decision after the migration: %+v, want none (the old row has no sender)", got)
	}
	for _, id := range []string{"NEW", "GONE"} {
		if _, err := s.InboxPut(ctx, "msg|"+id, "message", []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.InboxOldest(ctx, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("inbox %+v %v", rows, err)
	}
	for i, id := range []string{"NEW", "GONE"} {
		if err := s.Decide(ctx, rows[i], Seen{Chat: group, MsgID: id, ServerTime: at, Sender: sender, SenderAlt: phone},
			func(*sql.Tx) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	// A delete already queued for GONE (under the member's other address):
	// it is not listed again.
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, _, err := InsertLedger(ctx, tx, LedgerRow{Action: ActRevoke, Chat: group, Target: phone,
			TriggerID: "test", Community: group, Mode: ModeEnforce, ConfigHash: "x", MsgID: "GONE"}, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := posts()
	if len(got) != 1 || got[0] != (Post{Chat: group, MsgID: "NEW", Sender: sender, ServerTime: time.UnixMilli(at.UnixMilli())}) {
		t.Fatalf("posts %+v, want the one decided after the migration that no delete is queued for", got)
	}
	// SeenBy (`member show`) lists every post of theirs on record, a delete
	// queued or not, by either address; never the old row with no sender.
	seenBy := func(id string) []string {
		t.Helper()
		got, err := s.SeenBy(ctx, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, p := range got {
			ids = append(ids, p.MsgID)
		}
		return ids
	}
	for _, id := range []string{sender, phone} {
		if got := seenBy(id); len(got) != 2 || got[0] != "GONE" || got[1] != "NEW" {
			t.Fatalf("SeenBy(%s) = %v, want GONE and NEW", id, got)
		}
	}
	// member forget leaves the delivery record but drops who sent it.
	if _, err := s.ForgetMember(ctx, []string{sender}); err != nil {
		t.Fatal(err)
	}
	if got := posts(); len(got) != 0 {
		t.Fatalf("posts after member forget: %+v, want none", got)
	}
	if got := seenBy(phone); len(got) != 0 {
		t.Fatalf("SeenBy after member forget: %v, want none", got)
	}
}

package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/store/sessiontest"
)

func openAt(t *testing.T, now *time.Time) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "g.db"), Options{Now: func() time.Time { return *now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestOutboxPurged: an outbox row lives only while its action is pending: the
// final status drops it, and a purge clears any left behind.
func TestOutboxPurged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := openAt(t, &now)
	var ids []int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		for _, chat := range []string{"99999000000111@g.us", "99999000000222@g.us"} {
			id, _, err := InsertLedger(ctx, tx, LedgerRow{Action: ActRemove, Chat: chat, Target: "99999000000444@lid",
				TriggerID: "M1", Community: "c", Mode: ModeEnforce, ConfigHash: "h"}, now)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		// A shadow row is never queued.
		_, _, err := InsertLedger(ctx, tx, LedgerRow{Action: ActRemove, Chat: "99999000000888@g.us", Target: "99999000000444@lid",
			TriggerID: "M1", Community: "c", Mode: ModeShadow, ConfigHash: "h"}, now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.OutboxLen(ctx); n != 2 {
		t.Fatalf("outbox %d, want 2", n)
	}
	if settled, err := s.Finish(ctx, ids[0], Requested, "", 0, now); err != nil || !settled {
		t.Fatalf("finish: settled %v, %v", settled, err)
	}
	if n, _ := s.OutboxLen(ctx); n != 1 {
		t.Fatalf("outbox %d after a final status, want 1", n)
	}
	// A row settled without its outbox row going (written by an older binary,
	// say) is cleared by the purge.
	if _, err := s.db.ExecContext(ctx, `UPDATE ledger SET status = 'failed' WHERE id = ?`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PurgeOutbox(ctx); err != nil || n != 1 {
		t.Fatalf("purged %d (%v), want 1", n, err)
	}
	if n, _ := s.OutboxLen(ctx); n != 0 {
		t.Fatalf("outbox %d after the purge", n)
	}
}

func secret(t *testing.T, db *sql.DB, chat, id string) {
	t.Helper()
	sessiontest.Exec(t, db, `INSERT INTO whatsmeow_message_secrets (our_jid, chat_jid, sender_jid, message_id, key)
VALUES ('447700900789@s.whatsapp.net', ?, '99999000000555@lid', ?, x'00')`, chat, id)
}

func secretIDs(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT message_id FROM whatsmeow_message_secrets`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	return out
}

const announcement = "99999000000333@g.us"

// purgeAt runs the purge as the Purger does: secrets older than evidence_days
// (30) go, announcement-group secrets after announcement_secret_days (90).
func purgeAt(t *testing.T, sess *Session, now time.Time) {
	t.Helper()
	if _, err := sess.PurgeSecrets(context.Background(), now.Add(-30*24*time.Hour), now.Add(-90*24*time.Hour),
		[]string{announcement}); err != nil {
		t.Fatal(err)
	}
}

// TestMessageSecretsPurge: a message secret is purged once it is older than
// retention.evidence_days, aged from when groupwarden first saw it.
func TestMessageSecretsPurge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whatsmeow.db")
	db := sessiontest.Create(t, path)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	sess, err := OpenSession(context.Background(), path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	secret(t, db, "99999000000111@g.us", "OLD")
	purgeAt(t, sess, now) // first seen now
	now = now.Add(20 * 24 * time.Hour)
	secret(t, db, "99999000000111@g.us", "NEW")
	purgeAt(t, sess, now)
	now = now.Add(11 * 24 * time.Hour) // OLD seen 31 days ago, NEW 11
	purgeAt(t, sess, now)
	got := secretIDs(t, db)
	if got["OLD"] || !got["NEW"] {
		t.Fatalf("secrets left %v, want only NEW", got)
	}
	// The first-seen record of a purged secret goes with it.
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM groupwarden_secret_seen`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("first-seen rows %d (%v), want 1", n, err)
	}
}

// TestAnnouncementSecretKeptLonger: an announcement group's message secrets
// are kept for retention.announcement_secret_days, so replies to older
// announcements still decrypt.
func TestAnnouncementSecretKeptLonger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whatsmeow.db")
	db := sessiontest.Create(t, path)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	sess, err := OpenSession(context.Background(), path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	secret(t, db, announcement, "ANN-OLD")
	secret(t, db, "99999000000111@g.us", "CHAT")
	purgeAt(t, sess, now)
	now = now.Add(3 * 24 * time.Hour)
	secret(t, db, announcement, "ANN-NEW")
	purgeAt(t, sess, now)
	now = now.Add(86 * 24 * time.Hour) // ANN-OLD and CHAT 89 days, ANN-NEW 86
	purgeAt(t, sess, now)
	if got := secretIDs(t, db); !got["ANN-OLD"] || !got["ANN-NEW"] || got["CHAT"] {
		t.Fatalf("at 89 days: %v, want both announcement secrets kept and the chat secret gone", got)
	}
	now = now.Add(2 * 24 * time.Hour) // ANN-OLD 91 days, ANN-NEW 88
	purgeAt(t, sess, now)
	if got := secretIDs(t, db); got["ANN-OLD"] || !got["ANN-NEW"] {
		t.Fatalf("at 91 days: %v, want only ANN-NEW", got)
	}
}

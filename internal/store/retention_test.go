package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
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

// TestSecretPurgePlans: the purge holds whatsmeow.db's write lock, during
// which the library cannot decrypt incoming messages, so none of its
// statements may scan a table once per row of another (a correlated subquery
// that scans). That shape made the purge quadratic: 20,000 secrets held the
// lock for 102s here, past the 10s busy timeout. The checker must flag that
// old statement, so it is known to see a per-row scan.
func TestSecretPurgePlans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "whatsmeow.db")
	sessiontest.Create(t, path)
	sess, err := OpenSession(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	perRowScans := func(query string, args ...any) []string {
		t.Helper()
		rows, err := sess.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		detail, parent := map[int]string{}, map[int]int{}
		var order []int
		for rows.Next() {
			var id, par, unused int
			var d string
			if err := rows.Scan(&id, &par, &unused, &d); err != nil {
				t.Fatal(err)
			}
			detail[id], parent[id] = d, par
			order = append(order, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(order) == 0 {
			t.Fatalf("no query plan for %s", query)
		}
		var bad []string
		for _, id := range order {
			if !strings.HasPrefix(detail[id], "SCAN ") {
				continue
			}
			// The nearest enclosing subquery decides: a scan in a correlated
			// one runs once per row of the outer query.
			for p := parent[id]; p != 0; p = parent[p] {
				if strings.Contains(detail[p], "SUBQUERY") {
					if strings.HasPrefix(detail[p], "CORRELATED ") {
						bad = append(bad, detail[p]+" > "+detail[id])
					}
					break
				}
			}
		}
		return bad
	}
	ages := []any{`["99999000000222@g.us"]`, int64(1), int64(2)}
	for name, c := range map[string]struct {
		query string
		args  []any
	}{"stampSecrets": {stampSecrets, []any{int64(1)}}, "purgeSecrets": {purgeSecrets, ages},
		"purgeStamps": {purgeStamps, ages}} {
		if bad := perRowScans(c.query, c.args...); len(bad) != 0 {
			t.Errorf("%s scans a table once per row: %v", name, bad)
		}
	}
	old := `DELETE FROM groupwarden_secret_seen WHERE NOT EXISTS (
	SELECT 1 FROM whatsmeow_message_secrets m WHERE m.chat_jid = groupwarden_secret_seen.chat_jid
		AND m.sender_jid = groupwarden_secret_seen.sender_jid AND m.message_id = groupwarden_secret_seen.message_id)`
	if bad := perRowScans(old); len(bad) == 0 {
		t.Fatal("the checker did not flag the old per-row scan of the library's message secrets")
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

// TestStripsDuePerCommunity: a posted message is due for text removal once
// its report is older than its own community's evidence window (the default
// for any other), chosen in the query; a stripped message keeps no copy.
func TestStripsDuePerCommunity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := openAt(t, &now)
	const short, long = "99999000000777@g.us", "99999000000888@g.us"
	for i, community := range []string{short, long, "", short} {
		id, err := s.AddReport(ctx, Report{Kind: "action", Text: "r", Community: community}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AddTGMessage(ctx, TGMessage{ReportID: id, Role: RoleReport, ChatID: -1, MessageID: i + 1,
			Stripped: "header (message text removed)", SentAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(10 * 24 * time.Hour)
	ago := func(days int) time.Time { return now.AddDate(0, 0, -days) }
	cutoffs := map[string]time.Time{short: ago(7), long: ago(30)}
	due, err := s.StripsDue(ctx, ago(30), cutoffs, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[0].MessageID != 1 || due[1].MessageID != 4 {
		t.Fatalf("due %+v, want messages 1 and 4 (the 7-day community)", due)
	}
	if due, err = s.StripsDue(ctx, ago(30), cutoffs, 1); err != nil || len(due) != 1 {
		t.Fatalf("limit 1: %d due, %v", len(due), err)
	}
	if due, err = s.StripsDue(ctx, ago(5), nil, 10); err != nil || len(due) != 4 {
		t.Fatalf("no overrides, 5-day default: %d due, %v", len(due), err)
	}
	if err := s.MarkTGStripped(ctx, due[0].ID); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.TGMessagesFor(ctx, due[0].ReportID)
	if err != nil || len(msgs) != 1 || msgs[0].Stripped != "" || msgs[0].StrippedAt.IsZero() {
		t.Fatalf("after the strip %+v (%v), want no stripped copy kept", msgs, err)
	}
	if again, _ := s.StripsDue(ctx, ago(5), nil, 10); len(again) != 3 {
		t.Fatalf("%d due after one strip, want 3", len(again))
	}
}

// TestReportPurgeTakesPressesAndUnlinkedPosts: purging a report also deletes
// the button presses on it (the admin's Telegram ID and name) and its posts;
// posts tied to no report (summaries, plain replies) go once older than the
// cutoff. A newer report keeps its press and posts.
func TestReportPurgeTakesPressesAndUnlinkedPosts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	s := openAt(t, &now)
	post := func(report int64, role string, msg int) {
		t.Helper()
		if err := s.AddTGMessage(ctx, TGMessage{ReportID: report, Role: role, ChatID: -1, MessageID: msg, SentAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	report := func(msg int) int64 {
		t.Helper()
		id, err := s.AddReport(ctx, Report{Kind: "action", Text: "r"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.MarkReportSent(ctx, id); err != nil {
			t.Fatal(err)
		}
		post(id, RoleReport, msg)
		if _, _, err := s.ClaimPress(ctx, Press{ReportID: id, Button: "Undo", UserID: 501, UserName: "Ann"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	old := report(1)
	post(0, RoleSummary, 2)
	post(0, RoleReply, 3)
	now = now.AddDate(0, 7, 0)
	cutoff := now
	now = now.AddDate(0, 0, 1)
	fresh := report(4)
	post(0, RoleReply, 5)
	n, err := s.PurgeReports(ctx, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("purged %d reports (%v), want 1", n, err)
	}
	ids := func(query string) []int64 {
		t.Helper()
		rows, err := s.db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}
	if got := ids(`SELECT report_id FROM tg_presses`); len(got) != 1 || got[0] != fresh {
		t.Errorf("presses left for reports %v, want only %d (not %d)", got, fresh, old)
	}
	if got := ids(`SELECT message_id FROM tg_messages ORDER BY message_id`); len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Errorf("posts left %v, want 4 (the newer report) and 5 (the newer reply)", got)
	}
}

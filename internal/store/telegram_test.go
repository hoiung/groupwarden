package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestEditQueuedAgainWhileAppliedStays: an edit queued again after the
// delivery loop read it (an attachment upload landing for a member already
// forgotten) is still queued once the loop drops the edit it applied; and a
// post recorded as taken down keeps the time it first went.
func TestEditQueuedAgainWhileAppliedStays(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := openAt(t, &now)
	id, err := s.AddReport(ctx, Report{Kind: "action", Text: "r"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	queue := func() {
		t.Helper()
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			return QueueReportEdits(ctx, tx, []int64{id}, []string{EditDeleteAttachment}, s.now())
		}); err != nil {
			t.Fatal(err)
		}
	}
	queue()
	read, err := s.ReportEdits(ctx)
	if err != nil || len(read) != 1 {
		t.Fatalf("edits %+v (%v)", read, err)
	}
	queue() // queued again before the loop dropped the one it read
	if err := s.DeleteReportEdit(ctx, read[0].ID); err != nil {
		t.Fatal(err)
	}
	left, err := s.ReportEdits(ctx)
	if err != nil || len(left) != 1 || left[0].ReportID != id || left[0].Op != EditDeleteAttachment {
		t.Fatalf("edits left %+v (%v), want the one queued again", left, err)
	}
	if err := s.DeleteReportEdit(ctx, left[0].ID); err != nil {
		t.Fatal(err)
	}
	if left, err := s.ReportEdits(ctx); err != nil || len(left) != 0 {
		t.Fatalf("edits left %+v (%v), want none", left, err)
	}

	if err := s.AddTGMessage(ctx, TGMessage{ReportID: id, Role: RoleAttachment, ChatID: -1, MessageID: 9, SentAt: now}); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.TGMessagesFor(ctx, id)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("messages %+v (%v)", msgs, err)
	}
	first := now
	if err := s.MarkTGGone(ctx, msgs[0].ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := s.MarkTGGone(ctx, msgs[0].ID); err != nil {
		t.Fatal(err)
	}
	m, ok, err := s.TGMessage(ctx, msgs[0].ID)
	if err != nil || !ok || !m.GoneAt.Equal(first) {
		t.Fatalf("message %+v (%v, %v), want gone at %v", m, ok, err, first)
	}
	if _, ok, err := s.TGMessage(ctx, msgs[0].ID+1); err != nil || ok {
		t.Fatalf("a message not there: (%v, %v), want not ok", ok, err)
	}
}

// TestReportOfTGMessage: a report's post names its report; a summary (no
// report) and an unknown message are "not a report"; a failed read is an
// error, never "not a report".
func TestReportOfTGMessage(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t, Options{})
	id, err := s.AddReport(ctx, Report{Kind: "action", Text: "r"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, m := range []TGMessage{{ReportID: id, Role: RoleReport, ChatID: -1, MessageID: 1, SentAt: now},
		{Role: RoleSummary, ChatID: -1, MessageID: 2, SentAt: now}} {
		if err := s.AddTGMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		msg    int
		wantID int64
		wantOK bool
	}{{1, id, true}, {2, 0, false}, {3, 0, false}} {
		got, ok, err := s.ReportOfTGMessage(ctx, -1, c.msg)
		if err != nil || got != c.wantID || ok != c.wantOK {
			t.Errorf("message %d: (%d, %v, %v), want (%d, %v, nil)", c.msg, got, ok, err, c.wantID, c.wantOK)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if got, ok, err := s.ReportOfTGMessage(cancelled, -1, 1); err == nil || ok || got != 0 {
		t.Fatalf("a failed read: (%d, %v, %v), want an error", got, ok, err)
	}
}

// TestAttachmentOwnerRecordedOnce: a report owns its post's attachment only
// when its writer asks and it is the first report of that evidence copy, and
// the flag stays as written: purging the owner never makes a later report
// about the same post (an action not done) the owner, which would post the
// attachment again.
func TestAttachmentOwnerRecordedOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := openAt(t, &now)
	var evID int64
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		evID, err = InsertEvidence(ctx, tx, Evidence{Chat: "g", MsgID: "M1", Fields: "[]", Normalised: "[]",
			MediaKind: "image", MediaState: MediaPending}, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	add := func(r Report) Report {
		t.Helper()
		id, err := s.AddReport(ctx, r, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.MarkReportSent(ctx, id, ""); err != nil {
			t.Fatal(err)
		}
		got, ok, err := s.Report(ctx, id)
		if err != nil || !ok {
			t.Fatalf("report %d: %v %v", id, ok, err)
		}
		return got
	}
	first := add(Report{Kind: "action", EvidenceID: evID, OwnsAttachment: true})
	now = now.Add(time.Hour)
	second := add(Report{Kind: "action", EvidenceID: evID, OwnsAttachment: true})
	later := add(Report{Kind: "not_done", EvidenceID: evID})
	noCopy := add(Report{Kind: "alert", OwnsAttachment: true})
	if !first.OwnsAttachment || second.OwnsAttachment || later.OwnsAttachment || noCopy.OwnsAttachment {
		t.Fatalf("owners: first %v, second asked %v, later %v, no evidence %v; want only the first",
			first.OwnsAttachment, second.OwnsAttachment, later.OwnsAttachment, noCopy.OwnsAttachment)
	}
	if n, err := s.PurgeReports(ctx, first.CreatedAt.Add(time.Millisecond)); err != nil || n != 1 {
		t.Fatalf("purged %d (%v), want the first report only", n, err)
	}
	for _, id := range []int64{second.ID, later.ID} {
		if r, ok, err := s.Report(ctx, id); err != nil || !ok || r.OwnsAttachment {
			t.Fatalf("report %d after the owner was purged: %+v (%v %v), want it still not the owner", id, r, ok, err)
		}
	}
}

// TestOwnerMigrationMarksTheFirstReport: migration 10 on a database written
// before it marks the first report of each evidence copy as its owner,
// except a log rule's (only counted in the daily summary), and no other.
func TestOwnerMigrationMarksTheFirstReport(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "g.db")
	all := migrations
	migrations = all[:9]
	s, err := Open(ctx, path, Options{})
	migrations = all
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO evidence (id, chat, community, sender, sender_alt, subject, push_name, msg_id, target_id, msg_time,
	fields, normalised, rule, config_hash, created_at) VALUES (1, 'g', '', '', '', '', '', 'M1', '', 0, '[]', '[]', '', '', 0),
	(2, 'g', '', '', '', '', '', 'M2', '', 0, '[]', '[]', '', '', 0)`,
		`INSERT INTO reports (id, kind, priority, community, subject, evidence_id, text, buttons, action, created_at) VALUES
	(1, 'action', 0, '', '', 1, '', '', '', 0), (2, 'not_done', 1, '', '', 1, '', '', '', 0),
	(3, 'log', 0, '', '', 2, '', '', '', 0), (4, 'alert', 0, '', '', NULL, '', '', '', 0)`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for id, owns := range map[int64]bool{1: true, 2: false, 3: false, 4: false} {
		r, ok, err := s.Report(ctx, id)
		if err != nil || !ok || r.OwnsAttachment != owns {
			t.Fatalf("report %d after migration 10: %+v (%v %v), want owns %v", id, r, ok, err, owns)
		}
	}
}

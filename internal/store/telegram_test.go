package store

import (
	"context"
	"database/sql"
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

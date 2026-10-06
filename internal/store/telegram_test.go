package store

import (
	"context"
	"testing"
	"time"
)

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

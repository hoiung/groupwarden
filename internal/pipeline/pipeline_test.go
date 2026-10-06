package pipeline

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config/configtest"
	"github.com/hoiung/groupwarden/internal/store"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

type recorder struct {
	mu    sync.Mutex
	items []Item
	fail  error
}

func (r *recorder) Decide(ctx context.Context, tx *sql.Tx, item Item) error {
	if err := store.IncrCounter(ctx, tx, "2026-10-06", "decided"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.items = append(r.items, item)
	return nil
}

func (r *recorder) got() []Item {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Item(nil), r.items...)
}

func open(t *testing.T, path string, now time.Time) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), path, store.Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func worker(s *store.Store, d Decider) *Worker {
	return &Worker{Store: s, Inbox: NewInbox(s), Decider: d, Config: configtest.Static(""),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func msg(id, text string, sent time.Time) *client.Message {
	return &client.Message{Chat: "99999000000111@g.us", Sender: "99999000000444@lid", ID: id, TargetID: id, Time: sent,
		Fields: []client.Field{{Name: "body", Text: text, Match: text}}}
}

func TestDuplicateDeliveryDeduped(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "g.db"), t0)
	rec := &recorder{}
	w := worker(s, rec)
	m := msg("ID1", "hello", t0)
	for i := 0; i < 3; i++ {
		if err := w.Inbox.Persist(m); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.InboxLen(ctx); n != 1 {
		t.Fatalf("inbox holds %d rows after 3 deliveries, want 1", n)
	}
	if err := w.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	// A redelivery after the decision is ignored too.
	if err := w.Inbox.Persist(m); err != nil {
		t.Fatal(err)
	}
	if err := w.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.got(); len(got) != 1 || got[0].Event.(*client.Message).ID != "ID1" {
		t.Fatalf("decided %d times, want once: %+v", len(got), got)
	}
	// A different message is not a duplicate.
	if err := w.Inbox.Persist(msg("ID2", "hello", t0)); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.InboxLen(ctx); n != 1 {
		t.Fatalf("second message not queued: %d rows", n)
	}
}

func TestInboxRowGoneAfterDecision(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "g.db")
	s := open(t, path, t0)
	rec := &recorder{fail: errors.New("decision failed")}
	w := worker(s, rec)
	const memberText = "MEMBER-TEXT-7f3a9c"
	if err := w.Inbox.Persist(msg("ID1", "buy now "+memberText, t0)); err != nil {
		t.Fatal(err)
	}
	// A decision that fails leaves the row queued and records nothing.
	if err := w.Drain(ctx); err == nil {
		t.Fatal("drain hid the decision failure")
	}
	if n, _ := s.InboxLen(ctx); n != 1 {
		t.Fatalf("failed decision removed the row (%d rows)", n)
	}
	if n, _ := s.Counter(ctx, "2026-10-06", "decided"); n != 0 {
		t.Fatalf("failed decision still wrote its record (%d)", n)
	}
	// A decision that succeeds removes the row in the same transaction.
	rec.fail = nil
	if err := w.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.InboxLen(ctx); n != 0 {
		t.Fatalf("inbox holds %d rows after the decision", n)
	}
	if n, _ := s.Counter(ctx, "2026-10-06", "decided"); n != 1 {
		t.Fatalf("decision record count %d, want 1", n)
	}
	// No copy of the member's text survives on disk.
	if err := s.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(memberText)) {
			t.Fatalf("%s still contains the decided message's text", filepath.Base(f))
		}
	}
}

func TestInboxRedrainedAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "g.db")
	s1, err := store.Open(ctx, path, store.Options{Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	in := NewInbox(s1)
	for _, id := range []string{"A", "B", "C"} {
		if err := in.Persist(msg(id, "x", t0)); err != nil {
			t.Fatal(err)
		}
	}
	_ = s1.Close() // the process dies before the worker ran

	s2 := open(t, path, t0)
	rec := &recorder{}
	ctx2, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker(s2, rec).Run(ctx2) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.got()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := rec.got()
	if len(got) != 3 {
		t.Fatalf("redrained %d rows, want 3", len(got))
	}
	for i, id := range []string{"A", "B", "C"} {
		if m := got[i].Event.(*client.Message); m.ID != id {
			t.Fatalf("row %d = %s, want %s (oldest first)", i, m.ID, id)
		}
	}
}

func TestReplayOlderThanWindowReportOnly(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "g.db"), t0)
	rec := &recorder{}
	w := worker(s, rec)

	// An original message, already decided, sent 47h30m ago.
	if err := w.Inbox.Persist(msg("ORIG", "clean", t0.Add(-47*time.Hour-30*time.Minute))); err != nil {
		t.Fatal(err)
	}
	edit := func(id, target string, at time.Time) *client.Message {
		m := msg(id, "spam", at)
		m.TargetID, m.IsEdit = target, true
		return m
	}
	cases := []struct {
		ev   *client.Message
		want bool
	}{
		{msg("OLD", "x", t0.Add(-48*time.Hour)), true},
		{msg("YOUNG", "x", t0.Add(-46*time.Hour)), false},
		{msg("EDGE", "x", t0.Add(-47*time.Hour)), false},
		// An edit is aged by its original's server time, not its own.
		{edit("E1", "ORIG", t0.Add(-1*time.Hour)), true},
		// An edit whose original is unknown is aged as if sent 15 minutes earlier.
		{edit("E2", "UNKNOWN", t0.Add(-46*time.Hour-50*time.Minute)), true},
		{edit("E3", "UNKNOWN2", t0.Add(-46*time.Hour)), false},
	}
	for _, c := range cases {
		if err := w.Inbox.Persist(c.ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	got := rec.got()
	if len(got) != len(cases)+1 {
		t.Fatalf("decided %d items", len(got))
	}
	for i, c := range cases {
		item := got[i+1]
		if item.Event.(*client.Message).ID != c.ev.ID || item.ReportOnly != c.want {
			t.Errorf("%s: ReportOnly = %v, want %v", c.ev.ID, item.ReportOnly, c.want)
		}
	}
}

// TestWorkerWaitsForGroupList: the worker decides nothing until the group
// directory has loaded once (a message matched against an empty directory
// would count as unmoderated and be dropped); the rows wait in the inbox and
// are decided as soon as it loads.
func TestWorkerWaitsForGroupList(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := open(t, filepath.Join(t.TempDir(), "g.db"), t0)
	rec := &recorder{}
	w := worker(s, rec)
	d := &Directory{}
	w.Ready = d.Loaded()
	if err := w.Inbox.Persist(msg("ID1", "hello", t0)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	if n, _ := s.InboxLen(ctx); n != 1 || len(rec.got()) != 0 || d.IsLoaded() {
		t.Fatalf("decided before the group list loaded: inbox %d, decided %d", n, len(rec.got()))
	}
	d.Update(nil)
	deadline := time.Now().Add(10 * time.Second)
	for len(rec.got()) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("not decided after the group list loaded")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !d.IsLoaded() {
		t.Fatal("IsLoaded false after Update")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A worker stopped while it waits returns at once.
	w2 := worker(s, rec)
	w2.Ready = (&Directory{}).Loaded()
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := w2.Run(ctx2); err != nil {
		t.Fatal(err)
	}
}

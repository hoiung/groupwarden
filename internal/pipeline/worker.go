package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/store"
)

// Worker timing. WhatsApp allows an edit only within 15 minutes of the
// original, so an edit whose original is unknown is aged as if the original
// were that much older.
const (
	pollEvery     = 30 * time.Second
	batchSize     = 100
	editWindow    = 15 * time.Minute
	seenRetention = 72 * time.Hour // longer than the 47h replay window
	purgeEvery    = time.Hour
)

// Item is one inbox event handed to the Decider.
type Item struct {
	Event      client.Event
	ReceivedAt time.Time
	// ReportOnly: the message is older than act_on_replay_max_age by its
	// server time (an offline replay), so it may only be reported.
	ReportOnly bool
}

// Decider records what to do about an item inside the transaction that also
// deletes its inbox row. It must not touch the network.
type Decider interface {
	Decide(ctx context.Context, tx *sql.Tx, item Item) error
}

// Worker drains the inbox.
type Worker struct {
	Store   *store.Store
	Inbox   *Inbox
	Decider Decider
	// Config supplies act_on_replay_max_age (read per message, so a reload
	// applies at once).
	Config *config.Holder
	Log    *slog.Logger

	lastPurge time.Time
}

// Run drains whatever is already queued (events left by a crash or restart),
// then keeps draining as the handler persists more, until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	for {
		if err := w.Drain(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.Log.Error("inbox drain failed; retrying", "err", mask.IDs(err.Error()))
		}
		w.purge(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-w.Inbox.wake:
		case <-time.After(pollEvery):
		}
	}
}

// Drain decides every queued row, oldest first.
func (w *Worker) Drain(ctx context.Context) error {
	for {
		rows, err := w.Store.InboxOldest(ctx, batchSize)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			if err := w.decide(ctx, row); err != nil {
				return err
			}
		}
	}
}

func (w *Worker) decide(ctx context.Context, row store.InboxRow) error {
	ev, err := decode(row.Kind, row.Payload)
	if err != nil {
		// A row this binary cannot read would block the queue forever: count
		// it, log it loudly and remove it.
		w.Log.Error("dropping unreadable inbox row", "row", row.ID, "kind", row.Kind, "err", err)
		return w.Store.Decide(ctx, row, store.Seen{}, func(tx *sql.Tx) error {
			return store.IncrCounter(ctx, tx, day(w.Store.Now()), "inbox_unreadable")
		})
	}
	reportOnly, err := w.reportOnly(ctx, ev)
	if err != nil {
		return err
	}
	item := Item{Event: ev, ReceivedAt: row.ReceivedAt, ReportOnly: reportOnly}
	if err := w.Store.Decide(ctx, row, seenOf(ev), func(tx *sql.Tx) error {
		return w.Decider.Decide(ctx, tx, item)
	}); err != nil {
		return fmt.Errorf("decide inbox row %d: %w", row.ID, err)
	}
	return nil
}

// reportOnly ages a message by the server time of the message an action would
// target: the original, for an edit.
func (w *Worker) reportOnly(ctx context.Context, ev client.Event) (bool, error) {
	m, ok := ev.(*client.Message)
	if !ok {
		return false, nil
	}
	sent := m.Time
	if m.TargetID != m.ID {
		orig, found, err := w.Store.SeenTime(ctx, string(m.Chat), m.TargetID)
		if err != nil {
			return false, err
		}
		if found {
			sent = orig
		} else {
			sent = m.Time.Add(-editWindow)
		}
	}
	maxAge := time.Duration(w.Config.Current().Config.ActOnReplayMaxAge)
	return w.Store.Now().Sub(sent) > maxAge, nil
}

func (w *Worker) purge(ctx context.Context) {
	now := w.Store.Now()
	if now.Sub(w.lastPurge) < purgeEvery {
		return
	}
	w.lastPurge = now
	if _, err := w.Store.PurgeSeen(ctx, now.Add(-seenRetention)); err != nil {
		w.Log.Error("purge seen deliveries", "err", err)
	}
}

func seenOf(ev client.Event) store.Seen {
	switch e := ev.(type) {
	case *client.Message:
		return store.Seen{Chat: string(e.Chat), MsgID: e.ID, ServerTime: e.Time}
	case *client.Undecryptable:
		return store.Seen{Chat: string(e.Chat), MsgID: e.ID, ServerTime: e.Time}
	case *client.GroupChange:
		return store.Seen{Chat: string(e.Group), ServerTime: e.Time}
	case *client.JoinedGroup:
		return store.Seen{Chat: string(e.Group), ServerTime: e.Time}
	}
	return store.Seen{}
}

// day is the UTC date used to bucket daily counters.
func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

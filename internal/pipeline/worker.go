package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
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
	// maxDecideTries is how many times in a row one inbox row's decision may
	// fail before it is set aside, so it cannot hold every row behind it.
	maxDecideTries = 3
)

// Item is one inbox event handed to the Decider.
type Item struct {
	Event      client.Event
	ReceivedAt time.Time
	// ReportOnly: the message is older than act_on_replay_max_age by its
	// server time (an offline replay), so it may only be reported.
	ReportOnly bool
	// TargetTime is the server time of the message an action would target:
	// the original, for an edit (zero for other events).
	TargetTime time.Time
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
	// Ready, when set, holds the first drain until it is closed (the group
	// directory's Loaded): a message decided before the bot knows its groups
	// would be taken for one from an unmoderated group and dropped. The rows
	// wait in the inbox meanwhile.
	Ready <-chan struct{}
	// Wake is called after the worker stored a report itself (nil: no-op).
	Wake func()

	lastPurge time.Time
	// stuckRow is the oldest row whose decision failed; stuckTries counts
	// its failures in a row.
	stuckRow   int64
	stuckTries int
}

// Run drains whatever is already queued (events left by a crash or restart),
// then keeps draining as the handler persists more, until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	if w.Ready != nil {
		select {
		case <-w.Ready:
		default:
			w.Log.Info("inbox worker waiting for the group list before deciding anything")
			select {
			case <-ctx.Done():
				return nil
			case <-w.Ready:
			}
			w.Log.Info("group list loaded; inbox worker starting")
		}
	}
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
			return store.IncrCounter(ctx, tx, day(w.Store.Now()), CounterInboxUnreadable)
		})
	}
	if err := w.decideEvent(ctx, row, ev); err != nil {
		if ctx.Err() != nil {
			return err
		}
		return w.failed(ctx, row, ev, err)
	}
	w.stuckRow, w.stuckTries = 0, 0
	return nil
}

func (w *Worker) decideEvent(ctx context.Context, row store.InboxRow, ev client.Event) error {
	reportOnly, target, err := w.age(ctx, ev)
	if err != nil {
		return err
	}
	item := Item{Event: ev, ReceivedAt: row.ReceivedAt, ReportOnly: reportOnly, TargetTime: target}
	return w.Store.Decide(ctx, row, seenOf(ev), func(tx *sql.Tx) error {
		return w.Decider.Decide(ctx, tx, item)
	})
}

// failed counts a failed decision of row. Once the same row has failed
// maxDecideTries times in a row it is set aside: deleted, marked seen (so a
// redelivery is ignored) and reported as a priority alert, so the rows behind
// it are decided. A store that cannot write fails the setting-aside too, so
// nothing is dropped while the store is down.
func (w *Worker) failed(ctx context.Context, row store.InboxRow, ev client.Event, cause error) error {
	err := fmt.Errorf("decide inbox row %d: %w", row.ID, cause)
	if w.stuckRow != row.ID {
		w.stuckRow, w.stuckTries = row.ID, 0
	}
	w.stuckTries++
	if w.stuckTries < maxDecideTries {
		return err
	}
	seen := seenOf(ev)
	w.Log.Error("setting aside an inbox item whose decision keeps failing", "row", row.ID, "kind", row.Kind,
		"chat", mask.IDs(seen.Chat), "tries", w.stuckTries, "err", mask.IDs(cause.Error()))
	text := fmt.Sprintf("The bot could not decide a %s in %s (tried %d times: %s). It was set aside so the bot "+
		"could carry on with the rest; check that group by hand.", strings.ReplaceAll(row.Kind, "_", " "),
		mask.IDs(seen.Chat), w.stuckTries, mask.IDs(cause.Error()))
	if serr := w.Store.Decide(ctx, row, seen, func(tx *sql.Tx) error {
		_, err := store.InsertReport(ctx, tx, store.Report{Kind: string(alert.Undecided), Priority: true, Text: text},
			nil, w.Store.Now())
		return err
	}); serr != nil {
		return fmt.Errorf("%w; setting it aside failed too: %v", err, serr)
	}
	w.stuckRow, w.stuckTries = 0, 0
	if w.Wake != nil {
		w.Wake()
	}
	return nil
}

// age ages a message by the server time of the message an action would
// target (the original, for an edit) and returns that time.
func (w *Worker) age(ctx context.Context, ev client.Event) (reportOnly bool, target time.Time, err error) {
	m, ok := ev.(*client.Message)
	if !ok {
		return false, time.Time{}, nil
	}
	sent, err := targetSent(ctx, w.Store, string(m.Chat), m.ID, m.TargetID, m.Time)
	if err != nil {
		return false, time.Time{}, err
	}
	maxAge := time.Duration(w.Config.Current().Config.ActOnReplayMaxAge)
	return w.Store.Now().Sub(sent) > maxAge, sent, nil
}

// targetSent is the server time of the message an action on message id (sent
// at t) targets: t itself, or for an edit its original's time as delivered
// (kept seenRetention, longer than any replay window), else editWindow before
// the edit. Both the inbox and [Ban] age a message by it.
func targetSent(ctx context.Context, st *store.Store, chat, id, targetID string, t time.Time) (time.Time, error) {
	if targetID == id {
		return t, nil
	}
	orig, found, err := st.SeenTime(ctx, chat, targetID)
	if err != nil {
		return time.Time{}, err
	}
	if !found {
		return t.Add(-editWindow), nil
	}
	return orig, nil
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

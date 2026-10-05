package pipeline

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/store"
)

// CounterMissingParent counts announcement replies (and secret edits)
// whose parent secret is not stored; it feeds the daily summary.
const CounterMissingParent = "missing_parent_secret"

// Moderator is the Decider used by `run`.
type Moderator struct {
	Store   *store.Store
	Alerter alert.Alerter
	Log     *slog.Logger
}

// Decide records the outcome for one inbox item.
func (m *Moderator) Decide(ctx context.Context, tx *sql.Tx, item Item) error {
	switch ev := item.Event.(type) {
	case *client.Undecryptable:
		if ev.Reason == client.ReasonMissingParentSecret {
			return store.IncrCounter(ctx, tx, day(m.Store.Now()), CounterMissingParent)
		}
		return m.Alerter.Alert(ctx, alert.Alert{
			Kind:     alert.DecryptError,
			Priority: true,
			Text:     "could not decrypt a message in " + mask.IDs(string(ev.Chat)) + ": " + mask.IDs(ev.Detail),
		})
	case *client.Message:
		m.Log.Debug("message", "chat", mask.IDs(string(ev.Chat)), "fields", len(ev.Fields), "edit", ev.IsEdit,
			"comment", ev.IsComment, "report_only", item.ReportOnly)
	case *client.GroupChange:
		m.Log.Debug("group change", "group", mask.IDs(string(ev.Group)), "joined", len(ev.Joined), "left", len(ev.Left),
			"promoted", len(ev.Promoted), "demoted", len(ev.Demoted))
	case *client.JoinedGroup:
		m.Log.Info("bot joined a group", "group", mask.IDs(string(ev.Group)), "reason", ev.Reason)
	}
	return nil
}

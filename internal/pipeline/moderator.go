package pipeline

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/rules"
	"github.com/hoiung/groupwarden/internal/store"
)

// CounterMissingParent counts announcement replies (and secret edits)
// whose parent secret is not stored; it feeds the daily summary.
const CounterMissingParent = "missing_parent_secret"

// Moderator is the Decider used by `run`.
type Moderator struct {
	Store     *store.Store
	Alerter   alert.Alerter
	Config    *config.Holder
	Directory *Directory
	Log       *slog.Logger
}

// Evaluate decides one message against the config in use and returns the
// decision with that config's hash. It reads the config once, so a reload
// in the middle never mixes two configs.
func (m *Moderator) Evaluate(msg *client.Message) (rules.Decision, string, string) {
	cur := m.Config.Current()
	community := m.Directory.Community(msg.Chat, cur.Rules)
	if community == "" {
		return rules.Decision{}, "", cur.Hash
	}
	d := cur.Rules.Decide(rules.Input{
		Community: community, Fields: msg.Fields, PushName: msg.PushName,
		SenderIsAdmin: m.Directory.IsAdmin(msg.Sender, msg.SenderAlt, cur.Rules),
		FromMetaAI:    msg.FromMetaAI,
	})
	return d, community, cur.Hash
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
		d, community, hash := m.Evaluate(ev)
		if community == "" {
			m.Log.Debug("message in a group that is not moderated", "chat", mask.IDs(string(ev.Chat)))
			return nil
		}
		if d.Action == rules.ActionNone {
			m.Log.Debug("message", "chat", mask.IDs(string(ev.Chat)), "lists", d.Lists, "config", "v"+hash)
			return nil
		}
		// Actions arrive with the ledger and outbox; until then each
		// decision is logged with the rule and config that made it.
		m.Log.Info("decision", "action", string(d.Action), "rule", d.Rule, "would_have_acted", d.WouldHaveActed,
			"exempt", string(d.Exempt), "community", mask.IDs(community), "chat", mask.IDs(string(ev.Chat)),
			"sender", mask.IDs(string(ev.Sender)), "edit", ev.IsEdit, "comment", ev.IsComment,
			"report_only", item.ReportOnly, "config", "v"+hash)
	case *client.GroupChange:
		m.Log.Debug("group change", "group", mask.IDs(string(ev.Group)), "joined", len(ev.Joined), "left", len(ev.Left),
			"promoted", len(ev.Promoted), "demoted", len(ev.Demoted))
	case *client.JoinedGroup:
		m.Log.Info("bot joined a group", "group", mask.IDs(string(ev.Group)), "reason", ev.Reason)
	}
	return nil
}

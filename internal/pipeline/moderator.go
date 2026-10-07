package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/normalise"
	"github.com/hoiung/groupwarden/internal/rules"
	"github.com/hoiung/groupwarden/internal/store"
)

// Daily counters (the admin chat's daily summary reads them).
const (
	// CounterMissingParent counts announcement replies (and secret edits)
	// whose parent secret is not stored.
	CounterMissingParent = "missing_parent_secret"
	// CounterInboxUnreadable counts inbox items that could not be decoded.
	CounterInboxUnreadable = "inbox_unreadable"
	// CounterKeywordOnly prefixes "keyword_only|<community>|<word list>": a
	// post with a keyword from that list that no rule acted on.
	CounterKeywordOnly = "keyword_only|"
)

// Moderator is the Decider used by `run`. It turns each decision into a
// ledger plan written in the decision's transaction: nothing fires until the
// rows are committed.
type Moderator struct {
	Store     *store.Store
	Config    *config.Holder
	Directory *Directory
	Enforcer  *Enforcer
	Log       *slog.Logger
}

// Evaluate decides one message against the config in use and returns the
// decision with that config's hash. It reads the config once, so a reload
// in the middle never mixes two configs.
func (m *Moderator) Evaluate(msg *client.Message) (rules.Decision, string, string) {
	return m.evaluate(msg, m.Config.Current())
}

func (m *Moderator) evaluate(msg *client.Message, cur *config.Loaded) (rules.Decision, string, string) {
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
		m.Log.Warn("could not decrypt a message", "chat", mask.IDs(string(ev.Chat)), "reason", ev.Reason,
			"detail", mask.IDs(ev.Detail))
		// Stored with the decision (the store has one connection: nothing
		// may write outside this transaction until it ends).
		_, err := store.InsertReport(ctx, tx, store.Report{Kind: string(alert.DecryptError),
			Text: "Could not decrypt a message in " + mask.IDs(string(ev.Chat)) + ": " + mask.IDs(ev.Detail)}, nil,
			m.Store.Now())
		if err == nil {
			m.Enforcer.wake()
		}
		return err
	case *client.Message:
		return m.decideMessage(ctx, tx, ev, item)
	case *client.GroupChange:
		m.Log.Debug("group change", "group", mask.IDs(string(ev.Group)), "joined", len(ev.Joined), "left", len(ev.Left),
			"promoted", len(ev.Promoted), "demoted", len(ev.Demoted))
		return m.Enforcer.OnGroupChange(ctx, tx, ev, m.Store.Now())
	case *client.JoinedGroup:
		info := ev.Info
		info.JID = ev.Group
		m.Directory.Join(info)
		m.Log.Info("bot joined a group", "group", mask.IDs(string(ev.Group)), "reason", ev.Reason,
			"community", mask.IDs(m.Directory.Community(ev.Group, m.Config.Current().Rules)))
	}
	return nil
}

func (m *Moderator) decideMessage(ctx context.Context, tx *sql.Tx, ev *client.Message, item Item) error {
	cur := m.Config.Current()
	d, community, hash := m.evaluate(ev, cur)
	if community == "" {
		m.Log.Debug("message in a group that is not moderated", "chat", mask.IDs(string(ev.Chat)))
		return nil
	}
	if d.Action == rules.ActionNone {
		m.Log.Debug("message", "chat", mask.IDs(string(ev.Chat)), "lists", d.Lists, "config", "v"+hash)
		// A keyword no rule acted on goes into the daily summary only.
		for _, list := range d.Lists {
			if err := store.IncrCounter(ctx, tx, day(m.Store.Now()), CounterKeywordOnly+community+"|"+list); err != nil {
				return err
			}
		}
		return nil
	}
	m.Log.Info("decision", "action", string(d.Action), "rule", d.Rule, "would_have_acted", d.WouldHaveActed,
		"exempt", string(d.Exempt), "community", mask.IDs(community), "chat", mask.IDs(string(ev.Chat)),
		"sender", mask.IDs(string(ev.Sender)), "edit", ev.IsEdit, "comment", ev.IsComment,
		"report_only", item.ReportOnly, "config", "v"+hash)
	// A pause covering removals and bans holds an enforced ban too: the ban is
	// applied after [Resume] if the post still counts (deletes continue unless
	// every action is paused).
	var paused store.Scope
	if d.Action == rules.DeleteRemoveBan && !item.ReportOnly {
		if all, _ := m.Store.PausedForTx(ctx, tx, store.ScopeAll); all {
			paused = store.ScopeAll
		} else if rb, _ := m.Store.PausedForTx(ctx, tx, store.ScopeRemoveBan); rb {
			paused = store.ScopeRemoveBan
		}
	}
	p, err := m.messagePlan(ev, item, d, community, cur, paused)
	if err != nil {
		return err
	}
	w, err := ledger.Write(ctx, tx, p, m.Store.Now())
	if err != nil {
		return err
	}
	if w.Queued > 0 || len(w.ReportIDs) > 0 {
		m.Enforcer.wake()
	}
	return nil
}

// messagePlan turns a message decision into its ledger plan. An enforced
// match deletes the message (by the original's ID for an edit), removes the
// sender from every group the ban covers and bans them. A watch-only match,
// or a message too old to act on, records the same rows in shadow. An exempt
// sender or a log rule is reported only. paused is the scope of the pause in
// force ("" none): it holds the ban, and its actions wait for [Resume].
func (m *Moderator) messagePlan(ev *client.Message, item Item, d rules.Decision, community string, cur *config.Loaded,
	paused store.Scope) (ledger.Plan, error) {
	member := m.Directory.Complete(client.MemberOf(ev.Sender, ev.SenderAlt))
	evidence, err := m.evidence(ev, community, cur)
	if err != nil {
		return ledger.Plan{}, err
	}
	p := ledger.Plan{Trigger: ev.ID, Target: member, Rule: d.Rule, ConfigHash: cur.Hash, Evidence: evidence}
	where := CommunityLabel(cur.Config, community)
	targetTime := item.TargetTime
	if targetTime.IsZero() {
		targetTime = ev.Time
	}
	act := d.Action == rules.DeleteRemoveBan || d.WouldHaveActed
	enforce := d.Action == rules.DeleteRemoveBan && !item.ReportOnly
	if act && member.Key() == "" {
		// WhatsApp gave the sender an address that is neither a LID nor a
		// phone number: there is no one to remove or ban, so the admins are
		// told instead (a plan with actions and no target fails its write
		// and would hold the inbox).
		m.Log.Warn("rule matched a sender the bot cannot act on; reported only", "rule", d.Rule,
			"chat", mask.IDs(string(ev.Chat)), "sender", mask.IDs(string(ev.Sender)))
		p.Reason = "the sender has no address the bot can act on"
		p.Reports = []store.Report{{Kind: ledger.KindUnaddressable, Priority: true, Community: community,
			Text: fmt.Sprintf("Rule %s matched a post in %s, but the sender's address (%s) is not one the bot can "+
				"remove or ban: reported only. Delete it by hand if it is spam.", d.Rule, where, mask.IDs(string(ev.Sender)))}}
		return p, nil
	}
	if act {
		banIn := d.BanIn
		if len(banIn) == 0 { // a watch-only match: the ban it would have made
			banIn = cur.Rules.BanTargets(community)
		}
		p.Intents = []ledger.Intent{{Action: store.ActRevoke, Chat: ev.Chat, Community: community, Enforce: enforce,
			Address: ev.Sender, MsgID: ev.TargetID, MsgTime: targetTime}}
		removals, ok := m.Enforcer.Removals(member, banIn, ev.Chat, cur.Rules)
		if !ok {
			// An admin's post is never actioned (an enforced match was
			// already turned into an exempt report by the rules).
			p.Intents = nil
			p.Reports = []store.Report{{Kind: ledger.KindExempt, Community: community,
				Text: fmt.Sprintf("Rule %s matched a post in %s by an admin: reported only.", d.Rule, where)}}
			return p, nil
		}
		for _, r := range removals {
			r.Enforce = r.Enforce && enforce
			r.MsgTime = targetTime
			p.Intents = append(p.Intents, r)
		}
		p.Ban, p.BanEnforce, p.BanCommunity = BanScopes(cur.Rules, community), enforce, community
		p.BanHeld = enforce && paused != ""
	}
	switch {
	case enforce:
		p.Reason = "spam post (rule " + d.Rule + ")"
		buttons := []string{ledger.ButtonUndo}
		if evidence.MediaState == store.MediaPending {
			buttons = append(buttons, ledger.ButtonShowAttachment)
		}
		text := fmt.Sprintf("Spam in %s matched rule %s: the post is deleted and the sender removed and banned.", where, d.Rule)
		switch paused {
		case store.ScopeRemoveBan:
			text = fmt.Sprintf("Spam in %s matched rule %s: the post is deleted. Removals and bans are paused, so the "+
				"sender is removed and banned after [Resume] if the config then still acts on this post.", where, d.Rule)
		case store.ScopeAll:
			text = fmt.Sprintf("Spam in %s matched rule %s. Every action is paused, so after [Resume] the post is "+
				"deleted and the sender removed and banned if the config then still acts on this post.", where, d.Rule)
		}
		p.Reports = []store.Report{{Kind: ledger.KindAction, Community: community, Buttons: buttons, Text: text}}
	case act && item.ReportOnly:
		p.Reason = "too old to act on (older than act_on_replay_max_age)"
		p.Reports = []store.Report{{Kind: ledger.KindWouldHaveActed, Community: community,
			Text: fmt.Sprintf("Spam in %s matched rule %s, but the message is older than act_on_replay_max_age: reported only.", where, d.Rule)}}
	case act:
		p.Reason = "watch-only rule (rule " + d.Rule + ")"
		p.Reports = []store.Report{{Kind: ledger.KindWouldHaveActed, Community: community, Buttons: []string{ledger.ButtonBan},
			Text: fmt.Sprintf("Would have acted in %s: rule %s matched but is watch-only.", where, d.Rule)}}
	case d.Exempt != "":
		p.Reports = []store.Report{{Kind: ledger.KindExempt, Community: community,
			Text: fmt.Sprintf("Rule %s matched a post in %s by %s: reported only.", d.Rule, where, exemptWho(d.Exempt))}}
	default:
		p.Reports = []store.Report{{Kind: ledger.KindLog, Community: community,
			Text: fmt.Sprintf("Rule %s matched a post in %s (report only).", d.Rule, where)}}
	}
	return p, nil
}

func exemptWho(e rules.Exemption) string {
	if e == rules.ExemptMetaAI {
		return "Meta AI"
	}
	return "an admin"
}

// evidence builds the evidence copy: every field as sent with its matching
// view, the normalised text, and the attachment's description (downloaded
// later, never delaying the delete; recorded by type, name and size only when
// it declares a size over evidence.max_attachment_mb). A declared size is the
// sender's claim, so a smaller or missing one only means the download is
// tried, and the download itself stops at the limit (action.MediaFetcher).
func (m *Moderator) evidence(ev *client.Message, community string, cur *config.Loaded) (*store.Evidence, error) {
	fields, err := json.Marshal(ev.Fields)
	if err != nil {
		return nil, fmt.Errorf("evidence fields: %w", err)
	}
	norm := make([]string, len(ev.Fields))
	for i, f := range ev.Fields {
		norm[i] = normalise.Base(f.Text)
	}
	normJSON, err := json.Marshal(norm)
	if err != nil {
		return nil, fmt.Errorf("evidence text: %w", err)
	}
	e := &store.Evidence{Chat: string(ev.Chat), Community: community, Sender: string(ev.Sender), SenderAlt: string(ev.SenderAlt),
		PushName: ev.PushName, MsgID: ev.ID, TargetID: ev.TargetID, MsgTime: ev.Time, Fields: string(fields),
		Normalised: string(normJSON)}
	if md := ev.Media; md != nil {
		e.MediaKind, e.MediaMime, e.MediaName, e.MediaSize = md.Kind, md.MimeType, md.FileName, md.Size
		limit := uint64(cur.Config.Evidence.MaxAttachmentMB) << 20 // #nosec G115 -- the schema bounds it to 1..50
		if md.Size > limit {
			e.MediaState = store.MediaTooLarge
		} else {
			e.MediaState, e.MediaRaw = store.MediaPending, md.Raw
		}
	}
	return e, nil
}

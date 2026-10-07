package pipeline

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/rules"
	"github.com/hoiung/groupwarden/internal/store"
)

// Enforcer keeps banned people out: it plans where a removal happens,
// removes a banned person who joins (unless a human admin added them, which
// lifts the ban), rejects their join requests and finds the LID of a ban
// known only by phone number.
type Enforcer struct {
	Store     *store.Store
	Config    *config.Holder
	Directory *Directory
	// Adapter is used only by the sweep-time checks (join requests, LID
	// lookups); deciding an event never touches the network.
	Adapter client.Adapter
	// Wake is called after actions or reports were queued (nil: no-op).
	Wake func()
	Log  *slog.Logger
}

func (e *Enforcer) wake() {
	if e != nil && e.Wake != nil {
		e.Wake()
	}
}

// Notify wakes the workers after something outside the enforcer (the sweep's
// coverage reports) queued reports.
func (e *Enforcer) Notify() { e.wake() }

// Removals plans the removal of m from every group of each community where
// the directory shows them, plus inChat (where they were just seen). Where
// the bot is a community admin it also removes them at community level (the
// AC 1.9 E1 probe says whether that alone reaches every group; the per-group
// removals report already_gone when it did). A community in shadow mode gets
// shadow rows. A current human admin is never planned for removal: ok is
// false and the caller reports it instead.
func (e *Enforcer) Removals(m client.Member, communities []string, inChat client.JID, rs *rules.Ruleset) (intents []ledger.Intent, ok bool) {
	if e.Directory.IsAdmin(m.LID, m.Phone, rs) {
		return nil, false
	}
	for _, c := range communities {
		mode, configured := rs.ModeFor(c)
		if !configured {
			continue
		}
		enforce := mode == rules.Enforce
		seen := map[client.JID]bool{}
		add := func(chat client.JID) {
			if !seen[chat] {
				seen[chat] = true
				intents = append(intents, ledger.Intent{Action: store.ActRemove, Chat: chat, Community: c, Enforce: enforce,
					Address: e.Directory.AddressIn(chat, m)})
			}
		}
		if cj := client.JID(c); e.Directory.IsCommunity(cj) && e.Directory.BotIsAdmin(cj) {
			add(cj)
		}
		if inChat != "" && e.Directory.Community(inChat, rs) == c {
			add(inChat)
		}
		for _, g := range e.Directory.Groups(c, rs) {
			if e.Directory.Present(g, m) {
				add(g)
			}
		}
	}
	return intents, true
}

// BanScopes is where a new ban for a post in community applies: the ban
// list scope of each ban row.
func BanScopes(rs *rules.Ruleset, community string) []string {
	if rs.BanScope() == rules.PerCommunity {
		return []string{community}
	}
	return []string{store.BanEverywhere}
}

// eventID is a short stable ID for an event that has no ID of its own.
func eventID(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return prefix + hex.EncodeToString(sum[:8])
}

// OnGroupChange records what a membership change means, inside the decision
// transaction: a banned person who joined is removed (or, when a current
// human admin added them, their ban in that community is lifted); a human
// admin removing someone offers [Add to ban list] [No]; the directory learns
// the change.
func (e *Enforcer) OnGroupChange(ctx context.Context, tx *sql.Tx, ch *client.GroupChange, now time.Time) error {
	cur := e.Config.Current()
	rs := cur.Rules
	community := e.Directory.Community(ch.Group, rs)
	name := e.Directory.GroupName(string(ch.Group))
	// Membership is learned before the bans are checked, so a removal plan
	// sees the person in the group they just joined.
	e.Directory.Apply(ch)
	if community == "" {
		return nil
	}
	where := CommunityLabel(cur.Config, community)
	if err := e.selfChange(ctx, tx, ch, community, name, now); err != nil {
		return err
	}
	trigger := eventID("join:", ch.DedupeKey())
	actor := ch.Actor.Bare()
	humanAdmin := actor != "" && !e.Directory.IsSelf(actor) && e.Directory.IsAdmin(actor, ch.ActorAlt, rs)
	for _, j := range ch.Joined {
		m := e.Directory.Complete(client.MemberOf(j))
		ban, banned, err := store.FindBan(ctx, tx, m.IDs(), community)
		if err != nil {
			return err
		}
		if !banned || e.Directory.IsSelf(j) {
			continue
		}
		if m.LID == "" && ban.LID != "" {
			m.LID = client.JID(ban.LID)
		}
		p := ledger.Plan{Trigger: trigger, Target: m, ConfigHash: cur.Hash, Actor: actor}
		switch {
		case humanAdmin && actor != j.Bare() && client.JID(m.Key()) != actor:
			e.Log.Info("ban lifted: a human admin re-added a banned member", "community", mask.IDs(community),
				"group", mask.IDs(string(ch.Group)), "member", mask.IDs(m.Key()), "actor", mask.IDs(string(actor)))
			p.Lift, p.LiftIn, p.BanCommunity, p.Reason = true, community, community, "ban lifted by human re-add"
			p.Reports = []store.Report{{Kind: ledger.KindBanLifted, Community: community, Text: fmt.Sprintf(
				"A human admin re-added a banned member to a group in %s: the ban is lifted (their next spam post is deleted, removed and banned again).",
				where)}}
		default:
			intents, ok := e.Removals(m, []string{community}, ch.Group, rs)
			if !ok {
				p.Reports = []store.Report{adminSpared(community, where)}
				break
			}
			p.Intents, p.Reason = intents, "banned member joined"
			removed := onceUnpaused(pausedScope(ctx, tx, e.Store), "removed")
			rep := store.Report{Kind: ledger.KindBannedRejoin, Community: community, Buttons: []string{ledger.ButtonUndo},
				Text: fmt.Sprintf("A banned member joined a group in %s (%s): %s.", where, joinHow(ch), removed)}
			switch {
			case !enforced(rs, community):
				rep = store.Report{Kind: ledger.KindWouldRemove, Community: community, Text: fmt.Sprintf(
					"A banned member joined a group in %s (%s): they would be removed, but it is in shadow mode: not done.",
					where, joinHow(ch))}
			case actor == "":
				rep.Kind, rep.Priority = ledger.KindUnknownActor, true
				rep.Text = fmt.Sprintf("A banned member joined a group in %s and WhatsApp did not say who added them: %s. If an admin added them on purpose, press [Undo].", where, removed)
			}
			e.Log.Info("banned member joined; removal planned", "community", mask.IDs(community),
				"group", mask.IDs(string(ch.Group)), "member", mask.IDs(m.Key()), "actor_known", actor != "",
				"enforce", enforced(rs, community))
			p.Reports = []store.Report{rep}
		}
		if _, err := ledger.Write(ctx, tx, p, now); err != nil {
			return err
		}
	}
	if humanAdmin {
		for _, l := range ch.Left {
			if l.Bare() == actor || e.Directory.IsSelf(l) {
				continue
			}
			m := e.Directory.Complete(client.MemberOf(l))
			if m.Key() == "" {
				// No LID or phone number: nobody the ban list could hold.
				e.Log.Warn("a human admin removed an address the bot cannot ban; not offered", "group",
					mask.IDs(string(ch.Group)), "member", mask.IDs(string(l)))
				continue
			}
			if _, banned, err := store.FindBan(ctx, tx, m.IDs(), community); err != nil || banned {
				if err != nil {
					return err
				}
				continue
			}
			e.Log.Info("a human admin removed a member; offering a ban", "community", mask.IDs(community),
				"group", mask.IDs(string(ch.Group)), "member", mask.IDs(m.Key()), "actor", mask.IDs(string(actor)))
			p := ledger.Plan{Trigger: eventID("left:", ch.DedupeKey()+string(l)), Target: m, ConfigHash: cur.Hash, Actor: actor,
				Reports: []store.Report{{Kind: ledger.KindHumanRemoval, Community: community,
					Buttons: []string{ledger.ButtonAddToBanList, ledger.ButtonNo},
					Text:    fmt.Sprintf("A human admin removed a member from a group in %s. Add them to the ban list?", where)}}}
			if _, err := ledger.Write(ctx, tx, p, now); err != nil {
				return err
			}
		}
	}
	e.wake()
	return nil
}

// selfChange raises a priority report when the bot itself was demoted in, or
// removed from, a moderated group: it can no longer act there until a human
// admin promotes or re-adds it. The group's stored coverage changes in the
// same transaction, so the next sweep does not report the loss again.
func (e *Enforcer) selfChange(ctx context.Context, tx *sql.Tx, ch *client.GroupChange, community, name string, now time.Time) error {
	label, cname := Label(name, ch.Group), CommunityLabel(e.Config.Current().Config, community)
	var reps []store.Report
	state := ""
	if slices.ContainsFunc(ch.Demoted, e.Directory.IsSelf) {
		state = store.CoverageNotAdmin
		reps = append(reps, store.Report{Kind: string(alert.BotDemoted), Community: community,
			Text: fmt.Sprintf("The bot is no longer an admin in %s (%s): it cannot delete or remove there "+
				"until a human admin promotes it again.", label, cname)})
	}
	if slices.ContainsFunc(ch.Left, e.Directory.IsSelf) {
		state = store.CoverageAbsent
		reps = append(reps, store.Report{Kind: string(alert.BotRemoved), Community: community,
			Text: fmt.Sprintf("The bot was removed from %s (%s): that group is not moderated until a "+
				"human admin adds the bot back and promotes it.", label, cname)})
	}
	if state != "" {
		if err := store.MarkCoverage(ctx, tx, string(ch.Group), community, state, now); err != nil {
			return err
		}
	}
	for _, r := range reps {
		if _, err := store.InsertReport(ctx, tx, r, nil, now); err != nil {
			return err
		}
		e.Log.Error("the bot lost a moderated group", "kind", r.Kind, "group", label)
	}
	if len(reps) > 0 {
		e.wake()
	}
	return nil
}

// adminSpared is the report about a banned member the bot did not remove
// because they are a current admin; where names the community.
func adminSpared(community, where string) store.Report {
	return store.Report{Kind: ledger.KindAdminSpared, Priority: true, Community: community,
		Text: fmt.Sprintf("A banned member is a current admin in %s, so the bot did not remove them. An admin must decide.", where)}
}

func joinHow(ch *client.GroupChange) string {
	switch {
	case ch.JoinReason == "invite":
		return "by invite link"
	case ch.Actor == "":
		return "added by an unknown actor"
	default:
		return "added by a non-admin or themselves"
	}
}

// CheckPresent queues the removal of every banned member the directory shows
// in group. Sweep retries of the same removal are one row and one report per
// episode.
func (e *Enforcer) CheckPresent(ctx context.Context, group client.JID, runID string) error {
	cur := e.Config.Current()
	rs := cur.Rules
	community := e.Directory.Community(group, rs)
	if community == "" {
		return nil
	}
	for _, m := range e.Directory.Members(group) {
		if e.Directory.IsSelf(m.LID) || e.Directory.IsSelf(m.Phone) {
			continue
		}
		if err := e.enforceBan(ctx, m, group, community, store.ActRemove, runID, cur); err != nil {
			return err
		}
	}
	return nil
}

// CheckJoinRequests rejects every pending join request from a banned person
// (there is no event for a request; the sweep polls).
func (e *Enforcer) CheckJoinRequests(ctx context.Context, group client.JID, runID string) error {
	cur := e.Config.Current()
	rs := cur.Rules
	community := e.Directory.Community(group, rs)
	if community == "" {
		return nil
	}
	reqs, err := e.Adapter.JoinRequests(ctx, group)
	if err != nil {
		return fmt.Errorf("join requests of %s: %w", mask.IDs(string(group)), err)
	}
	for _, r := range reqs {
		m := e.Directory.Complete(client.MemberOf(r.JID))
		if err := e.enforceBan(ctx, m, group, community, store.ActReject, runID, cur); err != nil {
			return err
		}
	}
	return nil
}

// enforceBan plans removing (or rejecting) m in group when they are banned.
func (e *Enforcer) enforceBan(ctx context.Context, m client.Member, group client.JID, community string, a store.Action,
	runID string, cur *config.Loaded) error {
	rs, where := cur.Rules, CommunityLabel(cur.Config, community)
	queued := 0
	err := e.Store.Write(ctx, func(tx *sql.Tx) error {
		ban, banned, err := store.FindBan(ctx, tx, m.IDs(), community)
		if err != nil || !banned {
			return err
		}
		if m.LID == "" && ban.LID != "" {
			m.LID = client.JID(ban.LID)
		}
		p := ledger.Plan{Trigger: "sweep:" + runID, Target: m, ConfigHash: cur.Hash, Episode: true, Reason: "banned member present"}
		if a == store.ActReject {
			p.Reason = "banned member asked to join"
		}
		if e.Directory.IsAdmin(m.LID, m.Phone, rs) {
			// Once per ban: a sweep finds the same admin every time.
			told, err := store.Reported(ctx, tx, ledger.KindAdminSpared, community, m.Key(), ban.CreatedAt)
			if err != nil || told {
				return err
			}
			e.Log.Warn("a banned member is a current admin; not removed", "community", mask.IDs(community),
				"group", mask.IDs(string(group)), "member", mask.IDs(m.Key()))
			p.Reports = []store.Report{adminSpared(community, where)}
			_, err = ledger.Write(ctx, tx, p, e.Store.Now())
			return err
		}
		enforce := enforced(rs, community)
		p.Intents = []ledger.Intent{{Action: a, Chat: group, Community: community, Enforce: enforce}}
		p.Reports = []store.Report{{Kind: ledger.KindBannedRejoin, Community: community, Buttons: []string{ledger.ButtonUndo},
			Text: fmt.Sprintf("A banned member was found in a group in %s (%s): %s.", where, p.Reason,
				onceUnpaused(pausedScope(ctx, tx, e.Store), verb(a)))}}
		if !enforce {
			p.Reports = []store.Report{{Kind: ledger.KindWouldRemove, Community: community, Text: fmt.Sprintf(
				"A banned member was found in a group in %s (%s): %s, but it is in shadow mode: not done.",
				where, p.Reason, wouldVerb(a))}}
		}
		w, err := ledger.Write(ctx, tx, p, e.Store.Now())
		if err == nil && w.New > 0 {
			e.Log.Info("sweep found a banned member", "action", string(a), "community", mask.IDs(community),
				"group", mask.IDs(string(group)), "member", mask.IDs(m.Key()), "enforce", enforce)
		}
		queued = w.Queued
		return err
	})
	if err == nil && queued > 0 {
		e.wake()
	}
	return err
}

// pausedScope is which actions a pause holds now: store.ScopeAll (every
// action), store.ScopeRemoveBan (removals and bans; deletes go on) or ""
// (none). A report says what waits, so admins are never told a held action
// was done.
func pausedScope(ctx context.Context, tx *sql.Tx, st *store.Store) store.Scope {
	if all, _ := st.PausedForTx(ctx, tx, store.ScopeAll); all {
		return store.ScopeAll
	}
	if rb, _ := st.PausedForTx(ctx, tx, store.ScopeRemoveBan); rb {
		return store.ScopeRemoveBan
	}
	return ""
}

// onceUnpaused says when a removal (or rejection) happens: at once, or once
// the pause holding it ends (every pause holds removals).
func onceUnpaused(paused store.Scope, done string) string {
	if paused == "" {
		return done
	}
	return done + " once the pause ends"
}

func verb(a store.Action) string {
	if a == store.ActReject {
		return "request rejected"
	}
	return "removed"
}

func wouldVerb(a store.Action) string {
	if a == store.ActReject {
		return "their request would be rejected"
	}
	return "they would be removed"
}

// enforced reports whether community is in enforce mode.
func enforced(rs *rules.Ruleset, community string) bool {
	mode, _ := rs.ModeFor(community)
	return mode == rules.Enforce
}

// ResolveBans looks up the LID of every ban known only by a phone number. A
// failed lookup keeps the phone key and raises a priority report.
func (e *Enforcer) ResolveBans(ctx context.Context) error {
	bans, err := e.Store.UnresolvedBans(ctx)
	if err != nil {
		return err
	}
	looked := map[string]bool{} // one lookup per phone: each settles every ban on it
	for _, b := range bans {
		if looked[b.Phone] {
			continue
		}
		looked[b.Phone] = true
		lid, err := e.Adapter.ResolvePhoneToLID(ctx, client.JID(b.Phone))
		if err == nil && lid.Server() == "lid" {
			if err := e.Store.ResolveBan(ctx, b.Phone, string(lid.Bare())); err != nil {
				return err
			}
			continue
		}
		if err == nil {
			err = errors.New("WhatsApp returned no LID")
		}
		if errors.Is(err, client.ErrRateLimited) {
			return err
		}
		e.Log.Warn("ban phone number not resolved to a LID", "phone", mask.IDs(b.Phone), "err", mask.IDs(err.Error()))
		reported, err := e.Store.MarkBanLIDFailed(ctx, b.Phone, store.Report{Kind: ledger.KindLIDUnresolved, Priority: true,
			Subject: b.Member, Text: "A banned phone number could not be matched to its WhatsApp ID (LID). The ban is kept " +
				"on the phone number; someone who joins under their LID only will not match it."})
		if err != nil {
			return err
		}
		if reported {
			e.wake()
		}
	}
	return nil
}

package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/rules"
	"github.com/hoiung/groupwarden/internal/store"
)

// Admin applies the admin chat's decisions ([Undo], [Ban], [Add to ban
// list]) to the ledger. Each is one plan written in one transaction, its
// rows triggered "tg:<report>:<button>" and naming the admin as the actor.
// The executor fires the rows like any other, with every fire-time check
// except asking the rules again (the rows carry no evidence: an admin
// decided).
type Admin struct {
	Store     *store.Store
	Config    *config.Holder
	Directory *Directory
	Enforcer  *Enforcer
	Log       *slog.Logger
}

// errGroupsNotLoaded refuses a ban until the group list has loaded: one
// planned before then finds no group to delete the member's posts in or to
// check them as an admin of, and once they are banned no later ban plans
// those deletes. The press (or command) can be tried again.
var errGroupsNotLoaded = errors.New("the bot has not loaded its group list yet (it has just started, or it cannot read the " +
	"list from WhatsApp, which it alerts about); try again in a minute")

// adminTrigger is a press's ledger trigger.
func adminTrigger(reportID int64, button string) string {
	return store.AdminTrigger + strconv.FormatInt(reportID, 10) + ":" + strings.ToLower(strings.ReplaceAll(button, " ", "_"))
}

// Undone is what [Undo] did.
type Undone struct {
	Member     client.Member
	BansLifted int
	Overturned int64
	// RemovedFrom lists every group the bot removed the member from: an
	// admin re-invites them there by hand.
	RemovedFrom []client.JID
}

// Undo lifts every ban of the report's member and marks the bot's
// removals, rejections and bans of them overturned (a delete of the
// report's message still queued is cancelled; one already sent cannot be
// undone).
func (a *Admin) Undo(ctx context.Context, reportID int64, actor string) (Undone, error) {
	r, err := a.report(ctx, reportID)
	if err != nil {
		return Undone{}, err
	}
	if r.Subject == "" {
		return Undone{}, fmt.Errorf("report #%d is not about a member", reportID)
	}
	m, err := a.member(ctx, r.Subject)
	if err != nil {
		return Undone{}, err
	}
	cur := a.Config.Current()
	reason := "undone in the admin chat by " + actor
	p := ledger.Plan{Trigger: adminTrigger(reportID, ledger.ButtonUndo), Target: m, ConfigHash: cur.Hash,
		Actor: client.JID(actor), Reason: reason, Lift: true, BanCommunity: r.Community}
	var n int64
	var bans []store.Ban
	removed := map[client.JID]bool{}
	now := a.Store.Now()
	// The reads and the overturn are one transaction, so an action the
	// executor settles meanwhile is either listed here or overturned after.
	if err := a.Store.Write(ctx, func(tx *sql.Tx) error {
		rows, err := store.LedgerForTargetsIn(ctx, tx, m.IDs())
		if err != nil {
			return err
		}
		linked, err := store.LedgerForReportIn(ctx, tx, reportID)
		if err != nil {
			return err
		}
		if bans, err = store.BansForIn(ctx, tx, m.IDs()); err != nil {
			return err
		}
		var ids []int64
		for _, row := range rows {
			switch row.Action {
			case store.ActRemove, store.ActReject, store.ActBan:
				if row.Status == store.Intended || row.Status == store.Requested {
					ids = append(ids, row.ID)
				}
				if row.Action == store.ActRemove && row.Status == store.Requested && row.Mode == store.ModeEnforce {
					removed[client.JID(row.Chat)] = true
				}
			}
		}
		for _, row := range linked {
			if row.Action == store.ActRevoke && row.Status == store.Intended {
				ids = append(ids, row.ID)
			}
		}
		if _, err := ledger.Write(ctx, tx, p, now); err != nil {
			return err
		}
		n, err = store.Overturn(ctx, tx, ids, reason, now)
		return err
	}); err != nil {
		return Undone{}, err
	}
	out := Undone{Member: m, BansLifted: len(bans), Overturned: n}
	for g := range removed {
		out.RemovedFrom = append(out.RemovedFrom, g)
	}
	sort.Slice(out.RemovedFrom, func(i, j int) bool { return out.RemovedFrom[i] < out.RemovedFrom[j] })
	a.Log.Info("undo", "report", reportID, "target", mask.IDs(m.Key()), "bans_lifted", len(bans), "overturned", n,
		"removed_from", len(out.RemovedFrom), "by", actor)
	return out, nil
}

// Banned is what [Ban] or [Add to ban list] planned.
type Banned struct {
	Member client.Member
	// Spared: the member is a current admin, so nothing was done.
	Spared bool
	// Deleting: the message's delete is queued; TooOld: it was older than
	// act_on_replay_max_age, so it is left alone.
	Deleting, TooOld bool
	Removals         int
	Shadow           bool // the community is in shadow mode: recorded only
	// Paused is the pause in force when the ban was written ("": none): the
	// ban applies at once, but the removals (and, under store.ScopeAll, the
	// deletes) wait for it to end.
	Paused store.Scope
	// Posts is what the ban did about the member's other posts.
	Posts Posts
}

// Posts counts what a ban did about the banned member's posts the bot still
// has a record of (postDeletes).
type Posts struct {
	Deleting int // deletes queued (a full pause holds them until it ends)
	Refused  int // older than act_on_replay_max_age: recorded failed, never sent
	Shadow   int // in a community in shadow mode, or the ban is not enforced: recorded only
	Spared   int // in a community where the member is a current admin: left alone
}

// postsTooOld is why a post older than act_on_replay_max_age is not deleted.
func postsTooOld(maxAge time.Duration) string {
	return store.ReasonTooOld + " (" + maxAge.String() + ")"
}

// postDeletes plans the delete of every post of m the bot still has a record
// of (store.PostsBy) in the groups the ban scopes reach (each scope's
// community, or every configured one for store.BanEverywhere), except the
// post skipMsg in skipChat (a reported post, deleted on its own). A post
// older than act_on_replay_max_age is recorded failed and never sent; one in
// a community in shadow mode, or for a ban that is not enforced, is recorded
// only; one in a community where m is a current admin is left alone (the
// executor would refuse each delete with a report of its own). It reads
// through tx, so a delete queued meanwhile is never planned twice.
func (a *Admin) postDeletes(ctx context.Context, tx *sql.Tx, m client.Member, scopes []string, enforce bool,
	skipChat client.JID, skipMsg string) ([]ledger.Intent, Posts, error) {
	var n Posts
	cur := a.Config.Current()
	rs := cur.Rules
	var communities []string
	for _, s := range scopes {
		if s == store.BanEverywhere {
			communities = rs.Communities()
			break
		}
		if _, ok := rs.ModeFor(s); ok && !slices.Contains(communities, s) {
			communities = append(communities, s)
		}
	}
	communityOf := map[string]string{}
	var chats []string
	for _, c := range communities {
		for _, g := range a.Directory.Groups(c, rs) {
			communityOf[string(g)] = c
			chats = append(chats, string(g))
		}
	}
	if len(chats) == 0 || len(m.IDs()) == 0 {
		why := "no group the bot knows is in the ban's scope"
		if len(m.IDs()) == 0 {
			why = "the member has no address to find posts by"
		}
		a.Log.Info("admin ban: no post deletes planned", "reason", why, "target", mask.IDs(m.Key()))
		return nil, n, nil
	}
	posts, err := store.PostsBy(ctx, tx, m.IDs(), chats)
	if err != nil {
		return nil, n, err
	}
	maxAge := time.Duration(cur.Config.ActOnReplayMaxAge)
	now := a.Store.Now()
	admin := map[string]bool{}
	var out []ledger.Intent
	for _, p := range posts {
		if client.JID(p.Chat) == skipChat && p.MsgID == skipMsg {
			continue
		}
		c := communityOf[p.Chat]
		isAdmin, known := admin[c]
		if !known {
			isAdmin = a.Directory.IsAdmin(m.LID, m.Phone, c, rs)
			admin[c] = isAdmin
		}
		if isAdmin {
			n.Spared++
			continue
		}
		in := ledger.Intent{Action: store.ActRevoke, Chat: client.JID(p.Chat), Community: c, Enforce: enforce && enforced(rs, c),
			Address: client.JID(p.Sender), MsgID: p.MsgID, MsgTime: p.ServerTime}
		switch {
		case !in.Enforce:
			n.Shadow++
		case now.Sub(p.ServerTime) > maxAge:
			in.Refused = postsTooOld(maxAge)
			n.Refused++
		default:
			n.Deleting++
		}
		out = append(out, in)
	}
	return out, n, nil
}

// Ban acts on a watch-only ("would have acted") report: it deletes the
// message while it is younger than act_on_replay_max_age, and removes and
// bans the sender wherever the ban reaches.
func (a *Admin) Ban(ctx context.Context, reportID int64, actor string) (Banned, error) {
	r, err := a.report(ctx, reportID)
	if err != nil {
		return Banned{}, err
	}
	if r.Kind != ledger.KindWouldHaveActed {
		return Banned{}, fmt.Errorf("only a \"would have acted\" report can be acted on with [Ban]; #%d is a %s report",
			reportID, r.Kind)
	}
	ev, ok, err := a.Store.Evidence(ctx, r.EvidenceID)
	if err != nil {
		return Banned{}, err
	}
	if !ok {
		return Banned{}, fmt.Errorf("the evidence copy of #%d is gone (purged or forgotten); use `groupwarden ban add`",
			reportID)
	}
	m, err := Identify(ctx, a.Directory, a.Store, nil, client.MemberOf(client.JID(ev.Sender), client.JID(ev.SenderAlt)))
	if err != nil {
		return Banned{}, err
	}
	cur := a.Config.Current()
	now := a.Store.Now()
	// An edit is aged by its original's time: the message the delete targets.
	sent, err := targetSent(ctx, a.Store, ev.Chat, ev.MsgID, ev.TargetID, ev.MsgTime)
	if err != nil {
		return Banned{}, err
	}
	var del *ledger.Intent
	tooOld := now.Sub(sent) > time.Duration(cur.Config.ActOnReplayMaxAge)
	if !tooOld {
		del = &ledger.Intent{Action: store.ActRevoke, Chat: client.JID(ev.Chat), Community: r.Community,
			Address: client.JID(ev.Sender), MsgID: ev.TargetID, MsgTime: sent}
	}
	out, err := a.banPlan(ctx, ledger.Plan{Trigger: adminTrigger(reportID, ledger.ButtonBan), Target: m, Rule: ev.Rule,
		Reason: "banned in the admin chat by " + actor}, r.Community, client.JID(ev.Chat), del, ev.TargetID, actor)
	out.TooOld = tooOld
	return out, err
}

// AddToBanList bans the member a human admin removed (the "Add them to the
// ban list?" report) and removes them from every group the ban reaches.
func (a *Admin) AddToBanList(ctx context.Context, reportID int64, actor string) (Banned, error) {
	r, err := a.report(ctx, reportID)
	if err != nil {
		return Banned{}, err
	}
	if r.Kind != ledger.KindHumanRemoval || r.Subject == "" {
		return Banned{}, fmt.Errorf("only a human-removal report can be acted on with [Add to ban list]; #%d is a %s report",
			reportID, r.Kind)
	}
	m, err := a.member(ctx, r.Subject)
	if err != nil {
		return Banned{}, err
	}
	return a.banPlan(ctx, ledger.Plan{Trigger: adminTrigger(reportID, ledger.ButtonAddToBanList), Target: m,
		Reason: "added to the ban list in the admin chat by " + actor}, r.Community, "", nil, "", actor)
}

// banPlan completes p with the removals and the ban for community (plus the
// delete of the reported post reported in inChat, when given) and the
// deletes of the member's other posts the bot still has a record of, and
// writes it.
func (a *Admin) banPlan(ctx context.Context, p ledger.Plan, community string, inChat client.JID, del *ledger.Intent,
	reported, actor string) (Banned, error) {
	if !a.Directory.IsLoaded() {
		return Banned{}, errGroupsNotLoaded
	}
	cur := a.Config.Current()
	rs := cur.Rules
	mode, configured := rs.ModeFor(community)
	if !configured {
		return Banned{}, fmt.Errorf("%s is no longer a configured community", community)
	}
	enforce := mode == rules.Enforce
	removals, spared := a.Enforcer.Removals(p.Target, rs.BanTargets(community), inChat, rs)
	if slices.Contains(spared, community) {
		a.Log.Info("admin ban skipped: the member is a current admin", "target", mask.IDs(p.Target.Key()), "by", actor)
		return Banned{Member: p.Target, Spared: true}, nil
	}
	if del != nil {
		del.Enforce = enforce
	}
	p.ConfigHash, p.Actor = cur.Hash, client.JID(actor)
	p.Ban, p.BanEnforce, p.BanCommunity = BanScopes(rs, community), enforce, community
	var w ledger.Written
	var paused store.Scope
	var posts Posts
	if err := a.Store.Write(ctx, func(tx *sql.Tx) error {
		paused = pausedScope(ctx, tx, a.Store)
		deletes, n, err := a.postDeletes(ctx, tx, p.Target, p.Ban, enforce, inChat, reported)
		if err != nil {
			return err
		}
		posts, p.Intents = n, nil
		if del != nil {
			p.Intents = append(p.Intents, *del)
		}
		p.Intents = append(append(p.Intents, deletes...), removals...)
		w, err = ledger.Write(ctx, tx, p, a.Store.Now())
		return err
	}); err != nil {
		return Banned{}, err
	}
	if w.Queued > 0 {
		a.Enforcer.wake()
	}
	a.Log.Info("admin ban", "trigger", p.Trigger, "target", mask.IDs(p.Target.Key()), "delete", del != nil,
		"removals", len(removals), "posts_deleting", posts.Deleting, "posts_refused", posts.Refused,
		"posts_shadow", posts.Shadow, "posts_spared", posts.Spared, "queued", w.Queued, "enforce", enforce, "by", actor)
	return Banned{Member: p.Target, Deleting: del != nil && enforce, Removals: len(removals), Shadow: !enforce,
		Paused: paused, Posts: posts}, nil
}

// NumberBanned is what /ban <number> did.
type NumberBanned struct {
	// Member is the number, with the LID the bot knows or found for it.
	Member client.Member
	// Already: the number is banned everywhere already, so no ban was added
	// (its posts are still deleted and the caller still asks for a sweep).
	// Self: it is the bot's own number. PerCommunity: bans.scope is
	// per_community, so a ban needs its community (`groupwarden ban add
	// --community`). Spared: it is a current admin of a community the bot
	// moderates, which [Ban] refuses too. None of those three changes
	// anything.
	Already, Self, PerCommunity, Spared bool
	Posts                               Posts
	// Paused is the pause in force when the ban was written ("": none).
	Paused store.Scope
}

// BanNumber is /ban <number>: it bans a phone number in the admin chat,
// stored as `groupwarden ban add <phone>` stores it (keyed by the phone JID
// until the sweep resolves its LID, in the scope bans.scope gives, with
// every action in enforce), its ledger rows triggered store.AdminBanTrigger
// and naming the admin, and deletes the number's posts the bot still has a
// record of, found by the number and by the LID the bot knows or finds for
// it (numberMember). Removing them from the groups is the sweep's, which the
// caller asks for. A number already banned everywhere gets no second ban,
// but its posts are deleted all the same: every /ban cleans up.
func (a *Admin) BanNumber(ctx context.Context, phone client.JID, actor string) (NumberBanned, error) {
	if !a.Directory.IsLoaded() {
		return NumberBanned{}, errGroupsNotLoaded
	}
	cur := a.Config.Current()
	target := client.Member{Phone: phone}
	switch {
	case a.Directory.IsSelf(phone):
		a.Log.Info("admin ban by number refused: the bot's own number", "by", actor)
		return NumberBanned{Member: target, Self: true}, nil
	case cur.Rules.BanScope() == rules.PerCommunity:
		a.Log.Info("admin ban by number refused: bans.scope is per_community", "target", mask.IDs(target.Key()), "by", actor)
		return NumberBanned{Member: target, PerCommunity: true}, nil
	}
	m, err := a.numberMember(ctx, phone)
	if err != nil {
		return NumberBanned{}, err
	}
	out := NumberBanned{Member: m}
	if slices.ContainsFunc(cur.Rules.Communities(), func(c string) bool {
		return a.Directory.IsAdmin(m.LID, m.Phone, c, cur.Rules)
	}) {
		a.Log.Info("admin ban by number skipped: a current admin", "target", mask.IDs(m.Key()), "by", actor)
		return NumberBanned{Member: m, Spared: true}, nil
	}
	now := a.Store.Now()
	p := ledger.Plan{Trigger: store.AdminBanTrigger + strconv.FormatInt(now.UnixMilli(), 10), Target: target,
		ConfigHash: cur.Hash, Actor: client.JID(actor), Reason: "banned in the admin chat by " + actor,
		Ban: []string{store.BanEverywhere}, BanEnforce: true, BanCommunity: store.BanEverywhere}
	scopes := p.Ban
	var w ledger.Written
	if err := a.Store.Write(ctx, func(tx *sql.Tx) error {
		// Only a ban everywhere is the one this would add; a community's own
		// ban (from a per_community config before) does not stop it.
		bans, err := store.BansForIn(ctx, tx, m.IDs())
		if err != nil {
			return err
		}
		if out.Already = slices.ContainsFunc(bans, func(b store.Ban) bool { return b.Scope == store.BanEverywhere }); out.Already {
			p.Ban, p.BanEnforce = nil, false
		}
		out.Paused = pausedScope(ctx, tx, a.Store)
		if p.Intents, out.Posts, err = a.postDeletes(ctx, tx, m, scopes, true, "", ""); err != nil {
			return err
		}
		w, err = ledger.Write(ctx, tx, p, now)
		return err
	}); err != nil {
		return NumberBanned{}, err
	}
	if w.Queued > 0 {
		a.Enforcer.wake()
	}
	a.Log.Info("admin ban by number", "trigger", p.Trigger, "target", mask.IDs(m.Key()), "already", out.Already,
		"posts_deleting", out.Posts.Deleting, "posts_refused", out.Posts.Refused, "posts_shadow", out.Posts.Shadow,
		"posts_spared", out.Posts.Spared, "queued", w.Queued, "by", actor)
	return out, nil
}

// numberMember is the member a phone number names, by every address the bot
// knows (member) and, when it has no record of their LID, the one WhatsApp
// gives for the number (its own store first, then one lookup): a ban by
// number then finds the posts of someone who joined since the group list was
// last read and whose posts came without their number, and an unban by
// number finds a ban that names them by LID alone. A lookup that finds
// nothing is logged and the number alone used (for a ban, the sweep looks it
// up again).
func (a *Admin) numberMember(ctx context.Context, phone client.JID) (client.Member, error) {
	m, err := a.member(ctx, string(phone))
	if err != nil || m.LID != "" || a.Enforcer == nil || a.Enforcer.Adapter == nil {
		return m, err
	}
	lid, err := a.Enforcer.Adapter.ResolvePhoneToLID(ctx, phone)
	if err == nil && lid.Server() != "lid" {
		err = fmt.Errorf("%w: WhatsApp returned %s", client.ErrNoLID, mask.IDs(string(lid)))
	}
	if err != nil {
		level := slog.LevelWarn // WhatsApp could not be asked
		if errors.Is(err, client.ErrNoLID) {
			level = slog.LevelInfo // it was asked and has none
		}
		a.Log.Log(ctx, level, "admin command by number: no LID found for the number; using the number alone",
			"target", mask.IDs(string(phone)), "err", mask.IDs(err.Error()))
		return m, nil
	}
	m.LID = lid.Bare()
	return a.Directory.Complete(m), nil
}

// NumberUnbanned is what /unban <number> did.
type NumberUnbanned struct {
	// Member is the number, with the LID the directory and the ban list know.
	Member client.Member
	// Lifted counts the scopes the member was banned in, each once, whether
	// by a ban list entry or a ban a pause held (dropped too), and Held those
	// only a held ban covered; NotBanned: there was neither, so nothing
	// changed. Overturned counts their deletes and removals still queued,
	// cancelled.
	Lifted, Held int
	NotBanned    bool
	Overturned   int64
}

// UnbanNumber is /unban <number>: it lifts every ban of a phone number,
// whether the ban is still keyed by the phone JID or already resolved to a
// LID (numberMember), and every ban a pause holds for it, with an unban row
// triggered store.AdminUnbanTrigger and naming the admin, and cancels their
// deletes and removals still queued, as [Undo] does: the executor would fail
// each as "not banned", and one whose call is already in flight is reported
// (ledger.KindUndoRace). Nobody is added back to a group: an admin re-invites
// them by hand.
func (a *Admin) UnbanNumber(ctx context.Context, phone client.JID, actor string) (NumberUnbanned, error) {
	m, err := a.numberMember(ctx, phone)
	if err != nil {
		return NumberUnbanned{}, err
	}
	out := NumberUnbanned{Member: m}
	now := a.Store.Now()
	reason := "unbanned in the admin chat by " + actor
	p := ledger.Plan{Trigger: store.AdminUnbanTrigger + strconv.FormatInt(now.UnixMilli(), 10), Target: m,
		ConfigHash: a.Config.Current().Hash, Actor: client.JID(actor), Reason: reason,
		Lift: true, BanCommunity: store.BanEverywhere}
	if err := a.Store.Write(ctx, func(tx *sql.Tx) error {
		bans, err := store.BansForIn(ctx, tx, m.IDs())
		if err != nil {
			return err
		}
		held, err := store.HeldBanScopesIn(ctx, tx, m.IDs())
		if err != nil {
			return err
		}
		// One ban per scope: a ban by number and a held ban by LID in the
		// same scope (both there while a pause lasts) are the same ban.
		scopes := map[string]bool{}
		for _, b := range bans {
			scopes[b.Scope] = true
		}
		for _, s := range held {
			if !scopes[s] {
				scopes[s] = true
				out.Held++
			}
		}
		out.Lifted, out.NotBanned = len(scopes), len(scopes) == 0
		if out.NotBanned {
			return nil
		}
		rows, err := store.LedgerForTargetsIn(ctx, tx, m.IDs())
		if err != nil {
			return err
		}
		var queued []int64
		for _, row := range rows {
			switch row.Action {
			case store.ActRevoke, store.ActRemove, store.ActReject:
				if row.Status == store.Intended && row.Mode == store.ModeEnforce {
					queued = append(queued, row.ID)
				}
			}
		}
		if _, err = ledger.Write(ctx, tx, p, now); err != nil {
			return err
		}
		out.Overturned, err = store.Overturn(ctx, tx, queued, reason, now)
		return err
	}); err != nil {
		return NumberUnbanned{}, err
	}
	a.Log.Info("admin unban by number", "trigger", p.Trigger, "target", mask.IDs(m.Key()), "bans_lifted", out.Lifted,
		"held_only_scopes", out.Held, "not_banned", out.NotBanned, "overturned", out.Overturned, "by", actor)
	return out, nil
}

// report reads a report or says it is gone.
func (a *Admin) report(ctx context.Context, id int64) (store.Report, error) {
	r, ok, err := a.Store.Report(ctx, id)
	if err != nil {
		return r, err
	}
	if !ok {
		return r, fmt.Errorf("there is no report #%d (it may have been purged)", id)
	}
	return r, nil
}

// member is a report subject by every address the bot knows (Identify). A
// ban written for them then carries both, and a lookup by either finds it.
func (a *Admin) member(ctx context.Context, key string) (client.Member, error) {
	return Identify(ctx, a.Directory, a.Store, nil, client.MemberOf(client.JID(key)))
}

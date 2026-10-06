package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
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

// adminTrigger is a press's ledger trigger.
func adminTrigger(reportID int64, button string) string {
	return store.AdminTrigger + strconv.FormatInt(reportID, 10) + ":" + strings.ToLower(strings.ReplaceAll(button, " ", "_"))
}

// Undone is what [Undo] did.
type Undone struct {
	Member     client.Member
	BansLifted int
	Overturned int64
	// RemovedFrom lists every group (or community) the bot removed the
	// member from: an admin re-invites them there by hand.
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
	m := a.Directory.Complete(client.MemberOf(client.JID(ev.Sender), client.JID(ev.SenderAlt)))
	cur := a.Config.Current()
	now := a.Store.Now()
	var del *ledger.Intent
	tooOld := now.Sub(ev.MsgTime) > time.Duration(cur.Config.ActOnReplayMaxAge)
	if !tooOld {
		del = &ledger.Intent{Action: store.ActRevoke, Chat: client.JID(ev.Chat), Community: r.Community,
			Address: client.JID(ev.Sender), MsgID: ev.TargetID, MsgTime: ev.MsgTime}
	}
	out, err := a.banPlan(ctx, ledger.Plan{Trigger: adminTrigger(reportID, ledger.ButtonBan), Target: m, Rule: ev.Rule,
		Reason: "banned in the admin chat by " + actor}, r.Community, client.JID(ev.Chat), del, actor)
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
		Reason: "added to the ban list in the admin chat by " + actor}, r.Community, "", nil, actor)
}

// banPlan completes p with the removals and the ban for community (plus the
// delete, when given) and writes it.
func (a *Admin) banPlan(ctx context.Context, p ledger.Plan, community string, inChat client.JID, del *ledger.Intent,
	actor string) (Banned, error) {
	cur := a.Config.Current()
	rs := cur.Rules
	mode, configured := rs.ModeFor(community)
	if !configured {
		return Banned{}, fmt.Errorf("%s is no longer a configured community", community)
	}
	enforce := mode == rules.Enforce
	removals, ok := a.Enforcer.Removals(p.Target, rs.BanTargets(community), inChat, rs)
	if !ok {
		a.Log.Info("admin ban skipped: the member is a current admin", "target", mask.IDs(p.Target.Key()), "by", actor)
		return Banned{Member: p.Target, Spared: true}, nil
	}
	if del != nil {
		del.Enforce = enforce
		p.Intents = append(p.Intents, *del)
	}
	p.Intents = append(p.Intents, removals...)
	p.ConfigHash, p.Actor = cur.Hash, client.JID(actor)
	p.Ban, p.BanEnforce, p.BanCommunity = BanScopes(rs, community), enforce, community
	var w ledger.Written
	if err := a.Store.Write(ctx, func(tx *sql.Tx) error {
		var err error
		w, err = ledger.Write(ctx, tx, p, a.Store.Now())
		return err
	}); err != nil {
		return Banned{}, err
	}
	if w.Queued > 0 {
		a.Enforcer.wake()
	}
	a.Log.Info("admin ban", "trigger", p.Trigger, "target", mask.IDs(p.Target.Key()), "delete", del != nil,
		"removals", len(removals), "queued", w.Queued, "enforce", enforce, "by", actor)
	return Banned{Member: p.Target, Deleting: del != nil && enforce, Removals: len(removals), Shadow: !enforce}, nil
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

// member is a report subject by every address the directory and the ban
// list know.
func (a *Admin) member(ctx context.Context, key string) (client.Member, error) {
	m := a.Directory.Complete(client.MemberOf(client.JID(key)))
	bans, err := a.Store.BansFor(ctx, m.IDs())
	if err != nil {
		return m, err
	}
	for _, b := range bans {
		if m.LID == "" && b.LID != "" {
			m.LID = client.JID(b.LID)
		}
		if m.Phone == "" && b.Phone != "" {
			m.Phone = client.JID(b.Phone)
		}
	}
	return a.Directory.Complete(m), nil
}

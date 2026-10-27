// Package ledger records every moderation action before it can fire: one
// transaction writes the action rows (intended), queues the enforce ones in
// the outbox, adds the ban, keeps the evidence copy and stores the admin
// report. It also recovers after a crash and purges by retention; the admin
// chat (internal/telegram) delivers the reports.
package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/store"
)

// Report kinds.
const (
	KindAction         = "action"           // deleted, removed and banned (enforce)
	KindWouldHaveActed = "would_have_acted" // a watch-only rule matched, or the message was too old to act on
	KindExempt         = "exempt"           // an admin or Meta AI matched an acting rule: reported only
	KindUnaddressable  = "unaddressable"    // an acting rule matched a sender the bot cannot remove or ban: reported only
	KindLog            = "log"              // a log rule matched
	KindBannedRejoin   = "banned_rejoin"    // a banned member joined and is removed
	KindBanLifted      = "ban_lifted"       // a human admin re-added a banned member: the ban is lifted
	KindUnknownActor   = "unknown_actor"    // a banned member joined and WhatsApp did not say who added them
	KindHumanRemoval   = "human_removal"    // a human admin removed someone: offer to ban them
	KindAdminSpared    = "admin_spared"     // a removal skipped a current human admin
	KindLIDUnresolved  = "lid_unresolved"   // a phone number could not be resolved to a LID
	KindWouldRemove    = "would_remove"     // a removal in a community that is in shadow mode
	KindStartup        = "startup"          // actions left at intended by a crash, retried
	KindUndoRace       = "undo_race"        // a removal went out while [Undo] or /unban ran: re-invite by hand (priority)
	KindBanCLI         = "ban_cli"          // a ban added or removed with the ban command
	KindNotDone        = "not_done"         // an action the bot gave up on: do it by hand (priority)
)

// Buttons on reports (Telegram renders them; the names are the contract).
const (
	ButtonUndo           = "Undo"
	ButtonBan            = "Ban"
	ButtonResume         = "Resume"
	ButtonAddToBanList   = "Add to ban list"
	ButtonNo             = "No"
	ButtonShowAttachment = "Show attachment"
	ButtonDone           = "Done"
)

// Intent is one action on the plan's target.
type Intent struct {
	Action    store.Action
	Chat      client.JID // the group or community it happens in
	Community string     // the configured community it counts towards
	Enforce   bool       // false: recorded in shadow, never fired
	// Address is the target's address as Chat knows it (a revoke's author,
	// a removal's participant, a rejection's requester).
	Address client.JID
	// Message-triggered intents: the message to delete (revoke) and the
	// server time of the message an action targets (age re-check).
	MsgID   string
	MsgTime time.Time
	// Refused, when set, is why the action cannot be done (a post older than
	// act_on_replay_max_age): the row is written failed with it, recorded
	// and never queued.
	Refused string
}

// Plan is everything one decision writes, in one transaction.
type Plan struct {
	// Trigger identifies what caused it: a message ID, "join:<hash>",
	// "sweep:<run>", "cli:<time>", a report and button, or an admin-chat
	// command naming a phone number (store.AdminBanTrigger,
	// store.AdminUnbanTrigger).
	Trigger string
	Target  client.Member
	// Person is the target with every address the caller knows for them,
	// which finds their ban list entry (one per person and scope); empty: the
	// target. /ban <number> keeps the number as its target and passes the LID
	// it found here.
	Person     client.Member
	Rule       string
	ConfigHash string
	Actor      client.JID
	Reason     string
	Intents    []Intent
	// Ban adds the target to the ban list in each scope (store.BanEverywhere
	// or a community) with a ban row in the ledger; BanEnforce false records
	// shadow ban rows only. BanCommunity is the community those rows count
	// towards. Someone already on the list there keeps their one entry: a
	// ban reuses the row the entry links to when the entry keeps its link
	// (store.BanEntry.Keeps); otherwise (an admin's ban, store.AdminOrdered,
	// of someone the bot banned, or an entry whose row was purged since) it
	// writes its own ban row and the entry's link moves to it (store.PutBan).
	Ban          []string
	BanEnforce   bool
	BanCommunity string
	// BanHeld: a pause covering removals and bans is in force, so an enforce
	// ban is recorded but not applied. Once no pause covers bans the executor
	// settles it: applied when any post it stands for still counts under the
	// current config, failed when none does. An admin's ban of the member
	// applies at once and settles it too.
	BanHeld bool
	// Lift removes the target's bans (a human admin re-added them, or an
	// admin unbanned them) with an unban row in the ledger. LiftIn narrows it
	// to the bans covering one community (its own and an everywhere ban, as
	// store.FindBan matches them), which the unban row names; "" lifts every
	// ban. A lift names the target as a person: a ban of another LID that
	// holds their number (someone who held it before) stays on that LID and
	// only loses the number (store.ReleaseNumbers). LiftByNumber: it names a
	// phone number instead (/unban <number>, `ban remove <phone>`), and every
	// ban holding the number is lifted, whoever's.
	Lift         bool
	LiftIn       string
	LiftByNumber bool
	// Episode: a sweep retry of the same action on the same target in the same
	// chat and mode reuses the open row (one row and one report per episode).
	Episode  bool
	Evidence *store.Evidence
	// Reports are linked to every ledger row the plan wrote; a plan whose
	// rows all already existed, or whose only new rows are ban rows an entry
	// already on the list moved its link to, writes no report.
	Reports []store.Report
}

// person is who the plan's ban list entries belong to (Plan.Person).
func (p Plan) person() client.Member {
	if p.Person.Key() == "" {
		return p.Target
	}
	return p.Person
}

// Written is what Write stored.
type Written struct {
	LedgerIDs []int64 // every row of the plan (new or existing)
	New       int     // rows inserted now
	Requeued  int     // failed episode rows put back in the outbox
	Queued    int     // rows now in the outbox
	// BansAdded counts ban list entries added; LinksMoved counts entries the
	// person already had whose link moved to the plan's new ban row.
	BansAdded  int
	LinksMoved int
	EvidenceID int64
	ReportIDs  []int64
	// BansLifted counts ban list entries a lift removed; NumbersReleased
	// counts entries of someone who held the target's number before that it
	// kept, taking the number off them (Plan Lift).
	BansLifted      int
	NumbersReleased int
}

// sweepPrefix marks sweep triggers, whose retries are one episode.
const sweepPrefix = "sweep:"

// existing is the row a new one would repeat, if any: the open removal or
// rejection of the member in the same chat (whatever triggered it: two posts,
// or a post and a sweep, remove once), the delete of the same message (a
// post and its edit, or two bans naming its author by different addresses,
// delete it once), the member's ban in that scope already in force (banRow)
// or held, or a sweep's row of the same episode in the same mode (an open
// row, or a failed enforce row to retry).
func existing(ctx context.Context, tx *sql.Tx, p Plan, r store.LedgerRow) (store.LedgerRow, bool, error) {
	if r.Mode == store.ModeEnforce {
		switch r.Action {
		case store.ActRevoke:
			return store.RevokeOf(ctx, tx, r.Chat, r.MsgID)
		case store.ActBan:
			return banRow(ctx, tx, p, r)
		case store.ActRemove, store.ActReject:
			if open, ok, err := store.OpenFor(ctx, tx, r.Action, r.Chat, r.Target); err != nil || ok {
				return open, ok, err
			}
		}
	}
	if !p.Episode {
		return store.LedgerRow{}, false, nil
	}
	open, ok, err := store.LatestFor(ctx, tx, r.Action, r.Chat, r.Target, sweepPrefix, r.Mode)
	if err != nil || !ok {
		return open, false, err
	}
	reuse := open.Status == store.Intended || (open.Status == store.Failed && r.Mode == store.ModeEnforce)
	return open, reuse, nil
}

// banRow is the ban row a ban r of p's person repeats: the one their ban list
// entry in r's scope links to (found by either address, store.BanEntryIn)
// when the entry keeps its link (store.BanEntry.Keeps; an admin's ban of
// someone the bot banned is written, and the link moves to it). Someone with
// no entry there whose ban a pause holds: a held ban r folds into it; an
// admin's ban (applied at once) never does.
func banRow(ctx context.Context, tx *sql.Tx, p Plan, r store.LedgerRow) (store.LedgerRow, bool, error) {
	who := p.person()
	e, found, err := store.BanEntryIn(ctx, tx, string(who.LID), string(who.Phone), r.Chat)
	switch {
	case err != nil:
		return store.LedgerRow{}, false, err
	case found && e.Keeps(store.AdminOrdered(p.Trigger)):
		return e.Row, true, nil
	case found || r.Status == store.Requested:
		return store.LedgerRow{}, false, nil
	}
	return store.HeldBanRowOf(ctx, tx, r.Target, r.Chat)
}

// revokesOnly reports whether p only deletes posts, each with its author's
// address: no removal, ban or lifting needs a ban-list key.
func revokesOnly(p Plan) bool {
	if len(p.Intents) == 0 || len(p.Ban) > 0 || p.Lift {
		return false
	}
	for _, in := range p.Intents {
		if in.Action != store.ActRevoke || in.Address == "" {
			return false
		}
	}
	return true
}

// ShowsAttachment reports whether a report of kind shows its post's
// attachment: a log rule's report is only counted in the daily summary, so it
// has no message to show it under.
func ShowsAttachment(kind string) bool {
	return kind != KindLog
}

// Write stores p inside tx: the evidence copy first, then every action row
// (intended; enforce rows queued), the ban (in the same transaction as the
// removal rows, or held by a pause) or its lifting, then the reports linked
// to the rows. A row that would repeat an existing one reuses it, and a plan
// whose rows all already existed writes no report and keeps no copy.
func Write(ctx context.Context, tx *sql.Tx, p Plan, now time.Time) (Written, error) {
	var w Written
	target := p.Target.Key()
	if target == "" && revokesOnly(p) {
		// A sender with neither a LID nor a phone number: only their post is
		// deleted, keyed by the address it came from.
		target = string(p.Intents[0].Address)
	}
	if target == "" && (len(p.Intents) > 0 || len(p.Ban) > 0 || p.Lift) {
		return w, fmt.Errorf("ledger: plan %s has actions but no target", p.Trigger)
	}
	if p.Evidence != nil {
		e := *p.Evidence
		e.Rule, e.ConfigHash, e.Subject = p.Rule, p.ConfigHash, target
		if e.MediaState == store.MediaPending && !slices.ContainsFunc(p.Reports, func(r store.Report) bool {
			return ShowsAttachment(r.Kind)
		}) {
			// No report of this plan can show the attachment, so it is
			// described (kind, name, size) and never downloaded.
			e.MediaState, e.MediaRaw = store.MediaNone, nil
		}
		id, err := store.InsertEvidence(ctx, tx, e, now)
		if err != nil {
			return w, err
		}
		w.EvidenceID = id
	}
	row := func(a store.Action, chat, community string, enforce bool) store.LedgerRow {
		mode := store.ModeShadow
		if enforce {
			mode = store.ModeEnforce
		}
		return store.LedgerRow{Action: a, Chat: chat, Target: target, TriggerID: p.Trigger, Community: community,
			Mode: mode, Reason: p.Reason, Rule: p.Rule, Actor: string(p.Actor), ConfigHash: p.ConfigHash,
			EvidenceID: w.EvidenceID}
	}
	// add writes r (or reuses the row it would repeat) and reports whether it
	// inserted it.
	add := func(r store.LedgerRow) (bool, error) {
		open, ok, err := existing(ctx, tx, p, r)
		if err != nil {
			return false, err
		}
		if ok {
			w.LedgerIDs = append(w.LedgerIDs, open.ID)
			if open.Status == store.Failed {
				if err := store.Requeue(ctx, tx, open, now); err != nil {
					return false, err
				}
				w.Requeued++
				w.Queued++
			}
			return false, nil
		}
		id, inserted, err := store.InsertLedger(ctx, tx, r, now)
		if err != nil {
			return false, err
		}
		w.LedgerIDs = append(w.LedgerIDs, id)
		if inserted {
			w.New++
			if r.Mode == store.ModeEnforce && r.Status == "" && r.Action.WhatsApp() {
				w.Queued++
			}
		}
		return inserted, nil
	}
	for _, in := range p.Intents {
		r := row(in.Action, string(in.Chat), in.Community, in.Enforce)
		r.Address, r.MsgID, r.MsgTime = string(in.Address), in.MsgID, in.MsgTime
		if in.Refused != "" {
			r.Status, r.Reason = store.Failed, in.Refused
		}
		if _, err := add(r); err != nil {
			return w, err
		}
	}
	held := p.BanEnforce && p.BanHeld
	who := p.person()
	for _, scope := range p.Ban {
		r := row(store.ActBan, scope, p.BanCommunity, p.BanEnforce)
		r.Address = string(p.Target.Phone) // kept for a held ban, applied later
		// A ban is applied here, in the transaction that also holds its
		// removal rows, so it never waits on the outbox; a pause covering
		// bans holds it at intended instead (never queued).
		if !held {
			r.Status = store.Requested
		}
		inserted, err := add(r)
		if err != nil {
			return w, err
		}
		if !p.BanEnforce || held {
			continue
		}
		// One entry per person: an entry they have there already is kept, with
		// the address it lacks filled in, also when its row is reused.
		if !inserted {
			if err := store.FillBan(ctx, tx, string(who.LID), string(who.Phone), scope); err != nil {
				return w, err
			}
			continue
		}
		id := w.LedgerIDs[len(w.LedgerIDs)-1]
		added, moved, err := store.PutBan(ctx, tx, store.Ban{Member: target, Scope: scope, LID: string(p.Target.LID),
			Phone: string(p.Target.Phone), Reason: p.Reason, LedgerID: id}, string(who.LID), string(who.Phone),
			store.AdminOrdered(p.Trigger), now)
		if err != nil {
			return w, err
		}
		if added {
			w.BansAdded++
		}
		if moved {
			w.LinksMoved++
		}
		// A ban a pause holds for the member there is in force now too.
		if err := store.SettleHeldBansIn(ctx, tx, p.Target.IDs(), scope, id, now); err != nil {
			return w, err
		}
	}
	if p.Lift {
		r := row(store.ActUnban, p.LiftIn, p.BanCommunity, true)
		r.Status = store.Requested
		if _, err := add(r); err != nil {
			return w, err
		}
		if lid := string(p.Target.LID); lid != "" && !p.LiftByNumber {
			n, err := store.ReleaseNumbers(ctx, tx, p.Target.IDs(), lid, p.LiftIn, now)
			if err != nil {
				return w, err
			}
			w.NumbersReleased = int(n)
		}
		n, err := store.RemoveBans(ctx, tx, p.Target.IDs(), p.LiftIn)
		if err != nil {
			return w, err
		}
		w.BansLifted = int(n)
		if err := store.DropHeldBans(ctx, tx, p.Target.IDs(), p.LiftIn, "unbanned before a pause let the ban apply", now); err != nil {
			return w, err
		}
	}
	if w.New == w.LinksMoved && (len(p.Intents) > 0 || len(p.Ban) > 0 || p.Lift) {
		// Every row already existed (a redelivery, an edit of a post already
		// acted on, or a sweep retrying the same episode): the admins were
		// already told, and the rows keep the copy they were written with, so
		// this one is not kept. Or the only new rows are ban rows of someone
		// already on the ban list, whose entry moved its link to them: nobody
		// was added, so no "ban added" report.
		if w.New == 0 && w.EvidenceID != 0 {
			if err := store.DeleteEvidence(ctx, tx, w.EvidenceID); err != nil {
				return w, err
			}
			w.EvidenceID = 0
		}
		return w, nil
	}
	for _, rep := range p.Reports {
		rep.EvidenceID = w.EvidenceID
		// The post's own report, written with its evidence copy, is the one its
		// attachment is shown under.
		rep.OwnsAttachment = w.EvidenceID != 0 && ShowsAttachment(rep.Kind)
		if rep.Subject == "" {
			rep.Subject = target
		}
		id, err := store.InsertReport(ctx, tx, rep, w.LedgerIDs, now)
		if err != nil {
			return w, err
		}
		w.ReportIDs = append(w.ReportIDs, id)
	}
	return w, nil
}

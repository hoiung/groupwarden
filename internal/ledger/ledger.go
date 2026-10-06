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
	KindUndoRace       = "undo_race"        // a removal went out while [Undo] ran: re-invite by hand (priority)
	KindBanCLI         = "ban_cli"          // a ban added or removed with the ban command
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
}

// Plan is everything one decision writes, in one transaction.
type Plan struct {
	// Trigger identifies what caused it: a message ID, "join:<hash>",
	// "sweep:<run>", "cli:<time>" or a report and button.
	Trigger    string
	Target     client.Member
	Rule       string
	ConfigHash string
	Actor      client.JID
	Reason     string
	Intents    []Intent
	// Ban adds the target to the ban list in each scope (store.BanEverywhere
	// or a community) with a ban row in the ledger; BanEnforce false records
	// shadow ban rows only. BanCommunity is the community those rows count
	// towards.
	Ban          []string
	BanEnforce   bool
	BanCommunity string
	// BanHeld: a pause covering removals and bans is in force, so an enforce
	// ban is recorded but not applied; the first removal of the target that
	// passes its fire-time re-check after [Resume] applies it
	// (store.ApplyHeldBans), and a re-check that finds the message no longer
	// spam fails it.
	BanHeld bool
	// Lift removes every ban of the target (a human admin re-added them, or
	// an admin unbanned them) with an unban row in the ledger.
	Lift bool
	// Episode: a sweep retry of the same action on the same target in the same
	// chat and mode reuses the open row (one row and one report per episode).
	Episode  bool
	Evidence *store.Evidence
	// Reports are linked to every ledger row the plan wrote; a plan whose
	// rows all already existed writes no report.
	Reports []store.Report
}

// Written is what Write stored.
type Written struct {
	LedgerIDs  []int64 // every row of the plan (new or existing)
	New        int     // rows inserted now
	Requeued   int     // failed episode rows put back in the outbox
	Queued     int     // rows now in the outbox
	EvidenceID int64
	ReportIDs  []int64
}

// sweepPrefix marks sweep triggers, whose retries are one episode.
const sweepPrefix = "sweep:"

// existing is the row a new one would repeat, if any: the open removal or
// rejection of the member in the same chat (whatever triggered it: two posts,
// or a post and a sweep, remove once), the delete of the same message (a
// post and its edit delete it once), the member's ban in that scope already
// in force or held, or a sweep's row of the same episode in the same mode
// (an open row, or a failed enforce row to retry).
func existing(ctx context.Context, tx *sql.Tx, p Plan, r store.LedgerRow) (store.LedgerRow, bool, error) {
	if r.Mode == store.ModeEnforce {
		switch r.Action {
		case store.ActRevoke:
			return store.RevokeOf(ctx, tx, r.Chat, r.Target, r.MsgID)
		case store.ActBan:
			return store.BanRowOf(ctx, tx, r.Target, r.Chat)
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

// Write stores p inside tx: the evidence copy first, then every action row
// (intended; enforce rows queued), the ban (in the same transaction as the
// removal rows, or held by a pause) or its lifting, then the reports linked
// to the rows. A row that would repeat an existing one reuses it, and a plan
// whose rows all already existed writes no report.
func Write(ctx context.Context, tx *sql.Tx, p Plan, now time.Time) (Written, error) {
	var w Written
	target := p.Target.Key()
	if target == "" && (len(p.Intents) > 0 || len(p.Ban) > 0 || p.Lift) {
		return w, fmt.Errorf("ledger: plan %s has actions but no target", p.Trigger)
	}
	if p.Evidence != nil {
		e := *p.Evidence
		e.Rule, e.ConfigHash, e.Subject = p.Rule, p.ConfigHash, target
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
		if _, err := add(r); err != nil {
			return w, err
		}
	}
	held := p.BanEnforce && p.BanHeld
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
		if !p.BanEnforce || held || !inserted {
			continue
		}
		if err := store.AddBan(ctx, tx, store.Ban{Member: target, Scope: scope, LID: string(p.Target.LID),
			Phone: string(p.Target.Phone), Reason: p.Reason, LedgerID: w.LedgerIDs[len(w.LedgerIDs)-1]}, now); err != nil {
			return w, err
		}
	}
	if p.Lift {
		r := row(store.ActUnban, "", p.BanCommunity, true)
		r.Status = store.Requested
		if _, err := add(r); err != nil {
			return w, err
		}
		if _, err := store.RemoveBans(ctx, tx, p.Target.IDs()); err != nil {
			return w, err
		}
		if err := store.DropHeldBans(ctx, tx, p.Target.IDs(), "unbanned before a pause let the ban apply", now); err != nil {
			return w, err
		}
	}
	if w.New == 0 && (len(p.Intents) > 0 || len(p.Ban) > 0 || p.Lift) {
		// Every row already existed (a redelivery, or a sweep retrying the
		// same episode): the admins were already told.
		return w, nil
	}
	for _, rep := range p.Reports {
		rep.EvidenceID = w.EvidenceID
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

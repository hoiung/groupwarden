package ledger_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
)

// actionOf maps a fake adapter call to the ledger action it carries out.
func actionOf(call string) (store.Action, client.JID, bool) {
	f := strings.Fields(call)
	if len(f) < 2 {
		return "", "", false
	}
	switch f[0] {
	case "Revoke":
		return store.ActRevoke, client.JID(f[1]), true
	case "Remove":
		return store.ActRemove, client.JID(f[1]), true
	case "RejectJoinRequests":
		return store.ActReject, client.JID(f[1]), true
	}
	return "", "", false
}

// TestWriteBeforeFire: every WhatsApp call happens only after its ledger row
// was committed at intended (with its evidence copy), and the row then says
// "requested", never "succeeded".
func TestWriteBeforeFire(t *testing.T) {
	k := modtest.New(t, "")
	calls := 0
	k.Fake.OnCall = func(call string) {
		a, chat, ok := actionOf(call)
		if !ok {
			return
		}
		calls++
		rows := k.Find(modtest.SpammerM, a, chat)
		if len(rows) != 1 || rows[0].Status != store.Intended || rows[0].Mode != store.ModeEnforce {
			t.Errorf("%s fired with ledger rows %+v, want one enforce row at intended", call, rows)
		}
		if rows[0].EvidenceID == 0 {
			t.Errorf("%s fired before its evidence copy was linked", call)
		}
	}
	k.Deliver(k.Spam("M1", modtest.G1))
	if k.Fake.Count("Revoke")+k.Fake.Count("Remove") != 0 {
		t.Fatal("an action fired inside the decision")
	}
	if n := k.Fire(); n == 0 || calls == 0 {
		t.Fatalf("fired %d actions, %d calls", n, calls)
	}
	for _, r := range k.Rows(modtest.SpammerM) {
		if r.Action == store.ActBan {
			continue
		}
		if r.Status != store.Requested {
			t.Errorf("%s %s ended at %s, want requested", r.Action, r.Chat, r.Status)
		}
	}
	if n, _ := k.Store.OutboxLen(k.Ctx); n != 0 {
		t.Fatalf("outbox still holds %d rows", n)
	}
}

func join(k *modtest.Kit, group client.JID, actor client.JID, members ...client.JID) *client.GroupChange {
	k.Clock.Advance(time.Minute)
	return &client.GroupChange{Group: group, Actor: actor, JoinReason: "invite", Time: k.Clock.Now(), Joined: members}
}

// TestRepeatRejoinRemovedEachTime: a banned person who rejoins twice is
// removed twice, each time under its own ledger row.
func TestRepeatRejoinRemovedEachTime(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.Other1M)
	for i := 0; i < 2; i++ {
		k.Deliver(join(k, modtest.G1, modtest.Other1, modtest.Other1))
		k.Fire()
	}
	rows := k.Find(modtest.Other1M, store.ActRemove, modtest.G1)
	if len(rows) != 2 || rows[0].TriggerID == rows[1].TriggerID {
		t.Fatalf("rows %+v, want two rows with different triggers", rows)
	}
	for _, r := range rows {
		if r.Status != store.Requested {
			t.Errorf("row %d at %s", r.ID, r.Status)
		}
	}
	if n := k.Fake.Count("Remove " + string(modtest.G1) + " " + string(modtest.Other1)); n != 2 {
		t.Fatalf("removed from the group %d times, want 2", n)
	}
}

// TestTwoBannedSameGroupBothRemoved: two banned people joining the same group
// in one event are both removed (the target is part of the key).
func TestTwoBannedSameGroupBothRemoved(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.Other1M)
	k.Ban(modtest.Other2M)
	k.Deliver(join(k, modtest.G1, modtest.Member, modtest.Other1, modtest.Other2))
	k.Fire()
	for _, m := range []client.JID{modtest.Other1, modtest.Other2} {
		if n := k.Fake.Count("Remove " + string(modtest.G1) + " " + string(m)); n != 1 {
			t.Errorf("%s removed %d times, want 1", m, n)
		}
		if rows := k.Find(client.Member{LID: m}, store.ActRemove, modtest.G1); len(rows) != 1 {
			t.Errorf("%s has %d remove rows", m, len(rows))
		}
	}
}

// TestCrashRetriesRepeatSafeAfterRecheck: rows a crash left at intended are
// retried after the restart, each re-checked first: a target unbanned in the
// meantime is not touched, the rest are sent.
func TestCrashRetriesRepeatSafeAfterRecheck(t *testing.T) {
	k := modtest.New(t, "")
	k.Deliver(k.Spam("M1", modtest.G1))
	other := k.Msg("M2", modtest.G1, modtest.Other1, modtest.SpamText)
	k.Deliver(other)
	k.Reopen() // the process died before the executor ran
	if n, err := ledger.Recover(k.Ctx, k.Store, k.Log); err != nil || n == 0 {
		t.Fatalf("recover: %d %v", n, err)
	}
	k.Unban(modtest.Other1M)
	k.Fire()
	if k.Fake.Count("Revoke "+string(modtest.G1)+" "+string(modtest.Spammer)+" M1") != 1 {
		t.Fatalf("the spammer's delete was not retried: %v", k.Fake.Calls())
	}
	if k.Fake.Count("Revoke "+string(modtest.G1)+" "+string(modtest.Other1)) != 0 {
		t.Fatal("an unbanned member's action was retried without its re-check")
	}
	for _, r := range k.Find(modtest.Other1M, store.ActRevoke, modtest.G1) {
		if r.Status != store.Failed || !strings.Contains(r.Reason, "not banned in") {
			t.Fatalf("unbanned row: %s %q", r.Status, r.Reason)
		}
	}
}

// TestStaleIntendedReportedAtStartup: the startup report lists every action
// left at intended, linked to those rows.
func TestStaleIntendedReportedAtStartup(t *testing.T) {
	k := modtest.New(t, "")
	k.Deliver(k.Spam("M1", modtest.G1))
	stale, err := k.Store.StaleIntended(k.Ctx)
	if err != nil || len(stale) == 0 {
		t.Fatalf("stale rows: %d %v", len(stale), err)
	}
	k.Reopen()
	n, err := ledger.Recover(k.Ctx, k.Store, k.Log)
	if err != nil || n != len(stale) {
		t.Fatalf("recover listed %d (%v), want %d", n, err, len(stale))
	}
	reps := k.Reports(ledger.KindStartup)
	if len(reps) != 1 || !strings.Contains(reps[0].Text, "Restarted with") {
		t.Fatalf("startup reports %+v", reps)
	}
	linked, err := k.Store.LedgerForReport(k.Ctx, reps[0].ID)
	if err != nil || len(linked) != len(stale) {
		t.Fatalf("startup report links %d rows (%v), want %d", len(linked), err, len(stale))
	}
	// A restart with nothing left over reports nothing.
	k.Fire()
	k.Reopen()
	if n, err := ledger.Recover(k.Ctx, k.Store, k.Log); n != 0 || err != nil {
		t.Fatalf("second recover: %d %v", n, err)
	}
}

// TestBanRowWithRemoveIntent: a ban's list entry commits in the same
// transaction as its removal rows: both or neither.
func TestBanRowWithRemoveIntent(t *testing.T) {
	k := modtest.New(t, "")
	k.Deliver(k.Spam("M1", modtest.G1))
	ban, ok, err := k.Store.FindBan(k.Ctx, modtest.SpammerM.IDs(), "")
	if err != nil || !ok {
		t.Fatalf("no ban: %v", err)
	}
	removes := k.Find(modtest.SpammerM, store.ActRemove, modtest.G1)
	if len(removes) != 1 || removes[0].Status != store.Intended {
		t.Fatalf("remove rows %+v", removes)
	}
	banRow, err := k.Store.Ledger(k.Ctx, ban.LedgerID)
	if err != nil || banRow.Action != store.ActBan || banRow.TriggerID != removes[0].TriggerID {
		t.Fatalf("ban entry points at %+v (%v), want the ban row of the same trigger", banRow, err)
	}
	// A transaction that fails after writing the plan leaves neither.
	p := ledger.Plan{Trigger: "T2", Target: modtest.Other1M, ConfigHash: "x", Ban: []string{store.BanEverywhere}, BanEnforce: true,
		Intents: []ledger.Intent{{Action: store.ActRemove, Chat: modtest.G1, Community: string(modtest.Community), Enforce: true}}}
	boom := errors.New("crash mid-transaction")
	err = k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
		if _, err := ledger.Write(k.Ctx, tx, p, k.Clock.Now()); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("write: %v", err)
	}
	if k.Banned(modtest.Other1M, "") || len(k.Rows(modtest.Other1M)) != 0 {
		t.Fatal("a failed transaction left a ban or a removal row behind")
	}
}

// TestWriteAdminBanOnBotBan (#8 AC 3.1): an admin's ban of someone the bot
// already banned writes the admin's ban row and moves their entry's link to
// it, with the number the entry lacked filled in; no entry is added, so the
// plan's "ban added" report is not written, and a ban a pause holds for them
// there is settled, as an added ban settles it.
func TestWriteAdminBanOnBotBan(t *testing.T) {
	k := modtest.New(t, "")
	write := func(p ledger.Plan) ledger.Written { t.Helper(); return writeBan(t, k, p) }
	if w := write(ledger.Plan{Trigger: "M1", Target: client.Member{LID: modtest.Spammer}, Reason: "spam post"}); w.BansAdded != 1 {
		t.Fatalf("the bot's ban: %+v, want one entry added", w)
	}
	var held int64
	if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) (err error) {
		held, _, err = store.InsertLedger(k.Ctx, tx, store.LedgerRow{Action: store.ActBan, Chat: store.BanEverywhere,
			Target: string(modtest.Spammer), TriggerID: "M2", Mode: store.ModeEnforce}, k.Clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w := write(ledger.Plan{Trigger: store.CLITrigger + "1", Target: modtest.SpammerM, Actor: "cli",
		Reason: "added with the ban command", Reports: []store.Report{{Kind: ledger.KindBanCLI,
			Community: store.BanEverywhere, Text: "A ban was added with the ban command."}}})
	if w.BansAdded != 0 || w.LinksMoved != 1 || w.New != 1 || len(w.ReportIDs) != 0 {
		t.Fatalf("written %+v, want no entry added, one link moved to the one new row, no report", w)
	}
	bans, err := k.Store.Bans(k.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bans) != 1 || bans[0].LedgerID != w.LedgerIDs[0] || bans[0].Phone != string(modtest.SpammerPhone) {
		t.Fatalf("bans %+v, want one entry linked to the admin's row %d, with the number", bans, w.LedgerIDs[0])
	}
	if reps := k.Reports(ledger.KindBanCLI); len(reps) != 0 {
		t.Fatalf("reports %+v, want no \"ban added\" report", reps)
	}
	if row, err := k.Store.Ledger(k.Ctx, held); err != nil || row.Status != store.Requested {
		t.Fatalf("held ban %+v (%v), want it settled", row, err)
	}
}

// writeBan writes p as a ban everywhere, applied at once.
func writeBan(t *testing.T, k *modtest.Kit, p ledger.Plan) ledger.Written {
	t.Helper()
	p.ConfigHash, p.BanCommunity = k.Holder.Current().Hash, store.BanEverywhere
	p.Ban, p.BanEnforce = []string{store.BanEverywhere}, true
	var w ledger.Written
	if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) (err error) {
		w, err = ledger.Write(k.Ctx, tx, p, k.Clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

// TestWriteOnAnEntryAlreadyThere: a ban of someone whose entry links an
// admin's row reuses that row and only fills in the address the entry lacks
// (Design item 2); and a plan whose only new row is a ban row an entry moved
// its link to (the entry's row was purged) keeps its evidence copy, which
// that row names, though it writes no report.
func TestWriteOnAnEntryAlreadyThere(t *testing.T) {
	t.Run("admin-link-kept-number-filled", func(t *testing.T) {
		k := modtest.New(t, "")
		first := writeBan(t, k, ledger.Plan{Trigger: store.AdminTrigger + "1:ban", Target: client.Member{LID: modtest.Spammer},
			Reason: "banned in the admin chat"})
		w := writeBan(t, k, ledger.Plan{Trigger: store.CLITrigger + "1", Target: modtest.SpammerM, Actor: "cli",
			Reason: "added with the ban command"})
		if w.BansAdded != 0 || w.LinksMoved != 0 || w.New != 0 {
			t.Fatalf("written %+v, want the admin's row reused: nothing added, moved or new", w)
		}
		bans, err := k.Store.Bans(k.Ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(bans) != 1 || bans[0].LedgerID != first.LedgerIDs[0] || bans[0].Phone != string(modtest.SpammerPhone) {
			t.Fatalf("bans %+v, want one entry still linked to the admin's row %d, its number filled in", bans,
				first.LedgerIDs[0])
		}
	})
	t.Run("moved-link-keeps-evidence", func(t *testing.T) {
		k := modtest.New(t, "")
		first := writeBan(t, k, ledger.Plan{Trigger: "M1", Target: modtest.SpammerM, Reason: "spam post"})
		if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(k.Ctx, `DELETE FROM ledger WHERE id = ?`, first.LedgerIDs[0])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		at := k.Clock.Now()
		w := writeBan(t, k, ledger.Plan{Trigger: "M2", Target: modtest.SpammerM, Reason: "spam post",
			Evidence: &store.Evidence{Chat: string(modtest.G1), Community: string(modtest.Community), Sender: string(modtest.Spammer),
				MsgID: "M2", TargetID: "M2", MsgTime: at, Fields: "{}", Normalised: "{}"},
			Reports: []store.Report{{Kind: ledger.KindLog, Community: string(modtest.Community), Text: "spam post"}}})
		if w.BansAdded != 0 || w.LinksMoved != 1 || w.New != 1 || len(w.ReportIDs) != 0 || w.EvidenceID == 0 {
			t.Fatalf("written %+v, want one link moved to the one new row, no report, the copy kept", w)
		}
		if _, ok, err := k.Store.Evidence(k.Ctx, w.EvidenceID); err != nil || !ok {
			t.Fatalf("evidence %d: %v (%v), want it kept", w.EvidenceID, ok, err)
		}
		if row, err := k.Store.Ledger(k.Ctx, w.LedgerIDs[0]); err != nil || row.EvidenceID != w.EvidenceID {
			t.Fatalf("ban row %+v (%v), want it to name evidence %d", row, err, w.EvidenceID)
		}
	})
}

// TestLiftInOneCommunity: a lift narrowed to one community removes only the
// bans covering it, applied or held by a pause, and its unban row names that
// community; a lift with no community removes every ban.
func TestLiftInOneCommunity(t *testing.T) {
	k := modtest.New(t, "bans:\n  scope: per_community\n")
	a, b := string(modtest.Community), modtest.SetB
	write := func(p ledger.Plan) {
		t.Helper()
		p.ConfigHash = k.Holder.Current().Hash
		if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
			_, err := ledger.Write(k.Ctx, tx, p, k.Clock.Now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	write(ledger.Plan{Trigger: "T1", Target: modtest.Other1M, Ban: []string{a, b}, BanEnforce: true, BanCommunity: a, Reason: "spam"})
	write(ledger.Plan{Trigger: "T2", Target: modtest.Other2M, Ban: []string{a, b}, BanEnforce: true, BanHeld: true, BanCommunity: a,
		Reason: "spam"})
	for _, m := range []client.Member{modtest.Other1M, modtest.Other2M} {
		write(ledger.Plan{Trigger: "L:" + m.Key(), Target: m, Lift: true, LiftIn: a, BanCommunity: a, Reason: "lifted in a"})
	}
	if k.Banned(modtest.Other1M, a) || !k.Banned(modtest.Other1M, b) {
		t.Fatalf("banned in %s: %v, in %s: %v; want only the ban in %s lifted", a, k.Banned(modtest.Other1M, a), b,
			k.Banned(modtest.Other1M, b), a)
	}
	if rows := k.Find(modtest.Other1M, store.ActUnban, modtest.Community); len(rows) != 1 {
		t.Fatalf("unban rows in %s: %+v", a, rows)
	}
	held := map[string]store.Status{}
	for _, r := range k.Rows(modtest.Other2M) {
		if r.Action == store.ActBan {
			held[r.Chat] = r.Status
		}
	}
	if held[a] != store.Overturned || held[b] != store.Intended {
		t.Fatalf("held bans %v, want the one in %s overturned and the one in %s still held", held, a, b)
	}
	write(ledger.Plan{Trigger: "L2", Target: modtest.Other1M, Lift: true, BanCommunity: a, Reason: "lifted everywhere"})
	if k.Banned(modtest.Other1M, "") {
		t.Fatal("a lift with no community left a ban")
	}
	if rows := k.Find(modtest.Other1M, store.ActUnban, ""); len(rows) != 1 {
		t.Fatalf("unban rows for every community: %+v", rows)
	}
}

// TestLiftOfANumbersHolder (#8 Gate 1): a lift that names a person by their
// LID keeps the ban of someone who held their number before, only in the
// community it lifts in, taking the number off it there; a lift whose target
// has no LID keeps nobody apart and lifts every ban of the number. Written
// counts the entries lifted and kept.
func TestLiftOfANumbersHolder(t *testing.T) {
	earlier := client.Member{LID: modtest.Other1, Phone: modtest.SpammerPhone}
	lift := func(k *modtest.Kit, p ledger.Plan) ledger.Written {
		t.Helper()
		p.Trigger, p.ConfigHash, p.Lift, p.Reason = "L1", k.Holder.Current().Hash, true, "lifted"
		var w ledger.Written
		if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) (err error) {
			w, err = ledger.Write(k.Ctx, tx, p, k.Clock.Now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return w
	}
	t.Run("in-one-community", func(t *testing.T) {
		k := modtest.New(t, "bans:\n  scope: per_community\n")
		a, b := string(modtest.Community), modtest.SetB
		for _, p := range []ledger.Plan{{Trigger: "T1", Target: earlier, Ban: []string{a, b}},
			{Trigger: "T2", Target: modtest.SpammerM, Ban: []string{a}}} {
			p.ConfigHash, p.BanEnforce, p.BanCommunity, p.Reason = k.Holder.Current().Hash, true, a, "spam"
			if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
				_, err := ledger.Write(k.Ctx, tx, p, k.Clock.Now())
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		w := lift(k, ledger.Plan{Target: modtest.SpammerM, LiftIn: a, BanCommunity: a})
		if w.BansLifted != 1 || w.NumbersReleased != 1 {
			t.Fatalf("written %+v, want one ban lifted and one kept", w)
		}
		bans, err := k.Store.Bans(k.Ctx)
		if err != nil {
			t.Fatal(err)
		}
		phones := map[string]string{}
		for _, ban := range bans {
			if ban.Member != string(modtest.Other1) {
				t.Fatalf("bans %+v, want only the earlier holder's", bans)
			}
			phones[ban.Scope] = ban.Phone
		}
		if len(phones) != 2 || phones[a] != "" || phones[b] != string(modtest.SpammerPhone) {
			t.Fatalf("the earlier holder's numbers by scope %v, want it off the ban in %s only", phones, a)
		}
	})
	t.Run("no-lid", func(t *testing.T) {
		k := modtest.New(t, "")
		k.Ban(earlier)
		w := lift(k, ledger.Plan{Target: client.Member{Phone: modtest.SpammerPhone}, BanCommunity: string(modtest.Community)})
		if w.BansLifted != 1 || w.NumbersReleased != 0 || k.Banned(modtest.Other1M, "") {
			t.Fatalf("written %+v, earlier holder banned %v; want their ban lifted with the number's",
				w, k.Banned(modtest.Other1M, ""))
		}
	})
}

// TestLedgerStampsConfigHash: every row carries the hash of the config that
// decided it; a reload changes the stamp on new rows only.
func TestLedgerStampsConfigHash(t *testing.T) {
	k := modtest.New(t, "")
	h1 := k.Holder.Current().Hash
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Reload("never_match: [\"chicken stock\"]\n")
	h2 := k.Holder.Current().Hash
	if h1 == h2 {
		t.Fatal("the reload did not change the hash")
	}
	k.Clock.Advance(time.Minute)
	k.Deliver(k.Msg("M2", modtest.GB, modtest.Member, modtest.SpamText))
	for _, r := range k.Rows(modtest.SpammerM) {
		if r.ConfigHash != h1 {
			t.Errorf("row %d stamped %s, want %s", r.ID, r.ConfigHash, h1)
		}
	}
	member := k.Rows(modtest.MemberM)
	if len(member) == 0 {
		t.Fatal("no rows for the second decision")
	}
	for _, r := range member {
		if r.ConfigHash != h2 {
			t.Errorf("row %d stamped %s, want %s", r.ID, r.ConfigHash, h2)
		}
	}
}

// TestSweepRetryDeduped: while a sweep keeps finding the same banned member
// and the removal keeps failing, that is one row and one report; a new
// episode starts after a removal went through.
func TestSweepRetryDeduped(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.Other1M)
	k.Dir.Apply(&client.GroupChange{Group: modtest.G1, Joined: []client.JID{modtest.Other1}})
	k.Fake.Results = map[client.JID]client.MemberStatus{modtest.Other1: client.MemberFailed}
	sweep := func(run string) {
		t.Helper()
		if err := k.Enforcer.CheckPresent(k.Ctx, modtest.G1, run); err != nil {
			t.Fatal(err)
		}
		k.Fire()
	}
	for _, run := range []string{"r1", "r2", "r3"} {
		sweep(run)
	}
	rows := k.Find(modtest.Other1M, store.ActRemove, modtest.G1)
	if len(rows) != 1 || rows[0].Status != store.Failed || rows[0].Attempts != 0 {
		t.Fatalf("rows %+v, want one failed row", rows)
	}
	if n := k.Fake.Count("Remove " + string(modtest.G1)); n != 3 {
		t.Fatalf("tried %d times, want 3 (one per sweep)", n)
	}
	if reps := k.Reports(ledger.KindBannedRejoin); len(reps) != 1 {
		t.Fatalf("%d reports for one episode, want 1", len(reps))
	}
	// The removal goes through: the episode ends; the next sighting is new.
	k.Fake.Results = nil
	sweep("r4")
	sweep("r5")
	rows = k.Find(modtest.Other1M, store.ActRemove, modtest.G1)
	if len(rows) != 2 || rows[0].Status != store.Requested || rows[1].Status != store.Requested {
		t.Fatalf("rows after success %+v", rows)
	}
	if reps := k.Reports(ledger.KindBannedRejoin); len(reps) != 2 {
		t.Fatalf("%d reports, want 2 (two episodes)", len(reps))
	}
}

var _ = context.Background

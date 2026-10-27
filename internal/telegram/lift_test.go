package telegram_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// TestLiftKeepsAnEarlierHoldersBan (#8, Expected Behavior 5): someone who
// held a number before is another person. While their ban still holds the
// number it covers whoever holds the number now; a lift that names the holder
// now ([Undo] on a report about them, an admin re-adding them by hand) lifts
// it for them and leaves the earlier holder banned on their LID, the number
// taken off their ban and its row. /unban <number> names the number and lifts
// every ban of it, the earlier holder's too.
func TestLiftKeepsAnEarlierHoldersBan(t *testing.T) {
	earlier := client.Member{LID: modtest.Other1, Phone: modtest.SpammerPhone}
	for name, c := range map[string]struct {
		lift          func(h *harness)
		earlierBanned bool
	}{
		"undo": {earlierBanned: true, lift: func(h *harness) {
			id := h.report(store.Report{Kind: ledger.KindAction, Community: string(modtest.Community),
				Subject: string(modtest.Spammer), Buttons: []string{ledger.ButtonUndo}, Text: "Spam in general matched rule pitch."})
			h.drain()
			h.press(adminUser, "undo", id)
		}},
		"human-re-add": {earlierBanned: true, lift: func(h *harness) {
			h.k.Clock.Advance(time.Minute)
			h.k.Deliver(&client.GroupChange{Group: modtest.G1, Actor: modtest.Admin, Time: h.k.Clock.Now(),
				Joined: []client.JID{modtest.Spammer}})
		}},
		"unban-number": {lift: func(h *harness) {
			h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "")
			h.k.Ban(earlier)
			if !h.k.Banned(modtest.SpammerM, "") {
				t.Fatalf("premise: the earlier holder's ban does not cover the number's holder now; bans %+v", h.bans())
			}
			c.lift(h)
			if h.k.Banned(modtest.SpammerM, "") {
				t.Fatalf("the number's holder now is still banned; bans %+v, reply %q", h.bans(), h.lastReply())
			}
			b, banned, err := h.k.Store.FindBan(h.k.Ctx, []string{string(modtest.Other1)}, "")
			if err != nil || banned != c.earlierBanned || banned && (b.Member != string(modtest.Other1) || b.Phone != "") {
				t.Fatalf("earlier holder's ban %+v, banned %v (%v); want banned %v, on their LID without the number",
					b, banned, err, c.earlierBanned)
			}
			if other, err := h.k.Store.OtherAddress(h.k.Ctx, string(modtest.SpammerPhone)); err != nil ||
				other == string(modtest.Other1) {
				t.Fatalf("the number still pairs with the earlier holder: %q (%v)", other, err)
			}
		})
	}
}

// TestUndoKeepsAnEarlierHoldersBanRow (#8 Gate 1): someone who held a number
// before was banned by the number and the ban resolved to their LID since, so
// its row names the number alone. [Undo] on a report about the number's
// holder now keeps that ban (TestLiftKeepsAnEarlierHoldersBan) and its row as
// it was, and the reply counts it as kept, not lifted.
func TestUndoKeepsAnEarlierHoldersBanRow(t *testing.T) {
	h := newHarness(t, "")
	h.k.Ban(client.Member{Phone: modtest.SpammerPhone})
	if err := h.k.Store.ResolveBan(h.k.Ctx, string(modtest.SpammerPhone), string(modtest.Other1)); err != nil {
		t.Fatal(err)
	}
	before := h.bans()
	if len(before) != 1 || before[0].Member != string(modtest.Other1) || before[0].Phone != string(modtest.SpammerPhone) {
		t.Fatalf("premise: bans %+v, want the earlier holder's ban on their LID with the number", before)
	}
	row, err := h.k.Store.Ledger(h.k.Ctx, before[0].LedgerID)
	if err != nil || row.Target != string(modtest.SpammerPhone) || row.Status == store.Overturned {
		t.Fatalf("premise: ban row %+v (%v), want one naming the number alone, not overturned", row, err)
	}
	id := h.report(store.Report{Kind: ledger.KindBannedRejoin, Community: string(modtest.Community),
		Subject: string(modtest.Spammer), Buttons: []string{ledger.ButtonUndo}, Text: "A banned member joined a group."})
	h.drain()
	h.press(adminUser, "undo", id)
	after := h.bans()
	if len(after) != 1 || after[0].Member != string(modtest.Other1) || after[0].Phone != "" ||
		after[0].LedgerID != row.ID {
		t.Fatalf("bans %+v, want the earlier holder's ban kept on their LID without the number, linking row %d", after,
			row.ID)
	}
	if got, err := h.k.Store.Ledger(h.k.Ctx, row.ID); err != nil || got.Status != row.Status {
		t.Fatalf("the kept ban's row is %+v (%v), want its status %q as before", got, err, row.Status)
	}
	if reply := h.lastReply(); !strings.Contains(reply, "(0 ban(s) lifted, 0 action(s) overturned). "+
		"1 ban(s) of someone who had their number before stay on that person, without the number. ") {
		t.Fatalf("reply %q, want no ban lifted, nothing overturned and one ban kept", reply)
	}
}

// TestLiftOfAnEarlierHolderKeepsTheHoldersBan (#8 Gate 1): someone who held a
// number before and its holder now are both banned with it. A lift of the
// earlier one ([Undo] on a report about them, an admin re-adding them by
// hand) finds the number is someone else's now (pipeline.Identify), so it
// lifts their ban alone: the holder's ban keeps the number and still covers
// it alone.
func TestLiftOfAnEarlierHolderKeepsTheHoldersBan(t *testing.T) {
	for name, lift := range map[string]func(h *harness){
		"undo": func(h *harness) {
			id := h.report(store.Report{Kind: ledger.KindAction, Community: string(modtest.Community),
				Subject: string(modtest.Other1), Buttons: []string{ledger.ButtonUndo}, Text: "Spam in general matched rule pitch."})
			h.drain()
			h.press(adminUser, "undo", id)
			if reply := h.lastReply(); !strings.Contains(reply, "(1 ban(s) lifted, ") || strings.Contains(reply, "stay on that person") {
				t.Fatalf("reply %q, want one ban lifted and none kept", reply)
			}
		},
		"human-re-add": func(h *harness) {
			h.k.Clock.Advance(time.Minute)
			h.k.Deliver(&client.GroupChange{Group: modtest.G1, Actor: modtest.Admin, Time: h.k.Clock.Now(),
				Joined: []client.JID{modtest.Other1}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "")
			h.k.Ban(client.Member{LID: modtest.Other1, Phone: modtest.SpammerPhone})
			h.k.Clock.Advance(time.Minute)
			h.k.Ban(modtest.SpammerM)
			if bans := h.bans(); len(bans) != 2 {
				t.Fatalf("premise: bans %+v, want the earlier holder's and the holder's", bans)
			}
			lift(h)
			if h.k.Banned(modtest.Other1M, "") {
				t.Fatalf("the earlier holder is still banned; bans %+v", h.bans())
			}
			b, banned, err := h.k.Store.FindBan(h.k.Ctx, []string{string(modtest.Spammer)}, "")
			if err != nil || !banned || b.Member != string(modtest.Spammer) || b.Phone != string(modtest.SpammerPhone) {
				t.Fatalf("holder's ban %+v, banned %v (%v); want it kept with the number", b, banned, err)
			}
			if !h.k.Banned(client.Member{Phone: modtest.SpammerPhone}, "") {
				t.Fatalf("the number alone is no longer banned; bans %+v", h.bans())
			}
			if other, err := h.k.Store.OtherAddress(h.k.Ctx, string(modtest.SpammerPhone)); err != nil ||
				other != string(modtest.Spammer) {
				t.Fatalf("the number pairs with %q (%v), want its holder %s", other, err, modtest.Spammer)
			}
		})
	}
}

// TestUndoOverturnsTheRowOfABanItLifts (#8 Gate 1): a ban by a number that
// resolved to its holder's LID since keeps a row naming the number alone. Once
// the number is someone else's, [Undo] on a report about the one banned finds
// them by their LID alone (pipeline.Identify), and still marks the row of the
// ban it lifts overturned.
func TestUndoOverturnsTheRowOfABanItLifts(t *testing.T) {
	h := newHarness(t, "")
	h.k.Ban(client.Member{Phone: modtest.SpammerPhone})
	if err := h.k.Store.ResolveBan(h.k.Ctx, string(modtest.SpammerPhone), string(modtest.Other1)); err != nil {
		t.Fatal(err)
	}
	bans := h.bans()
	if len(bans) != 1 || bans[0].Member != string(modtest.Other1) {
		t.Fatalf("premise: bans %+v, want one on the earlier holder's LID", bans)
	}
	rowID := bans[0].LedgerID
	id := h.report(store.Report{Kind: ledger.KindAction, Community: string(modtest.Community),
		Subject: string(modtest.Other1), Buttons: []string{ledger.ButtonUndo}, Text: "Spam in general matched rule pitch."})
	h.drain()
	h.press(adminUser, "undo", id)
	if bans := h.bans(); len(bans) != 0 {
		t.Fatalf("bans %+v, want none", bans)
	}
	if row, err := h.k.Store.Ledger(h.k.Ctx, rowID); err != nil || row.Target != string(modtest.SpammerPhone) ||
		row.Status != store.Overturned {
		t.Fatalf("the lifted ban's row %+v (%v), want the one naming the number, overturned", row, err)
	}
	if reply := h.lastReply(); !strings.Contains(reply, "(1 ban(s) lifted, 1 action(s) overturned). Th") {
		t.Fatalf("reply %q, want one ban lifted and its row overturned", reply)
	}
}

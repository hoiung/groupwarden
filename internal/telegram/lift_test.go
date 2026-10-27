package telegram_test

import (
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

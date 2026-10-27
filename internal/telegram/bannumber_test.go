package telegram_test

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// spammerNumber is the kit spammer's phone number (modtest.SpammerPhone) as
// an admin types it. The tests build its other spellings from it at run
// time, so only the synthetic form listed in .secret-pii-allowlist is in the
// source.
const spammerNumber = "+44 7700 900123"

// spellings are the ways an admin may write spammerNumber.
func spellings() map[string]string {
	plus := strings.ReplaceAll(spammerNumber, " ", "")
	return map[string]string{
		"plus":        plus,
		"spaces":      spammerNumber,
		"dashes":      strings.ReplaceAll(spammerNumber, " ", "-"),
		"brackets":    strings.Replace(spammerNumber, " 7700 ", " (7700) ", 1),
		"digits-only": strings.TrimPrefix(plus, "+"),
		// The national 0 some write in brackets after the country code.
		"trunk-zero": strings.Replace(spammerNumber, " 7700 ", " (0)7700 ", 1),
		// A 0 in brackets anywhere else is one of the number's digits.
		"bracketed-zero": strings.Replace(spammerNumber, "900123", "90(0)123", 1),
	}
}

// bans lists the whole ban list.
func (h *harness) bans() []store.Ban {
	h.t.Helper()
	bans, err := h.k.Store.Bans(h.k.Ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return bans
}

// db is a handle of the test's own on the kit's database.
func (h *harness) db() *sql.DB {
	h.t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.k.DBPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = db.Close() })
	return db
}

// ledgerRows counts every ledger row.
func (h *harness) ledgerRows() int {
	h.t.Helper()
	var n int
	if err := h.db().QueryRowContext(h.k.Ctx, `SELECT COUNT(*) FROM ledger`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

// post delivers an ordinary message (no rule matches it) from sender in chat.
func (h *harness) post(id string, chat, sender, alt client.JID) {
	h.t.Helper()
	m := h.k.Msg(id, chat, sender, "hello everyone, see you at the meetup")
	m.SenderAlt = alt
	h.k.Deliver(m)
}

// cliBan bans the kit spammer's phone number everywhere as `groupwarden ban
// add <phone>` does: keyed by the number, with no sweep asked for.
func (h *harness) cliBan() {
	h.t.Helper()
	h.cliBanOf(client.Member{Phone: modtest.SpammerPhone})
}

// cliBanOf bans m everywhere as `groupwarden ban add` does with m's address.
func (h *harness) cliBanOf(m client.Member) {
	h.t.Helper()
	p := ledger.Plan{Trigger: store.CLITrigger + "1", Target: m, Actor: "cli",
		BanCommunity: store.BanEverywhere, Ban: []string{store.BanEverywhere}, BanEnforce: true,
		Reason: "added with the ban command"}
	if err := h.k.Store.Write(h.k.Ctx, func(tx *sql.Tx) error {
		_, err := ledger.Write(h.k.Ctx, tx, p, h.k.Clock.Now())
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
}

// revoked reports whether the bot deleted message id in chat (sent once).
func (h *harness) revoked(chat client.JID, id string) bool {
	h.t.Helper()
	n := 0
	for _, c := range h.k.Fake.Calls() {
		if strings.HasPrefix(c, "Revoke "+string(chat)+" ") && strings.HasSuffix(c, " "+id) {
			n++
		}
	}
	if n > 1 {
		h.t.Fatalf("message %s in %s deleted %d times: %v", id, chat, n, h.k.Fake.Calls())
	}
	return n == 1
}

// TestBanByNumber (AC 5.1): /ban <number> bans a phone number written with
// its country code, with or without "+", spaces, dashes or brackets, and
// stores it as `groupwarden ban add <phone>` does (cmd/groupwarden
// membercmds.go ban: the phone JID is the key until the LID is found, scope
// everywhere under bans.scope all_communities), with a ban row triggered
// from the admin chat that names who sent it; a LID the bot knows (here from
// the group list) resolves the ban at once, as the sweep would. A
// number of fewer than 8 or more than 15 digits, or with letters, gets the
// usage text and bans nobody; a number already banned everywhere gets a reply
// saying so and no second entry (the admin's ban is recorded on it), and its
// posts are still deleted (every /ban cleans up); /ban as a reply to a report
// works as before.
func TestBanByNumber(t *testing.T) {
	phone := string(modtest.SpammerPhone)
	for name, number := range spellings() {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "")
			h.command(telegramtest.ChatID, adminUser, "/ban "+number, 0)
			if got := h.lastReply(); !contains(got, "Banned by Ann: phone…0123 is banned everywhere.") {
				t.Fatalf("reply %q", got)
			}
			bans := h.bans()
			if len(bans) != 1 {
				t.Fatalf("bans %+v, want one", bans)
			}
			// The group list knows their LID, so the ban is resolved at once,
			// as the sweep would resolve a `ban add` of the number.
			b, lid := bans[0], string(modtest.Spammer)
			if b.Member != lid || b.LID != lid || b.Phone != phone || b.Scope != store.BanEverywhere || b.LedgerID == 0 {
				t.Fatalf("ban %+v, want it keyed by %s with the number %s, everywhere", b, lid, phone)
			}
		})
	}
	digits := strings.TrimPrefix(strings.ReplaceAll(spammerNumber, " ", ""), "+")
	for name, number := range map[string]string{
		"too-short": "+" + digits[:7],
		"too-long":  "+" + digits + "4567",
		"letters":   strings.Replace(spammerNumber, "900123", "9001AB", 1),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "")
			h.command(telegramtest.ChatID, adminUser, "/ban "+number, 0)
			if got := h.lastReply(); !strings.HasPrefix(got, "Usage: /ban <number>") {
				t.Fatalf("reply to /ban %s: %q, want the usage text", number, got)
			}
			if bans := h.bans(); len(bans) != 0 || h.ledgerRows() != 0 {
				t.Fatalf("bans %+v, %d ledger rows after /ban %s, want none", bans, h.ledgerRows(), number)
			}
		})
	}
	// The bot banned them (the kit's ban is not an admin's): the admin's ban
	// is recorded on their one entry, which carries the number.
	t.Run("already-banned-still-cleans-up", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.k.Ban(client.Member{LID: modtest.Spammer})
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); !strings.HasPrefix(got, "phone…0123 is already banned everywhere, so no ban was added. "+
			"Their posts on record: 1 being deleted, 0 too old to delete. The bot sweeps every group now") {
			t.Fatalf("reply %q", got)
		}
		bans := h.bans()
		if len(bans) != 1 || bans[0].Member != string(modtest.Spammer) || bans[0].Phone != string(modtest.SpammerPhone) {
			t.Fatalf("bans %+v, want their one entry with the number", bans)
		}
		if row, err := h.k.Store.Ledger(h.k.Ctx, bans[0].LedgerID); err != nil ||
			!strings.HasPrefix(row.TriggerID, store.AdminBanTrigger) {
			t.Fatalf("the entry links to %+v (%v), want the admin's ban row (%s…)", row, err, store.AdminBanTrigger)
		}
		if b, ok, err := h.k.Store.FindBan(h.k.Ctx, []string{string(modtest.SpammerPhone)}, ""); err != nil || !ok ||
			b.Member != bans[0].Member {
			t.Fatalf("FindBan by the number alone: %+v %v (%v), want their entry", b, ok, err)
		}
		h.k.Fire()
		if !h.revoked(modtest.G1, "P1") {
			t.Fatalf("calls %v, want P1 deleted", h.k.Fake.Calls())
		}
	})
	t.Run("ledger-names-sender", func(t *testing.T) {
		h := newHarness(t, "")
		h.command(telegramtest.ChatID, secondAdm, "/ban "+spammerNumber, 0)
		rows := h.k.Rows(client.Member{Phone: modtest.SpammerPhone})
		if len(rows) != 1 {
			t.Fatalf("rows %+v, want the ban row", rows)
		}
		r := rows[0]
		by := "telegram:" + strconv.FormatInt(secondAdm, 10) + " (Bea)"
		if r.Action != store.ActBan || r.Status != store.Requested || r.Actor != by ||
			!strings.HasPrefix(r.TriggerID, store.AdminBanTrigger) || !strings.Contains(r.Reason, by) {
			t.Fatalf("ban row %+v, want an admin-chat ban (trigger %s…) naming %s", r, store.AdminBanTrigger, by)
		}
		if bans := h.bans(); len(bans) != 1 || bans[0].LedgerID != r.ID {
			t.Fatalf("bans %+v, want the one ban row %d stands for", bans, r.ID)
		}
	})
	t.Run("reply-to-report-unchanged", func(t *testing.T) {
		h := newHarness(t, "")
		r := watchOnly(t, h)
		h.command(telegramtest.ChatID, adminUser, "/ban", h.head(r.ID))
		reply := h.lastReply()
		h.k.Fire()
		if !strings.HasPrefix(reply, "Banned by Ann (#"+strconv.FormatInt(r.ID, 10)+"): the message is being deleted") ||
			!h.k.Banned(modtest.SpammerM, "") || !h.revoked(modtest.G1, "W1") || h.k.Fake.Count("Remove") != 2 {
			t.Fatalf("/ban as a reply: %q, banned %v, calls %v", reply, h.k.Banned(modtest.SpammerM, ""), h.k.Fake.Calls())
		}
		// The same press as [Ban]: a second one is answered with the first.
		h.press(secondAdm, "ban", r.ID)
		if !strings.Contains(h.answers()[0], "already done by Ann") {
			t.Fatalf("[Ban] after /ban answered %q", h.answers())
		}
	})
	t.Run("per-community-needs-cli", func(t *testing.T) {
		h := newHarness(t, "bans:\n  scope: per_community\n")
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); !strings.Contains(got, "bans.scope is per_community") || len(h.bans()) != 0 {
			t.Fatalf("reply %q, bans %+v", got, h.bans())
		}
	})
	t.Run("own-number", func(t *testing.T) {
		h := newHarness(t, "")
		bot := "+" + strings.TrimSuffix(string(modtest.BotPhone), "@s.whatsapp.net")
		h.command(telegramtest.ChatID, adminUser, "/ban "+bot, 0)
		if got := h.lastReply(); !strings.Contains(got, "the bot's own number") || len(h.bans()) != 0 {
			t.Fatalf("reply %q, bans %+v", got, h.bans())
		}
	})
	// A current admin's number is refused, as [Ban] refuses one: an admin
	// must decide.
	t.Run("current-admin", func(t *testing.T) {
		h := newHarness(t, "")
		admin := "+" + strings.TrimSuffix(string(modtest.AdminPhone), "@s.whatsapp.net")
		h.command(telegramtest.ChatID, adminUser, "/ban "+admin, 0)
		if got := h.lastReply(); !strings.HasPrefix(got, "Not banned: phone…0456 is a current admin") ||
			len(h.bans()) != 0 || h.ledgerRows() != 0 {
			t.Fatalf("reply %q, bans %+v, %d ledger rows", got, h.bans(), h.ledgerRows())
		}
	})
	// A number that someone else held before: the record of a post the bot
	// reported pairs it with that old owner's LID. The ban goes on its current
	// holder's LID, which the saved WhatsApp IDs give (in the first) or,
	// with none saved, WhatsApp (in the second): the bot's records come last.
	// The group list does not pair the number (they joined since it was read).
	for name, current := range map[string]func(h *harness){
		"number-moved-to-someone-else": func(h *harness) {
			h.k.Fake.Saved = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
		},
		"whatsapp-beats-records": func(h *harness) {
			h.k.Fake.LIDs = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "")
			recentJoiner(h)
			oldOwnerRecord(h)
			current(h)
			h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
			if bans := h.bans(); len(bans) != 1 || bans[0].Member != string(modtest.Spammer) ||
				bans[0].Phone != string(modtest.SpammerPhone) {
				t.Fatalf("bans %+v, want the ban on the number's current holder %s", bans, modtest.Spammer)
			}
		})
	}
}

// oldOwnerRecord records a post the bot reported from modtest.Other1 that
// carried the kit spammer's number: someone who held the number before.
func oldOwnerRecord(h *harness) {
	h.t.Helper()
	at := h.k.Clock.Now()
	if err := h.k.Store.Write(h.k.Ctx, func(tx *sql.Tx) error {
		_, err := store.InsertEvidence(h.k.Ctx, tx, store.Evidence{Chat: string(modtest.G1), Community: string(modtest.Community),
			Sender: string(modtest.Other1), SenderAlt: string(modtest.SpammerPhone), Subject: string(modtest.Other1), MsgID: "O1",
			TargetID: "O1", MsgTime: at, Fields: "{}", Normalised: "{}"}, at)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
}

// TestUnbanByNumber (AC 5.2): /unban <number> lifts the bans of a phone
// number, still keyed by the phone or already resolved to a LID, with an
// unban row naming who sent it, and adds nobody back to a group. A number of
// 8 or more digits is a phone number; "#N" or a shorter number is a report
// number, as before.
func TestUnbanByNumber(t *testing.T) {
	t.Run("phone-key", func(t *testing.T) {
		h := newHarness(t, "")
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
		got := h.lastReply()
		if !strings.HasPrefix(got, "Unbanned by Ann: phone…0123 is unbanned everywhere (1 ban(s) lifted).") ||
			!strings.Contains(got, "not added back to any group") {
			t.Fatalf("reply %q", got)
		}
		if bans := h.bans(); len(bans) != 0 {
			t.Fatalf("bans %+v after /unban", bans)
		}
	})
	t.Run("lid-resolved", func(t *testing.T) {
		h := newHarness(t, "")
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if err := h.k.Store.ResolveBan(h.k.Ctx, string(modtest.SpammerPhone), string(modtest.Spammer)); err != nil {
			t.Fatal(err)
		}
		if bans := h.bans(); len(bans) != 1 || bans[0].Member != string(modtest.Spammer) {
			t.Fatalf("bans %+v, want the ban re-keyed on the LID", bans)
		}
		h.command(telegramtest.ChatID, adminUser, "/unban "+strings.ReplaceAll(spammerNumber, " ", ""), 0)
		if got := h.lastReply(); !strings.HasPrefix(got, "Unbanned by Ann:") || len(h.bans()) != 0 ||
			h.k.Banned(modtest.SpammerM, "") {
			t.Fatalf("reply %q, bans %+v", got, h.bans())
		}
	})
	// The deletes a ban queued and the bot has not sent yet are cancelled
	// (never sent, however often the outbox fires); a later /ban plans them
	// again.
	t.Run("cancels-queued-deletes-reban-plans-again", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.post("P2", modtest.G2, modtest.Spammer, modtest.SpammerPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
		for i := 0; i < 5; i++ {
			h.k.Fire()
		}
		if n := h.k.Fake.Count("Revoke"); n != 0 {
			t.Fatalf("calls %v: %d deletes sent after /unban, want none", h.k.Fake.Calls(), n)
		}
		for _, r := range h.k.Rows(client.Member{Phone: modtest.SpammerPhone}) {
			if r.Action == store.ActRevoke && r.Status != store.Overturned {
				t.Fatalf("delete row %+v after /unban, want it overturned", r)
			}
		}
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if reply := h.lastReply(); !strings.Contains(reply, "Their posts on record: 2 being deleted") {
			t.Fatalf("reply %q, want both posts planned again", reply)
		}
		h.k.Fire()
		if !h.revoked(modtest.G1, "P1") || !h.revoked(modtest.G2, "P2") {
			t.Fatalf("calls %v, want P1 and P2 deleted after the second /ban", h.k.Fake.Calls())
		}
	})
	t.Run("not-banned", func(t *testing.T) {
		h := newHarness(t, "")
		h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
		if got := h.lastReply(); got != "phone…0123 is not banned; nothing changed." || h.ledgerRows() != 0 {
			t.Fatalf("reply %q, %d ledger rows", got, h.ledgerRows())
		}
	})
	for name, prefix := range map[string]string{"report-number-hash": "#", "report-number-bare": ""} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "")
			r := h.spam("U1")
			h.drain()
			h.command(telegramtest.ChatID, adminUser, "/unban "+prefix+strconv.FormatInt(r.ID, 10), 0)
			if got := h.lastReply(); !strings.HasPrefix(got, "Undone by Ann (#"+strconv.FormatInt(r.ID, 10)+")") ||
				h.k.Banned(modtest.SpammerM, "") {
				t.Fatalf("/unban %s%d: %q", prefix, r.ID, got)
			}
		})
	}
	t.Run("ledger-names-sender", func(t *testing.T) {
		h := newHarness(t, "")
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.command(telegramtest.ChatID, secondAdm, "/unban "+spammerNumber, 0)
		by := "telegram:" + strconv.FormatInt(secondAdm, 10) + " (Bea)"
		var unban []store.LedgerRow
		for _, r := range h.k.Rows(modtest.SpammerM) {
			if r.Action == store.ActUnban {
				unban = append(unban, r)
			}
		}
		if len(unban) != 1 || unban[0].Status != store.Requested || unban[0].Actor != by ||
			!strings.HasPrefix(unban[0].TriggerID, store.AdminUnbanTrigger) || !strings.Contains(unban[0].Reason, by) {
			t.Fatalf("unban rows %+v, want one from the admin chat naming %s", unban, by)
		}
	})
	t.Run("usage", func(t *testing.T) {
		h := newHarness(t, "")
		for _, arg := range []string{"", "#x", strings.Replace(spammerNumber, "900123", "9001AB", 1), "0" + strings.TrimPrefix(strings.ReplaceAll(spammerNumber, " ", ""), "+44")} {
			h.command(telegramtest.ChatID, adminUser, "/unban "+arg, 0)
			if got := h.lastReply(); !strings.HasPrefix(got, "Usage: /unban <number>") {
				t.Fatalf("/unban %q: %q, want the usage text", arg, got)
			}
		}
	})
}

// TestBanDeletesPosts (AC 5.4): every /ban deletes the banned person's posts
// the bot still has a record of (the seen table's sender, store migration
// 12), through the outbox, in every group the ban covers: none of anyone
// else's, none recorded before the migration, and a post too old for an
// admin delete is recorded failed, reported once in the reply and never
// sent.
func TestBanDeletesPosts(t *testing.T) {
	t.Run("deletes-recorded", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.post("P2", modtest.G2, modtest.Spammer, modtest.SpammerPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); !strings.Contains(got, "Their posts on record: 2 being deleted, 0 too old to delete.") {
			t.Fatalf("reply %q", got)
		}
		if queued, err := h.k.Store.OutboxLen(h.k.Ctx); err != nil || queued != 2 {
			t.Fatalf("%d queued (%v), want the 2 deletes in the outbox", queued, err)
		}
		h.k.Fire()
		if !h.revoked(modtest.G1, "P1") || !h.revoked(modtest.G2, "P2") {
			t.Fatalf("calls %v, want P1 and P2 deleted", h.k.Fake.Calls())
		}
		for _, c := range []string{"Revoke " + string(modtest.G1) + " " + string(modtest.Spammer) + " P1",
			"Revoke " + string(modtest.G2) + " " + string(modtest.Spammer) + " P2"} {
			if h.k.Fake.Count(c) != 1 {
				t.Fatalf("calls %v, want %q (the post's author as the group knows them)", h.k.Fake.Calls(), c)
			}
		}
		if rows := rowsOf(h, client.Member{Phone: modtest.SpammerPhone}); rows["revoke/requested"] != 2 {
			t.Fatalf("rows %v, want 2 deletes sent", rows)
		}
	})
	// A reply the bot could not decrypt is a post of its own: the ban deletes
	// it by its ID. A secret edit it could not decrypt is not (its ID is not
	// the post it edits).
	t.Run("unreadable-reply-deleted-edit-not", func(t *testing.T) {
		h := newHarness(t, "")
		for id, isEdit := range map[string]bool{"UC1": false, "UE1": true} {
			h.k.Deliver(&client.Undecryptable{Chat: modtest.G1, Sender: modtest.Spammer, SenderAlt: modtest.SpammerPhone,
				ID: id, IsEdit: isEdit, Time: h.k.Clock.Now(), Reason: client.ReasonDecryptError, Detail: "synthetic"})
		}
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.k.Fire()
		if !h.revoked(modtest.G1, "UC1") || h.revoked(modtest.G1, "UE1") {
			t.Fatalf("calls %v, want UC1 deleted and UE1 not", h.k.Fake.Calls())
		}
	})
	// The bot's own delete of a post, queued while paused, still goes out
	// after the rule is relaxed when an admin banned the author meanwhile:
	// the ban's delete folded into the bot's row. Without the admin's ban,
	// the relaxed rule keeps the post, also when the bot's own ban of the
	// author is on the ban list (no pause held it), and when the admin's
	// ban can no longer show who made it (its ledger row purged past
	// retention): the queue goes on, with a Warn. The admin's ban is found
	// for someone who joined since the group list was last read too: from
	// the admin chat it is resolved on their LID at once when their post is
	// on record; from the admin chat before their post (nothing pairs the
	// number with their LID yet and WhatsApp has none) and from the command
	// line, which asks for no sweep, it names their number, and the post they
	// sent pairs the two for the bot's row, which names their LID. An admin's
	// ban of someone the bot had already banned (no pause: the bot's ban is on
	// the list) is recorded on their entry, from the admin chat, [Ban] and the
	// command line, also when the bot's ban row was purged (#8 AC 3.1, 3.3);
	// so is the command line's ban by number that the sweep merges with the
	// bot's ban of a post sent under their LID alone (#8 AC 3.2).
	for name, c := range map[string]struct {
		pause, adminBan, banFirst, cliBan, cliFirst, cliLID, button, joiner, lidOnly, merge, purgeFirst, purge, deleted bool
	}{
		"admin-banned":                          {pause: true, adminBan: true, deleted: true},
		"bot-only":                              {pause: true},
		"bot-ban-on-list":                       {},
		"admin-ban-ledger-purged":               {pause: true, adminBan: true, purge: true},
		"admin-banned-recent-joiner":            {pause: true, adminBan: true, joiner: true, deleted: true},
		"admin-banned-unresolved-recent-joiner": {pause: true, adminBan: true, banFirst: true, joiner: true, deleted: true},
		"cli-banned-recent-joiner":              {pause: true, cliBan: true, joiner: true, deleted: true},
		"chat-after-bot-ban":                    {adminBan: true, deleted: true},
		"button-after-bot-ban":                  {button: true, deleted: true},
		"cli-after-bot-ban":                     {cliLID: true, deleted: true},
		"chat-after-bot-ban-row-purged":         {purgeFirst: true, adminBan: true, deleted: true},
		"cli-number-ban-then-bot-ban-merged":    {cliFirst: true, joiner: true, lidOnly: true, merge: true, deleted: true},
	} {
		t.Run("rule-relaxed-while-queued/"+name, func(t *testing.T) {
			h := newHarness(t, "")
			if c.joiner {
				recentJoiner(h)
			}
			if c.pause {
				if err := h.k.Store.SetPause(h.k.Ctx, store.Pause{Source: store.SourceAdmin, Scope: store.ScopeAll,
					Reason: "paused", Since: h.k.Clock.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			if c.banFirst {
				h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
				if b := h.bans(); len(b) != 1 || b[0].LID != "" || b[0].Phone != string(modtest.SpammerPhone) {
					t.Fatalf("bans %+v, want one keyed by the number alone", b)
				}
			}
			if c.cliFirst {
				h.cliBan()
			}
			var watched store.Report
			if c.button {
				watched = watchOnly(t, h)
			}
			spam := h.k.Spam("S1", modtest.G1)
			if c.lidOnly {
				spam.SenderAlt = ""
			}
			h.k.Deliver(spam)
			if c.purgeFirst {
				if n, err := h.k.Store.PurgeLedger(h.k.Ctx, h.k.Clock.Now().Add(time.Second)); err != nil || n == 0 {
					t.Fatalf("purged %d (%v), want the bot's ban row gone", n, err)
				}
			}
			if c.adminBan && !c.banFirst {
				h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
			}
			if c.cliBan {
				h.cliBan()
			}
			if c.cliLID {
				h.cliBanOf(client.Member{LID: modtest.Spammer})
			}
			if c.button {
				h.press(adminUser, "ban", watched.ID)
			}
			if c.merge {
				// The command line's ban names the number, the bot's their LID:
				// two entries until the sweep finds the LID in the saved IDs.
				if b := h.bans(); len(b) != 2 {
					t.Fatalf("bans %+v, want the number's and the LID's", b)
				}
				h.k.Fake.Saved = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
				if _, err := h.app.Sweep.Run(h.k.Ctx, "r1"); err != nil {
					t.Fatal(err)
				}
			}
			if c.adminBan || c.cliLID || c.button || c.merge {
				// Their one entry, linked to the admin's ban row.
				b := h.bans()
				if len(b) != 1 {
					t.Fatalf("bans %+v, want their one entry", b)
				}
				if row, err := h.k.Store.Ledger(h.k.Ctx, b[0].LedgerID); err != nil ||
					!strings.HasPrefix(row.TriggerID, store.AdminTrigger) && !strings.HasPrefix(row.TriggerID, store.CLITrigger) {
					t.Fatalf("the entry links to %+v (%v), want an admin's ban row", row, err)
				}
			}
			if !c.pause {
				if bans, err := h.k.Store.BansFor(h.k.Ctx, modtest.SpammerM.IDs()); err != nil || len(bans) == 0 {
					t.Fatalf("bans %v (%v), want the bot's own ban on the list", bans, err)
				}
			}
			if c.purge {
				if n, err := h.k.Store.PurgeLedger(h.k.Ctx, h.k.Clock.Now().Add(time.Second)); err != nil || n == 0 {
					t.Fatalf("purged %d (%v), want the admin's ban row gone", n, err)
				}
				bans, err := h.k.Store.BansFor(h.k.Ctx, modtest.SpammerM.IDs())
				if err != nil || len(bans) != 1 || bans[0].LedgerID == 0 {
					t.Fatalf("bans %v (%v), want the admin's ban kept with its purged ledger ID", bans, err)
				}
			}
			h.k.ReloadConfig(strings.Replace(modtest.Config, "      action: delete_remove_ban\n      confirmed: true",
				"      action: log", 1))
			if c.pause {
				if err := h.k.Store.ClearPause(h.k.Ctx, store.SourceAdmin); err != nil {
					t.Fatal(err)
				}
			}
			h.k.Fire()
			if got := h.revoked(modtest.G1, "S1"); got != c.deleted {
				t.Fatalf("S1 deleted %v, want %v; calls %v, rows %v", got, c.deleted, h.k.Fake.Calls(),
					rowsOf(h, modtest.SpammerM))
			}
			if n := h.k.Logged("a ban's ledger row is gone; not counted as an admin's ban"); (n > 0) != c.purge {
				t.Fatalf("%d gone-ledger-row warnings, want them only when the row was purged", n)
			}
		})
	}
	t.Run("skips-other-senders", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.post("M1", modtest.G1, modtest.Member, "")
		h.post("A1", modtest.G2, modtest.Admin, modtest.AdminPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.k.Fire()
		if !h.revoked(modtest.G1, "P1") || h.revoked(modtest.G1, "M1") || h.revoked(modtest.G2, "A1") ||
			h.k.Fake.Count("Revoke") != 1 {
			t.Fatalf("calls %v, want the spammer's post deleted and nobody else's", h.k.Fake.Calls())
		}
	})
	t.Run("skips-pre-migration-rows", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		// Every row written before migration 12 has no sender.
		if _, err := h.db().ExecContext(h.k.Ctx, `UPDATE seen SET sender = ''`); err != nil {
			t.Fatal(err)
		}
		h.post("P2", modtest.G2, modtest.Spammer, modtest.SpammerPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.k.Fire()
		if !strings.Contains(h.lastReply(), "Their posts on record: 1 being deleted") || h.revoked(modtest.G1, "P1") ||
			!h.revoked(modtest.G2, "P2") {
			t.Fatalf("reply %q, calls %v, want only the post decided after the migration", h.lastReply(), h.k.Fake.Calls())
		}
	})
	t.Run("refused-reported-once", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("OLD", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.k.Clock.Advance(47*time.Hour + time.Minute) // past act_on_replay_max_age, within the seen table's 72 hours
		h.post("NEW", modtest.G2, modtest.Spammer, modtest.SpammerPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); !strings.Contains(got,
			"Their posts on record: 1 being deleted, 1 too old to delete (older than act_on_replay_max_age).") {
			t.Fatalf("reply %q", got)
		}
		var old []store.LedgerRow
		for _, r := range h.k.Rows(client.Member{Phone: modtest.SpammerPhone}) {
			if r.Action == store.ActRevoke && r.MsgID == "OLD" {
				old = append(old, r)
			}
		}
		if len(old) != 1 || old[0].Status != store.Failed || !strings.Contains(old[0].Reason, "older than act_on_replay_max_age") {
			t.Fatalf("rows for the old post %+v, want one recorded failed", old)
		}
		for i := 0; i < 3; i++ {
			h.k.Fire()
			h.k.Clock.Advance(time.Hour)
		}
		if h.revoked(modtest.G1, "OLD") || !h.revoked(modtest.G2, "NEW") {
			t.Fatalf("calls %v, want the old post never sent, the new one deleted", h.k.Fake.Calls())
		}
		// Reported once: in the reply, with no report of its own, and a /ban
		// again changes nothing.
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); !strings.Contains(got, "already banned") || len(h.k.Reports(ledger.KindNotDone)) != 0 {
			t.Fatalf("reply %q, not-done reports %+v", got, h.k.Reports(ledger.KindNotDone))
		}
		if rows := rowsOf(h, client.Member{Phone: modtest.SpammerPhone}); rows["revoke/failed"] != 1 || rows["revoke/requested"] != 1 {
			t.Fatalf("rows %v", rows)
		}
	})
	t.Run("scope-per-community", func(t *testing.T) {
		h := newHarness(t, "bans:\n  scope: per_community\n")
		h.post("P2", modtest.G2, modtest.Spammer, modtest.SpammerPhone)
		h.post("P3", modtest.GB, modtest.Spammer, modtest.SpammerPhone)
		r := watchOnly(t, h)
		h.command(telegramtest.ChatID, adminUser, "/ban", h.head(r.ID))
		h.k.Fire()
		if !strings.Contains(h.lastReply(), "their other posts on record: 1 being deleted") ||
			!h.revoked(modtest.G1, "W1") || !h.revoked(modtest.G2, "P2") || h.revoked(modtest.GB, "P3") {
			t.Fatalf("reply %q, calls %v, want the report's community only", h.lastReply(), h.k.Fake.Calls())
		}
		if bans := h.bans(); len(bans) != 1 || bans[0].Scope != string(modtest.Community) {
			t.Fatalf("bans %+v, want the community's own", bans)
		}
	})
	t.Run("scope-all", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.post("P3", modtest.GB, modtest.Spammer, modtest.SpammerPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.k.Fire()
		if !strings.Contains(h.lastReply(), "Their posts on record: 2 being deleted") || !h.revoked(modtest.G1, "P1") ||
			!h.revoked(modtest.GB, "P3") {
			t.Fatalf("reply %q, calls %v, want every community's", h.lastReply(), h.k.Fake.Calls())
		}
	})
	// In a group that addresses members by phone number, the delete names
	// the phone number, as the moderator's own delete of that post would.
	t.Run("phone-addressed-group", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.SpammerPhone, modtest.Spammer)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.k.Fire()
		if c := "Revoke " + string(modtest.G1) + " " + string(modtest.SpammerPhone) + " P1"; h.k.Fake.Count(c) != 1 ||
			h.k.Fake.Count("Revoke") != 1 {
			t.Fatalf("calls %v, want %q", h.k.Fake.Calls(), c)
		}
	})
	// A post is found by either address its row keeps: here the group knew
	// the author by a LID the ban does not name, with the banned number as
	// their other address.
	t.Run("found-by-other-address", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.Other1, modtest.SpammerPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.k.Fire()
		if c := "Revoke " + string(modtest.G1) + " " + string(modtest.Other1) + " P1"; h.k.Fake.Count(c) != 1 {
			t.Fatalf("reply %q, calls %v, want %q", h.lastReply(), h.k.Fake.Calls(), c)
		}
	})
	// A delete given up on (too old to send) is never tried again, even by a
	// later ban after an /unban.
	t.Run("refused-not-retried-after-unban", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("OLD", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.k.Clock.Advance(47*time.Hour + time.Minute)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); !strings.Contains(got, "Their posts on record: 0 being deleted, 0 too old to delete.") {
			t.Fatalf("reply %q", got)
		}
		h.k.Fire()
		if rows := rowsOf(h, client.Member{Phone: modtest.SpammerPhone}); rows["revoke/failed"] != 1 || h.revoked(modtest.G1, "OLD") {
			t.Fatalf("rows %v, calls %v, want the one failed delete and no send", rows, h.k.Fake.Calls())
		}
	})
	// Several posts in one group are each deleted: one ban writes a delete
	// row per post, never one per group.
	t.Run("two-posts-one-group", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.post("P2", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); !strings.Contains(got, "Their posts on record: 2 being deleted, 0 too old to delete.") {
			t.Fatalf("reply %q", got)
		}
		if queued, err := h.k.Store.OutboxLen(h.k.Ctx); err != nil || queued != 2 {
			t.Fatalf("%d queued (%v), want the 2 deletes in the outbox", queued, err)
		}
		h.k.Fire()
		if !h.revoked(modtest.G1, "P1") || !h.revoked(modtest.G1, "P2") {
			t.Fatalf("calls %v, want P1 and P2 deleted", h.k.Fake.Calls())
		}
	})
	// An older post too old to delete does not stop a newer one in the same
	// group.
	t.Run("old-and-new-one-group", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("OLD", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.k.Clock.Advance(47*time.Hour + time.Minute)
		h.post("NEW", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); !strings.Contains(got, "Their posts on record: 1 being deleted, 1 too old to delete") {
			t.Fatalf("reply %q", got)
		}
		h.k.Fire()
		if h.revoked(modtest.G1, "OLD") || !h.revoked(modtest.G1, "NEW") {
			t.Fatalf("calls %v, want NEW deleted and OLD never sent", h.k.Fake.Calls())
		}
	})
	// [Ban] on a report deletes the reported post and the member's other
	// posts in the same group.
	t.Run("ban-button-other-post-same-group", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P2", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		r := watchOnly(t, h)
		h.press(adminUser, "ban", r.ID)
		h.k.Fire()
		if !h.revoked(modtest.G1, "W1") || !h.revoked(modtest.G1, "P2") {
			t.Fatalf("calls %v, want the reported W1 and the other post P2 deleted", h.k.Fake.Calls())
		}
	})
	// A delete dropped because an /unban got there before it was sent is
	// planned again by the next ban.
	t.Run("unbanned-before-send-planned-again", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
		h.k.Fire()
		if h.revoked(modtest.G1, "P1") {
			t.Fatalf("calls %v, want no delete sent while unbanned", h.k.Fake.Calls())
		}
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); !strings.Contains(got, "Their posts on record: 1 being deleted, 0 too old to delete.") {
			t.Fatalf("reply %q", got)
		}
		h.k.Fire()
		if !h.revoked(modtest.G1, "P1") {
			t.Fatalf("calls %v, want P1 deleted by the second ban", h.k.Fake.Calls())
		}
	})
	// A delete WhatsApp refused is never tried again by a later ban: one it
	// answered "not an admin" to, and one that failed every try.
	refusedThenRebanned := func(t *testing.T, fail func(h *harness)) {
		t.Helper()
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		fail(h)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		for i := 0; i < 10; i++ {
			h.k.Fire()
			h.k.Clock.Advance(30 * time.Minute)
		}
		sent := h.k.Fake.Count("Revoke")
		if rows := rowsOf(h, client.Member{Phone: modtest.SpammerPhone}); rows["revoke/failed"] != 1 || sent == 0 {
			t.Fatalf("rows %v, %d sends, want the delete sent and failed", rows, sent)
		}
		h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); !strings.Contains(got, "Their posts on record: 0 being deleted, 0 too old to delete.") {
			t.Fatalf("reply %q", got)
		}
		h.k.Fire()
		if h.k.Fake.Count("Revoke") != sent {
			t.Fatalf("calls %v, want no new send after the refusal", h.k.Fake.Calls())
		}
	}
	t.Run("not-admin-refusal-not-retried", func(t *testing.T) {
		refusedThenRebanned(t, func(h *harness) { h.k.Fake.FailNext("Revoke", client.ErrNotAdmin) })
	})
	t.Run("send-failures-not-retried", func(t *testing.T) {
		refusedThenRebanned(t, func(h *harness) {
			errs := make([]error, 8)
			for i := range errs {
				errs[i] = errors.New("server error")
			}
			h.k.Fake.FailNext("Revoke", errs...)
		})
	})
	// A delete that got one error (or a rate limit) and was then dropped by
	// an /unban before its retry was never refused by WhatsApp: the next ban
	// plans it again.
	for name, err := range map[string]error{"error": errors.New("server error"), "rate-limit": client.ErrRateLimited} {
		t.Run("one-"+name+"-then-unban-planned-again", func(t *testing.T) {
			h := newHarness(t, "")
			h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
			h.k.Fake.FailNext("Revoke", err)
			h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
			h.k.Fire()
			h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
			h.k.Clock.Advance(5 * time.Minute)
			h.k.Fire()
			h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
			if got := h.lastReply(); !strings.Contains(got, "Their posts on record: 1 being deleted, 0 too old to delete.") {
				t.Fatalf("reply %q, rows %v", got, rowsOf(h, client.Member{Phone: modtest.SpammerPhone}))
			}
			h.k.Fire()
			// The first try failed; the second ban's delete is sent and done.
			rows := rowsOf(h, client.Member{Phone: modtest.SpammerPhone})
			if h.k.Fake.Count("Revoke") != 2 || rows["revoke/requested"] != 1 {
				t.Fatalf("calls %v, rows %v, want P1 deleted by the second ban", h.k.Fake.Calls(), rows)
			}
		})
	}
	// /ban <number> and then [Ban] on a report of the same post delete it
	// once, though one names its author by phone number and the other by LID.
	t.Run("number-then-button-deletes-once", func(t *testing.T) {
		h := newHarness(t, "")
		r := watchOnly(t, h)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.press(adminUser, "ban", r.ID)
		h.k.Fire()
		dels := 0
		for _, row := range h.k.Rows(modtest.SpammerM) {
			if row.Action == store.ActRevoke && row.MsgID == "W1" && row.Mode == store.ModeEnforce {
				dels++
			}
		}
		if dels != 1 || !h.revoked(modtest.G1, "W1") {
			t.Fatalf("%d enforced delete rows for W1, calls %v, want one", dels, h.k.Fake.Calls())
		}
	})
	// A post the banned person sends after the ban, before the sweep removes
	// them, is deleted when it arrives; once they are unbanned it is not.
	t.Run("post-after-ban-deleted", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.post("P2", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.k.Fire()
		if !h.revoked(modtest.G1, "P1") || !h.revoked(modtest.G1, "P2") {
			t.Fatalf("calls %v, want P1 and P2 deleted", h.k.Fake.Calls())
		}
		if n := h.k.Logged("post by a banned member; delete planned"); n != 1 {
			t.Fatalf("%d delete-planned log lines, want 1 (for P2)", n)
		}
		h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
		h.post("P3", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.k.Fire()
		for _, r := range h.k.Rows(modtest.SpammerM) {
			if r.Action == store.ActRevoke && r.MsgID == "P3" {
				t.Fatalf("row %+v, want no delete planned for a post after the unban", r)
			}
		}
		if h.revoked(modtest.G1, "P3") {
			t.Fatalf("calls %v, want P3 kept", h.k.Fake.Calls())
		}
	})
	// In a community in shadow mode a ban deletes nothing: the reply counts the
	// person's posts there as left, says the sweep only reports them there, and
	// a post they send there after the ban is kept too.
	t.Run("shadow-community-not-deleted", func(t *testing.T) {
		cfg := strings.Replace(modtest.Config, "    name: set b\n", "    name: set b\n    mode: shadow\n", 1)
		if cfg == modtest.Config {
			t.Fatal("the kit config has no community named set b")
		}
		h := newHarnessKit(t, modtest.NewConfig(t, cfg))
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.post("P3", modtest.GB, modtest.Spammer, modtest.SpammerPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		got := h.lastReply()
		if !strings.Contains(got, "1 being deleted, 0 too old to delete, 1 left in a community in shadow mode") ||
			!strings.Contains(got, "except in a community in shadow mode, where the sweep only reports them") {
			t.Fatalf("reply %q", got)
		}
		h.post("P4", modtest.GB, modtest.Spammer, modtest.SpammerPhone)
		h.k.Fire()
		if !h.revoked(modtest.G1, "P1") || h.revoked(modtest.GB, "P3") || h.revoked(modtest.GB, "P4") {
			t.Fatalf("calls %v, want only P1 deleted", h.k.Fake.Calls())
		}
		// The ban's own rows for P3 are shadow rows; the later post P4 gets
		// none (the executor would turn an enforced one into shadow before
		// sending, so its mode alone would not show it was planned).
		for _, r := range h.k.Rows(modtest.SpammerM) {
			if r.Action == store.ActRevoke && (r.MsgID == "P4" || r.MsgID == "P3" && r.Mode == store.ModeEnforce) {
				t.Fatalf("row %+v, want no delete planned for P4 and only shadow rows for P3", r)
			}
		}
	})
	// A banned member's later post that a log rule or a watch-only rule
	// matches is reported as before and deleted too.
	// Both for a ban still keyed by the phone number and for one already
	// resolved to the LID (whose delete names the same member as the
	// watch-only match's shadow row).
	for _, how := range []string{"by-number", "resolved"} {
		t.Run("post-after-ban-rule-match-deleted/"+how, func(t *testing.T) {
			cfg := strings.Replace(modtest.Config, "communities:\n",
				"    - name: note\n      action: log\n      when:\n        words: crypto\ncommunities:\n", 1)
			if cfg == modtest.Config {
				t.Fatal("the kit config has no communities key")
			}
			h := newHarnessKit(t, modtest.NewConfig(t, cfg))
			if how == "resolved" {
				h.k.Ban(modtest.SpammerM)
			} else {
				h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
			}
			for id, text := range map[string]string{"L1": "crypto news today", "W1": "crypto: inbox me"} {
				m := h.k.Msg(id, modtest.G1, modtest.Spammer, text)
				m.SenderAlt = modtest.SpammerPhone
				h.k.Deliver(m)
			}
			h.k.Fire()
			if !h.revoked(modtest.G1, "L1") || !h.revoked(modtest.G1, "W1") {
				t.Fatalf("calls %v, want the log-rule match L1 and the watch-only match W1 deleted", h.k.Fake.Calls())
			}
			shadow := false
			for _, r := range h.k.Rows(modtest.SpammerM) {
				shadow = shadow || r.Action == store.ActRevoke && r.MsgID == "W1" && r.Mode == store.ModeShadow
			}
			if !shadow {
				t.Fatalf("rows %v, want the watch-only match's own shadow row kept", rowsOf(h, modtest.SpammerM))
			}
		})
	}
	// Under a pause that holds removals and bans (the breaker's: a rule may be
	// misfiring), a spam post's ban is held, not applied; the member's next
	// post is kept, and applying the ban later does not delete it.
	t.Run("post-after-held-ban-kept", func(t *testing.T) {
		h := newHarness(t, "")
		if err := h.k.Store.SetPause(h.k.Ctx, store.Pause{Source: store.SourceBreaker, Scope: store.ScopeRemoveBan,
			Reason: "too many removals", Since: h.k.Clock.Now()}); err != nil {
			t.Fatal(err)
		}
		h.k.Deliver(h.k.Spam("S1", modtest.G1))
		h.post("P2", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.k.Fire()
		if h.k.Banned(modtest.SpammerM, "") || !h.revoked(modtest.G1, "S1") || h.revoked(modtest.G1, "P2") {
			t.Fatalf("calls %v, want the ban held, S1 deleted by its rule and P2 kept", h.k.Fake.Calls())
		}
		for _, r := range h.k.Rows(modtest.SpammerM) {
			if r.Action == store.ActRevoke && r.MsgID == "P2" {
				t.Fatalf("row %+v, want no delete planned for P2", r)
			}
		}
	})
	// An edit deletes the post it edits, named by the author as the group
	// knows them, and once only: an edit of a post already being deleted
	// adds nothing.
	t.Run("post-after-ban-edit-deletes-original-once", func(t *testing.T) {
		h := newHarness(t, "")
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.post("P0", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		e := h.k.Msg("E1", modtest.G1, modtest.Spammer, "hello again, see you at the meetup")
		e.SenderAlt, e.IsEdit, e.TargetID = modtest.SpammerPhone, true, "P0"
		h.k.Deliver(e)
		h.k.Fire()
		want := "Revoke " + string(modtest.G1) + " " + string(modtest.Spammer) + " P0"
		if h.k.Fake.Count(want) != 1 || h.k.Fake.Count("Revoke") != 1 {
			t.Fatalf("calls %v, want only %q", h.k.Fake.Calls(), want)
		}
	})
	// A post older than act_on_replay_max_age (delivered late, after a
	// reconnect) is past an admin's delete window: nothing is planned for it.
	t.Run("old-replay-after-ban-kept", func(t *testing.T) {
		h := newHarness(t, "")
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		o := h.k.Msg("O1", modtest.G1, modtest.Spammer, "hello everyone, see you at the meetup")
		o.SenderAlt, o.Time = modtest.SpammerPhone, h.k.Clock.Now().Add(-60*time.Hour)
		h.k.Deliver(o)
		h.k.Fire()
		for _, r := range h.k.Rows(modtest.SpammerM) {
			if r.Action == store.ActRevoke && r.MsgID == "O1" {
				t.Fatalf("row %+v, want no delete planned for a post past act_on_replay_max_age", r)
			}
		}
	})
	// The paused reply says the same about a community in shadow mode.
	t.Run("shadow-community-paused-reply", func(t *testing.T) {
		cfg := strings.Replace(modtest.Config, "    name: set b\n", "    name: set b\n    mode: shadow\n", 1)
		h := newHarnessKit(t, modtest.NewConfig(t, cfg))
		if err := h.k.Store.SetPause(h.k.Ctx, store.Pause{Source: store.SourceBreaker, Scope: store.ScopeRemoveBan,
			Reason: "too many removals", Since: h.k.Clock.Now()}); err != nil {
			t.Fatal(err)
		}
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); !strings.Contains(got, "removing them waits for the pause to end, except in a community "+
			"in shadow mode, where the sweep only reports them. To undo") {
			t.Fatalf("reply %q", got)
		}
	})
	// A post by a banned member who is a current admin is left alone, as
	// every action against an admin is.
	t.Run("post-by-banned-admin-kept", func(t *testing.T) {
		h := newHarness(t, "")
		h.k.Ban(modtest.AdminM)
		h.post("A1", modtest.G1, modtest.Admin, modtest.AdminPhone)
		h.k.Fire()
		for _, r := range h.k.Rows(modtest.AdminM) {
			if r.Action == store.ActRevoke {
				t.Fatalf("row %+v, want no delete planned for an admin's post", r)
			}
		}
		if h.revoked(modtest.G1, "A1") || h.k.Logged("post by a banned member who is a current admin; not deleted") != 1 {
			t.Fatalf("calls %v, want A1 kept and logged", h.k.Fake.Calls())
		}
	})
	// A spammer's ban, lifted since, paired their old number, which now
	// belongs to an admin. A ban no longer in force pairs nothing, so their
	// spam under their LID alone (they joined since the group list was last
	// read) is deleted and they are removed, not spared as "a current admin".
	t.Run("old-number-now-an-admin", func(t *testing.T) {
		h := newHarness(t, "")
		recentJoiner(h)
		then := client.Member{LID: modtest.Spammer, Phone: modtest.AdminPhone} // the number they held then
		h.k.Ban(then)
		h.k.Unban(then)
		s := h.k.Spam("S1", modtest.G1)
		s.SenderAlt = ""
		h.k.Deliver(s)
		h.k.Fire()
		spared := h.k.Reports(ledger.KindAdminSpared)
		if !h.revoked(modtest.G1, "S1") || h.k.Fake.Count("Remove "+string(modtest.G1)+" "+string(modtest.Spammer)) != 1 ||
			len(spared) != 0 {
			t.Fatalf("calls %v, rows %v, admin_spared reports %+v; want S1 deleted, them removed and nobody spared",
				h.k.Fake.Calls(), rowsOf(h, modtest.SpammerM), spared)
		}
	})
}

// TestBanWaitsForGroupList: a ban sent before the bot has read its group
// list is refused (it would find no group to delete the member's posts in,
// and once they are banned no later ban would plan those deletes); the press
// is released, so the same ban works once the list is in.
func TestBanWaitsForGroupList(t *testing.T) {
	const notLoaded = "Could not do that: the bot has not loaded its group list yet"
	t.Run("by-number", func(t *testing.T) {
		h := newHarness(t, "")
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		loaded := h.app.Admin.Directory
		h.app.Admin.Directory = &pipeline.Directory{} // the group list not read yet
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); !strings.HasPrefix(got, notLoaded) || len(h.bans()) != 0 {
			t.Fatalf("reply %q, bans %+v, want the refusal and no ban", got, h.bans())
		}
		h.app.Admin.Directory = loaded
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.k.Fire()
		if len(h.bans()) != 1 || !h.revoked(modtest.G1, "P1") {
			t.Fatalf("reply %q, calls %v, want the ban and P1 deleted", h.lastReply(), h.k.Fake.Calls())
		}
	})
	t.Run("button", func(t *testing.T) {
		h := newHarness(t, "")
		r := watchOnly(t, h)
		loaded := h.app.Admin.Directory
		h.app.Admin.Directory = &pipeline.Directory{}
		h.press(adminUser, "ban", r.ID)
		if a := h.answers(); len(a) != 1 || !strings.HasPrefix(a[0], notLoaded) || h.k.Banned(modtest.SpammerM, "") {
			t.Fatalf("answers %q, want the refusal and no ban", a)
		}
		h.app.Admin.Directory = loaded
		h.press(adminUser, "ban", r.ID)
		h.k.Fire()
		if !h.k.Banned(modtest.SpammerM, "") || !h.revoked(modtest.G1, "W1") {
			t.Fatalf("answers %q, calls %v, want the second press to ban and delete", h.answers(), h.k.Fake.Calls())
		}
	})
}

// recentJoiner makes the kit spammer someone who joined G1 since the group
// list was last read: the directory knows them by their LID only (a join
// event names no phone number).
func recentJoiner(h *harness) {
	h.t.Helper()
	groups := modtest.Groups()
	for i := range groups {
		var ps []client.Participant
		for _, p := range groups[i].Participants {
			if p.LID != modtest.Spammer {
				ps = append(ps, p)
			}
		}
		groups[i].Participants = ps
	}
	modtest.Load(h.t, h.k.Dir, groups)
	h.k.Deliver(&client.GroupChange{Group: modtest.G1, Time: h.k.Clock.Now(), JoinReason: "invite",
		Joined: []client.JID{modtest.Spammer}})
}

// TestBanRecentJoiner: /ban <number> for someone who joined since the group
// list was last read (the directory knows them by LID only; with no LID on
// record or from WhatsApp the ban is keyed by the phone number until the
// sweep) deletes the post they send after the ban, and the sweep the ban
// asks for removes them at once.
func TestBanRecentJoiner(t *testing.T) {
	t.Run("post-after-ban-deleted", func(t *testing.T) {
		h := newHarness(t, "")
		recentJoiner(h)
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.post("P2", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.k.Fire()
		if !h.revoked(modtest.G1, "P1") || !h.revoked(modtest.G1, "P2") {
			t.Fatalf("calls %v, rows %v, want P1 and P2 deleted", h.k.Fake.Calls(), rowsOf(h, modtest.SpammerM))
		}
	})
	t.Run("sweep-removes-at-once", func(t *testing.T) {
		h := newHarness(t, "")
		h.k.Fake.LIDs = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
		recentJoiner(h)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if _, err := h.app.Sweep.Run(h.k.Ctx, "r1"); err != nil {
			t.Fatal(err)
		}
		h.k.Fire()
		if want := "Remove " + string(modtest.G1) + " " + string(modtest.Spammer); h.k.Fake.Count(want) != 1 {
			t.Fatalf("calls %v, want %q from the first sweep", h.k.Fake.Calls(), want)
		}
	})
	// A rate-limited LID lookup does not hold up the groups: the sweep checks
	// them first, then looks up again; the next sweep removes them. (WhatsApp
	// has no LID for the number at /ban, so the ban waits for the sweep's.)
	t.Run("sweep-checks-groups-when-lookup-rate-limited", func(t *testing.T) {
		h := newHarness(t, "")
		recentJoiner(h)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.k.Fake.LIDs = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
		h.k.Fake.FailNext("ResolvePhoneToLID", fmt.Errorf("%w: status 429", client.ErrRateLimited))
		before := len(h.k.Fake.Calls()) // /ban's own lookup is not the sweep's
		res, err := h.app.Sweep.Run(h.k.Ctx, "r1")
		if err != nil {
			t.Fatal(err)
		}
		var lookups, checks []int
		for i, c := range h.k.Fake.Calls()[before:] {
			switch {
			case strings.HasPrefix(c, "ResolvePhoneToLID "):
				lookups = append(lookups, i)
			case strings.HasPrefix(c, "JoinRequests "):
				checks = append(checks, i)
			}
		}
		if len(lookups) != 2 || len(checks) == 0 || lookups[0] > checks[0] || lookups[1] < checks[len(checks)-1] {
			t.Fatalf("calls %v, want a lookup, the group checks, then the lookup again", h.k.Fake.Calls()[before:])
		}
		if slept := h.k.Clock.Slept(); len(slept) != 0 || res.RateLimited != 1 || res.Errors != 0 {
			t.Fatalf("waits %v, result %+v; want no wait (the groups came between), one rate limit, no error", slept, res)
		}
		if _, banned, err := h.k.Store.FindBan(h.k.Ctx, []string{string(modtest.Spammer)}, ""); err != nil || !banned {
			t.Fatalf("the ban has no LID after the sweep: %v (%v)", banned, err)
		}
		if _, err := h.app.Sweep.Run(h.k.Ctx, "r2"); err != nil {
			t.Fatal(err)
		}
		h.k.Fire()
		if want := "Remove " + string(modtest.G1) + " " + string(modtest.Spammer); h.k.Fake.Count(want) != 1 {
			t.Fatalf("calls %v, want %q from the next sweep", h.k.Fake.Calls(), want)
		}
	})
	// /unban <number> lifts a ban a pause holds for them: the held ban names
	// them by LID and keeps the phone, which is all the number gives.
	t.Run("unban-drops-held-ban", func(t *testing.T) {
		h := newHarness(t, "")
		recentJoiner(h)
		if err := h.k.Store.SetPause(h.k.Ctx, store.Pause{Source: store.SourceBreaker, Scope: store.ScopeRemoveBan,
			Reason: "too many removals", Since: h.k.Clock.Now()}); err != nil {
			t.Fatal(err)
		}
		h.k.Deliver(h.k.Spam("S1", modtest.G1))
		h.k.Fire()
		if held, err := h.k.Store.HeldBan(h.k.Ctx, []string{string(modtest.Spammer)}, ""); err != nil || !held {
			t.Fatalf("held %v (%v), want a ban the pause holds", held, err)
		}
		h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
		if reply := h.lastReply(); !strings.Contains(reply, "is unbanned everywhere (1 ban(s) lifted)") {
			t.Fatalf("reply %q, want the held ban lifted and counted", reply)
		}
		if held, err := h.k.Store.HeldBan(h.k.Ctx, []string{string(modtest.Spammer)}, ""); err != nil || held {
			t.Fatalf("held %v (%v) after /unban, want none", held, err)
		}
		if err := h.k.Store.ClearPause(h.k.Ctx, store.SourceBreaker); err != nil {
			t.Fatal(err)
		}
		h.k.Fire()
		if h.k.Banned(modtest.SpammerM, "") || h.k.Fake.Count("Remove "+string(modtest.G1)+" "+string(modtest.Spammer)) != 0 {
			t.Fatalf("calls %v: the ban applied after the pause ended, though they were unbanned", h.k.Fake.Calls())
		}
	})
	// /ban <number> then /unban <number> in the same pause: the ban by number
	// and the held ban by LID are one ban, lifted and counted once.
	t.Run("ban-then-unban-in-pause-counts-once", func(t *testing.T) {
		h := newHarness(t, "")
		recentJoiner(h)
		if err := h.k.Store.SetPause(h.k.Ctx, store.Pause{Source: store.SourceAdmin, Scope: store.ScopeAll, Reason: "paused",
			Since: h.k.Clock.Now()}); err != nil {
			t.Fatal(err)
		}
		h.k.Deliver(h.k.Spam("S1", modtest.G1))
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
		if reply := h.lastReply(); !strings.Contains(reply, "is unbanned everywhere (1 ban(s) lifted)") {
			t.Fatalf("reply %q, want one ban lifted", reply)
		}
		if held, err := h.k.Store.HeldBan(h.k.Ctx, []string{string(modtest.Spammer)}, ""); err != nil || held {
			t.Fatalf("held %v (%v) after /unban, want none", held, err)
		}
		if err := h.k.Store.ClearPause(h.k.Ctx, store.SourceAdmin); err != nil {
			t.Fatal(err)
		}
		h.k.Fire()
		if h.k.Banned(modtest.SpammerM, "") || h.k.Fake.Count("Remove "+string(modtest.G1)+" "+string(modtest.Spammer)) != 0 {
			t.Fatalf("calls %v: banned or removed after /unban", h.k.Fake.Calls())
		}
	})
	// [Add to ban list] for them writes the ban with their phone number, which
	// the record of their post gives (and, in a pause, the ban it holds), so
	// /unban <number> finds it and cancels the queued delete of their post.
	for _, tc := range []struct {
		name   string
		source string
		scope  store.Scope
	}{
		{"no-pause", "", ""},
		{"breaker-pause", store.SourceBreaker, store.ScopeRemoveBan},
	} {
		t.Run("add-to-ban-list-then-unban-number/"+tc.name, func(t *testing.T) {
			h := newHarness(t, "")
			recentJoiner(h)
			if tc.source != "" {
				if err := h.k.Store.SetPause(h.k.Ctx, store.Pause{Source: tc.source, Scope: tc.scope, Reason: "paused",
					Since: h.k.Clock.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
			h.k.Deliver(&client.GroupChange{Group: modtest.G1, Time: h.k.Clock.Now(), Actor: modtest.Admin,
				Left: []client.JID{modtest.Spammer}})
			h.drain()
			h.press(adminUser, "addban", h.only(ledger.KindHumanRemoval).ID)
			if bans := h.bans(); len(bans) != 1 || bans[0].LID != string(modtest.Spammer) ||
				bans[0].Phone != string(modtest.SpammerPhone) {
				t.Fatalf("bans %+v, want one entry with their LID and phone number", bans)
			}
			h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
			if reply := h.lastReply(); !strings.Contains(reply, "is unbanned everywhere (1 ban(s) lifted)") {
				t.Fatalf("reply %q, want the ban found by number and lifted", reply)
			}
			if tc.source != "" {
				if err := h.k.Store.ClearPause(h.k.Ctx, tc.source); err != nil {
					t.Fatal(err)
				}
			}
			h.k.Fire()
			if len(h.bans()) != 0 || h.k.Banned(modtest.SpammerM, "") || h.revoked(modtest.G1, "P1") {
				t.Fatalf("bans %+v, calls %v: still banned, or P1 deleted after /unban", h.bans(), h.k.Fake.Calls())
			}
		})
	}
	// [Undo] after a ban of their number with the ban command, before the
	// sweep re-keys it on their LID, lifts the number's ban too: the record
	// of their post gives the number, so the next sweep removes nobody.
	t.Run("undo-before-sweep-lifts-number-ban", func(t *testing.T) {
		h := newHarness(t, "")
		h.k.Fake.LIDs = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
		recentJoiner(h)
		if err := h.k.Store.SetPause(h.k.Ctx, store.Pause{Source: store.SourceBreaker, Scope: store.ScopeRemoveBan,
			Reason: "too many removals", Since: h.k.Clock.Now()}); err != nil {
			t.Fatal(err)
		}
		r := h.spam("S1")
		h.drain()
		h.cliBan()
		h.press(secondAdm, "undo", r.ID)
		if bans := h.bans(); len(bans) != 0 {
			t.Fatalf("bans %+v after [Undo], want none (the number's ban lifted too)", bans)
		}
		// The reply counts the number's ban, found by the address the report
		// does not name.
		if a := h.answers(); len(a) == 0 || !strings.Contains(a[len(a)-1], "(1 ban(s) lifted") {
			t.Fatalf("answers %q, want the number's ban counted as lifted", a)
		}
		if err := h.k.Store.ClearPause(h.k.Ctx, store.SourceBreaker); err != nil {
			t.Fatal(err)
		}
		if _, err := h.app.Sweep.Run(h.k.Ctx, "r1"); err != nil {
			t.Fatal(err)
		}
		h.k.Fire()
		if h.k.Banned(modtest.SpammerM, "") || h.k.Fake.Count("Remove "+string(modtest.G1)+" "+string(modtest.Spammer)) != 0 {
			t.Fatalf("calls %v: banned or removed after [Undo]", h.k.Fake.Calls())
		}
	})
	// /ban <number> deletes a post they sent without their number (the post
	// names their LID alone, which the directory does not know): the LID
	// WhatsApp gives for the number finds it.
	t.Run("ban-number-deletes-posts-sent-without-their-number", func(t *testing.T) {
		h := newHarness(t, "")
		h.k.Fake.LIDs = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
		recentJoiner(h)
		h.post("P1", modtest.G1, modtest.Spammer, "")
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if reply := h.lastReply(); !strings.Contains(reply, "Their posts on record: 1 being deleted") {
			t.Fatalf("reply %q, want P1 counted", reply)
		}
		h.k.Fire()
		if !h.revoked(modtest.G1, "P1") {
			t.Fatalf("calls %v, want P1 deleted", h.k.Fake.Calls())
		}
	})
	// [Undo] after a /ban <number> whose ban has their LID (WhatsApp gives it
	// at /ban here; otherwise the sweep finds it), for someone whose posts
	// never carried their number: only the ban list pairs their two
	// addresses, and through it [Undo] also overturns the number's own ban
	// row (keyed by the number), so the reply counts it.
	t.Run("undo-after-sweep-overturns-number-ban", func(t *testing.T) {
		h := newHarness(t, "")
		h.k.Fake.LIDs = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
		recentJoiner(h)
		if err := h.k.Store.SetPause(h.k.Ctx, store.Pause{Source: store.SourceAdmin, Scope: store.ScopeAll,
			Reason: "paused", Since: h.k.Clock.Now()}); err != nil {
			t.Fatal(err)
		}
		m := h.k.Spam("S1", modtest.G1)
		m.SenderAlt = ""
		h.k.Deliver(m)
		h.post("P1", modtest.G2, modtest.Spammer, "")
		h.drain()
		r := h.only(ledger.KindAction)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if _, err := h.app.Sweep.Run(h.k.Ctx, "r1"); err != nil {
			t.Fatal(err)
		}
		h.press(secondAdm, "undo", r.ID)
		if got := rowsOf(h, client.Member{Phone: modtest.SpammerPhone}); got["ban/overturned"] != 1 {
			t.Fatalf("number's rows %v, answers %q: want its ban row overturned", got, h.answers())
		}
		if a := h.answers(); len(a) == 0 || !strings.Contains(a[len(a)-1], "3 action(s) overturned") {
			t.Fatalf("answers %q, want the number's ban row counted with the bot's two", a)
		}
		if err := h.k.Store.ClearPause(h.k.Ctx, store.SourceAdmin); err != nil {
			t.Fatal(err)
		}
		h.k.Fire()
		if h.revoked(modtest.G2, "P1") || h.k.Banned(modtest.SpammerM, "") {
			t.Fatalf("calls %v: P1 deleted or still banned after [Undo]", h.k.Fake.Calls())
		}
	})
	// A ban the bot made by their LID alone (their post came without their
	// number) is found for /ban <number> through the LID WhatsApp gives:
	// already banned, so no second ban.
	t.Run("ban-number-already-banned-by-lid", func(t *testing.T) {
		h := newHarness(t, "")
		h.k.Fake.LIDs = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
		recentJoiner(h)
		m := h.k.Spam("S1", modtest.G1)
		m.SenderAlt = ""
		h.k.Deliver(m)
		h.k.Fire()
		if bans := h.bans(); len(bans) != 1 || bans[0].Phone != "" {
			t.Fatalf("bans %+v, want the bot's one ban by LID alone", bans)
		}
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if reply := h.lastReply(); !strings.Contains(reply, "already banned everywhere") || len(h.bans()) != 1 {
			t.Fatalf("reply %q, bans %+v, want the already-banned reply and no second ban", reply, h.bans())
		}
	})
	// A post they send without their number, after a ban of their number
	// with the ban command (keyed by the number until the sweep), is deleted
	// on arrival: an earlier post of theirs paired the two addresses.
	t.Run("post-without-number-after-ban-deleted", func(t *testing.T) {
		h := newHarness(t, "")
		recentJoiner(h)
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.cliBan()
		h.post("P2", modtest.G1, modtest.Spammer, "")
		h.k.Fire()
		if !h.revoked(modtest.G1, "P2") {
			t.Fatalf("calls %v, rows %v, want P2 deleted", h.k.Fake.Calls(), rowsOf(h, modtest.SpammerM))
		}
	})
	// [Ban] on a report about a post sent without their number bans them
	// under both addresses: an earlier post of theirs paired the two.
	t.Run("ban-button-carries-their-number", func(t *testing.T) {
		h := newHarness(t, "")
		recentJoiner(h)
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.k.Deliver(h.k.Msg("W1", modtest.G1, modtest.Spammer, "crypto: inbox me"))
		h.k.Fire()
		h.drain()
		h.press(adminUser, "ban", h.only(ledger.KindWouldHaveActed).ID)
		if bans := h.bans(); len(bans) != 1 || bans[0].LID != string(modtest.Spammer) ||
			bans[0].Phone != string(modtest.SpammerPhone) {
			t.Fatalf("bans %+v, want one ban naming their LID and their number", bans)
		}
	})
	// Banned by number with the ban command (which asks for no sweep), they
	// join a group again: the join names their LID alone, and the record of
	// their earlier post gives the number, so they are removed at the join.
	t.Run("rejoin-after-cli-ban-removed", func(t *testing.T) {
		h := newHarness(t, "")
		recentJoiner(h)
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.cliBan()
		h.k.Deliver(&client.GroupChange{Group: modtest.G2, Time: h.k.Clock.Now(), JoinReason: "invite",
			Joined: []client.JID{modtest.Spammer}})
		h.k.Fire()
		if h.k.Fake.Count("Remove "+string(modtest.G2)+" "+string(modtest.Spammer)) != 1 {
			t.Fatalf("calls %v, rows %v, want them removed from G2 at the join", h.k.Fake.Calls(),
				rowsOf(h, modtest.SpammerM))
		}
	})
	// Banned by number with the ban command and not resolved yet, they post,
	// join and ask to join under their LID alone, with no record of a post
	// pairing the two: the group list's pair (Identify's one Complete) gives
	// the number, so the post is deleted, the join removed and the request
	// rejected. The request is checked as the sweep checks each group, without
	// the sweep's own resolve of the ban first.
	for name, act := range map[string]func(h *harness) (string, bool){
		"post": func(h *harness) (string, bool) {
			h.post("P1", modtest.G1, modtest.Spammer, "")
			h.k.Fire()
			return "P1 deleted", h.revoked(modtest.G1, "P1")
		},
		"join": func(h *harness) (string, bool) {
			h.k.Deliver(&client.GroupChange{Group: modtest.G2, Time: h.k.Clock.Now(), JoinReason: "invite",
				Joined: []client.JID{modtest.Spammer}})
			h.k.Fire()
			return "removed from G2", h.k.Fake.Count("Remove "+string(modtest.G2)+" "+string(modtest.Spammer)) == 1
		},
		"join-request": func(h *harness) (string, bool) {
			h.k.Fake.Requests = map[client.JID][]client.JoinRequest{modtest.G2: {{JID: modtest.Spammer}}}
			if err := h.app.Sweep.Enforcer.CheckJoinRequests(h.k.Ctx, modtest.G2, "r1"); err != nil {
				h.t.Fatal(err)
			}
			h.k.Fire()
			return "request to G2 rejected",
				h.k.Fake.Count("RejectJoinRequests "+string(modtest.G2)+" "+string(modtest.Spammer)) == 1
		},
	} {
		t.Run("group-list-pair/"+name, func(t *testing.T) {
			h := newHarness(t, "")
			h.cliBan()
			if b := h.bans(); len(b) != 1 || b[0].LID != "" {
				t.Fatalf("bans %+v, want one keyed by the number alone", b)
			}
			if what, ok := act(h); !ok {
				t.Fatalf("want %s; calls %v, rows %v", what, h.k.Fake.Calls(), rowsOf(h, modtest.SpammerM))
			}
		})
	}
	// Their request to join a group, after a ban by number with the ban
	// command, names their LID alone: the record of their earlier post gives
	// the number, so the sweep rejects it.
	t.Run("join-request-after-cli-ban-rejected", func(t *testing.T) {
		h := newHarness(t, "")
		recentJoiner(h)
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.cliBan()
		h.k.Fake.Requests = map[client.JID][]client.JoinRequest{modtest.G2: {{JID: modtest.Spammer}}}
		if _, err := h.app.Sweep.Run(h.k.Ctx, "r1"); err != nil {
			t.Fatal(err)
		}
		h.k.Fire()
		if h.k.Fake.Count("RejectJoinRequests "+string(modtest.G2)+" "+string(modtest.Spammer)) != 1 {
			t.Fatalf("calls %v, want their join request to G2 rejected", h.k.Fake.Calls())
		}
	})
	// A human admin removes them after a ban by number with the ban command:
	// the removal names their LID alone, and the record of their earlier
	// post gives the number, so the bot does not offer to ban them again.
	t.Run("human-removal-after-cli-ban-not-offered", func(t *testing.T) {
		h := newHarness(t, "")
		recentJoiner(h)
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.cliBan()
		h.k.Deliver(&client.GroupChange{Group: modtest.G1, Time: h.k.Clock.Now(), Actor: modtest.Admin,
			Left: []client.JID{modtest.Spammer}})
		h.drain()
		if reps := h.k.Reports(ledger.KindHumanRemoval); len(reps) != 0 {
			t.Fatalf("reports %+v, want no [Add to ban list] offer for someone already banned", reps)
		}
	})
	// Banned by number with the ban command while already in a group, under
	// their LID alone: the sweep's check of the group's members finds the ban
	// through the record of their earlier post, when the sweep's LID lookup
	// finds nothing or is rate-limited, so the first sweep removes them.
	for name, fail := range map[string]error{
		"no-lid":       nil,
		"rate-limited": fmt.Errorf("%w: status 429", client.ErrRateLimited),
	} {
		t.Run("sweep-removes-present-through-record/"+name, func(t *testing.T) {
			h := newHarness(t, "")
			recentJoiner(h)
			h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
			h.cliBan()
			if fail != nil {
				h.k.Fake.LIDs = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
				h.k.Fake.FailNext("ResolvePhoneToLID", fail)
			}
			if _, err := h.app.Sweep.Run(h.k.Ctx, "r1"); err != nil {
				t.Fatal(err)
			}
			h.k.Fire()
			if want := "Remove " + string(modtest.G1) + " " + string(modtest.Spammer); h.k.Fake.Count(want) != 1 {
				t.Fatalf("calls %v, want %q from the first sweep", h.k.Fake.Calls(), want)
			}
		})
	}
	// A post they send under their LID alone right after /ban <number>,
	// before any sweep, with no record pairing their two addresses, is
	// deleted on arrival: the LID WhatsApp gave for the number resolves the
	// ban at once.
	t.Run("ban-number-post-before-sweep-deleted", func(t *testing.T) {
		h := newHarness(t, "")
		h.k.Fake.LIDs = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
		recentJoiner(h)
		h.post("P1", modtest.G1, modtest.Spammer, "")
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		h.post("P2", modtest.G1, modtest.Spammer, "")
		h.k.Fire()
		if !h.revoked(modtest.G1, "P1") || !h.revoked(modtest.G1, "P2") {
			t.Fatalf("calls %v, rows %v, want P1 and P2 deleted", h.k.Fake.Calls(), rowsOf(h, modtest.SpammerM))
		}
		if bans := h.bans(); len(bans) != 1 || bans[0].Member != string(modtest.Spammer) ||
			bans[0].Phone != string(modtest.SpammerPhone) {
			t.Fatalf("bans %+v, want the number's ban resolved on their LID", bans)
		}
	})
	// /ban <number> for a number the ban command already banned (keyed by the
	// number) adds no second ban, but resolves that one on the LID WhatsApp
	// gives, so a post they send under their LID alone before the sweep is
	// deleted.
	t.Run("already-banned-by-cli-resolved", func(t *testing.T) {
		h := newHarness(t, "")
		h.k.Fake.LIDs = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
		recentJoiner(h)
		h.cliBan()
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if reply := h.lastReply(); !strings.Contains(reply, "already banned everywhere") {
			t.Fatalf("reply %q, want the already-banned reply", reply)
		}
		if bans := h.bans(); len(bans) != 1 || bans[0].Member != string(modtest.Spammer) ||
			bans[0].Phone != string(modtest.SpammerPhone) {
			t.Fatalf("bans %+v, want the one ban resolved on their LID", bans)
		}
		h.post("P2", modtest.G1, modtest.Spammer, "")
		h.k.Fire()
		if !h.revoked(modtest.G1, "P2") {
			t.Fatalf("calls %v, rows %v, want P2 deleted", h.k.Fake.Calls(), rowsOf(h, modtest.SpammerM))
		}
	})
	// When WhatsApp has no LID for the number, or cannot be asked, the ban
	// goes ahead by the number alone: the first is an Info line, the second
	// a Warn.
	for name, c := range map[string]struct {
		fail  error
		level string
	}{"no-lid": {level: "INFO"}, "lookup-failed": {fail: errors.New("synthetic transport error"), level: "WARN"}} {
		t.Run("ban-number-lookup-"+name, func(t *testing.T) {
			h := newHarness(t, "")
			recentJoiner(h)
			if c.fail != nil {
				h.k.Fake.FailNext("ResolvePhoneToLID", c.fail)
			}
			h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
			if reply := h.lastReply(); !strings.Contains(reply, "is banned everywhere") {
				t.Fatalf("reply %q, want the number banned", reply)
			}
			lines := h.k.LogLines("admin command by number: no LID found for the number; using the number alone")
			if len(lines) != 1 || !strings.Contains(lines[0], " level="+c.level+" ") {
				t.Fatalf("log lines %q, want one at %s", lines, c.level)
			}
			phone := string(modtest.SpammerPhone)
			if bans := h.bans(); len(bans) != 1 || bans[0].Member != phone || bans[0].LID != "" {
				t.Fatalf("bans %+v, want it keyed by %s with no LID, as `ban add` stores it", bans, phone)
			}
		})
	}
	// WhatsApp cannot be asked (rate-limited) and the record of their post
	// pairs their two addresses: /ban <number> resolves the ban on their LID,
	// and /unban <number> lifts a ban that names them by LID alone, as the
	// records are still read after WhatsApp.
	t.Run("ban-number-whatsapp-down-uses-record", func(t *testing.T) {
		h := newHarness(t, "")
		recentJoiner(h)
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.k.Fake.FailNext("ResolvePhoneToLID", fmt.Errorf("%w: status 429", client.ErrRateLimited))
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if bans := h.bans(); len(bans) != 1 || bans[0].Member != string(modtest.Spammer) ||
			bans[0].Phone != string(modtest.SpammerPhone) {
			t.Fatalf("bans %+v, want the number's ban resolved on their LID", bans)
		}
	})
	t.Run("unban-number-whatsapp-down-uses-record", func(t *testing.T) {
		h := newHarness(t, "")
		recentJoiner(h)
		h.post("P1", modtest.G1, modtest.Spammer, modtest.SpammerPhone)
		h.k.Ban(client.Member{LID: modtest.Spammer})
		h.k.Fake.FailNext("ResolvePhoneToLID", fmt.Errorf("%w: status 429", client.ErrRateLimited))
		h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
		if bans := h.bans(); len(bans) != 0 {
			t.Fatalf("bans %+v after /unban <number>, want the ban by their LID lifted; reply %q", bans, h.lastReply())
		}
	})
	// /ban <number> while a pause holds their ban leaves that held ban to
	// apply: when their LID is not known the number's ban puts none on the
	// ban list, so settling the held ban would leave their queued delete and
	// removal "not banned". When the pause ends, with no sweep between, the
	// post is deleted and they are removed.
	for _, tc := range []struct {
		name   string
		source string
		scope  store.Scope
	}{
		{"pause-all", store.SourceAdmin, store.ScopeAll},
		{"breaker-pause", store.SourceBreaker, store.ScopeRemoveBan},
	} {
		t.Run("ban-number-keeps-held-ban/"+tc.name, func(t *testing.T) {
			h := newHarness(t, "")
			recentJoiner(h)
			if err := h.k.Store.SetPause(h.k.Ctx, store.Pause{Source: tc.source, Scope: tc.scope, Reason: "paused",
				Since: h.k.Clock.Now()}); err != nil {
				t.Fatal(err)
			}
			h.k.Deliver(h.k.Spam("S1", modtest.G1))
			h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
			if held, err := h.k.Store.HeldBan(h.k.Ctx, []string{string(modtest.Spammer)}, ""); err != nil || !held {
				t.Fatalf("held %v (%v) after /ban <number>, want the held ban kept", held, err)
			}
			// The record of S1 gives their LID, so the number's ban is resolved
			// on it at once; the held ban is kept all the same.
			if bans := h.bans(); len(bans) != 1 || bans[0].Member != string(modtest.Spammer) {
				t.Fatalf("bans %+v, want the number's ban resolved on their LID", bans)
			}
			if err := h.k.Store.ClearPause(h.k.Ctx, tc.source); err != nil {
				t.Fatal(err)
			}
			h.k.Fire()
			if !h.revoked(modtest.G1, "S1") ||
				h.k.Fake.Count("Remove "+string(modtest.G1)+" "+string(modtest.Spammer)) != 1 {
				t.Fatalf("calls %v, rows %v; want S1 deleted and them removed", h.k.Fake.Calls(), rowsOf(h, modtest.SpammerM))
			}
		})
	}
}

// TestBanNumberCommunityScope (AC 5.1, 5.2): a ban one community holds (from
// bans.scope per_community) is lifted by /unban <number>, and, left over
// after a switch to all_communities, does not make /ban <number> answer
// "already banned".
func TestBanNumberCommunityScope(t *testing.T) {
	communityBan := func(t *testing.T, h *harness) {
		t.Helper()
		h.k.Deliver(h.k.Spam("S1", modtest.G1))
		h.k.Fire()
		if bans := h.bans(); len(bans) != 1 || bans[0].Scope != string(modtest.Community) {
			t.Fatalf("premise: want one ban scoped to the community, got %+v", bans)
		}
	}
	t.Run("unban-lifts-community-ban", func(t *testing.T) {
		h := newHarness(t, "bans:\n  scope: per_community\n")
		communityBan(t, h)
		h.command(telegramtest.ChatID, adminUser, "/unban "+spammerNumber, 0)
		if reply := h.lastReply(); !strings.Contains(reply, "(1 ban(s) lifted)") || len(h.bans()) != 0 ||
			h.k.Banned(modtest.SpammerM, "") {
			t.Fatalf("reply %q, bans left %+v; want the community's ban lifted", reply, h.bans())
		}
	})
	t.Run("community-ban-not-already-everywhere", func(t *testing.T) {
		h := newHarness(t, "bans:\n  scope: per_community\n")
		communityBan(t, h)
		h.k.ReloadConfig(modtest.Config)
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if reply := h.lastReply(); strings.Contains(reply, "already banned") || !h.k.Banned(modtest.SpammerM, string(modtest.SetB)) {
			t.Fatalf("reply %q, bans %+v; want them banned everywhere", reply, h.bans())
		}
	})
}

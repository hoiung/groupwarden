package telegram_test

import (
	"database/sql"
	"errors"
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
// membercmds.go ban: the phone JID is the key and the phone, no LID until the
// sweep resolves it, scope everywhere under bans.scope all_communities),
// with a ban row triggered from the admin chat that names who sent it. A
// number of fewer than 8 or more than 15 digits, or with letters, gets the
// usage text and bans nobody; a number already banned gets a reply saying so
// and changes nothing; /ban as a reply to a report works as before.
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
			b := bans[0]
			if b.Member != phone || b.Phone != phone || b.LID != "" || b.Scope != store.BanEverywhere || b.LedgerID == 0 {
				t.Fatalf("ban %+v, want it keyed by %s with no LID, everywhere, as `ban add` stores it", b, phone)
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
	t.Run("already-banned", func(t *testing.T) {
		h := newHarness(t, "")
		h.k.Ban(modtest.SpammerM)
		before, bans := h.ledgerRows(), h.bans()
		h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
		if got := h.lastReply(); got != "phone…0123 is already banned; nothing changed." {
			t.Fatalf("reply %q", got)
		}
		if after := h.bans(); h.ledgerRows() != before || len(after) != len(bans) || after[0] != bans[0] {
			t.Fatalf("the ban list or the ledger changed: %+v → %+v", bans, after)
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
		for _, r := range h.k.Rows(modtest.SpammerM) {
			if r.Action == store.ActRevoke && r.Mode == store.ModeEnforce && r.MsgID != "P1" {
				t.Fatalf("row %+v, want no enforced delete outside the enforce community", r)
			}
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

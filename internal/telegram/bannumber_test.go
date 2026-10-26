package telegram_test

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
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
}

package telegram_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// TestOneBanPerPerson (#8 AC 3.4): one ban list entry per person and scope,
// tested as one class: each kind of entry the person already has (the bot's,
// written straight to the list so a ban a pause holds is not settled by it)
// meets each way of banning them, and they end with one entry: the ban list
// holds one line for them, [Undo] replies "(1 ban(s) lifted" and /status
// shows "Bans: 1". Another person's entry holding the same number (its old
// owner) stays theirs when the new ban knows the current holder's LID (two
// entries, one each; [Undo] is not pressed there, as it lifts every ban
// holding the person's number) and is the one entry when the new ban knows
// only the number. A ban that knows only the address the entry lacks, with
// nothing pairing the two when it is written, is a second entry until the
// sweep merges them (the Design's known limit), as is the bot's own ban of a
// post sent under the LID alone after a ban by number. The cells not in
// fixed also pass at 47b8ebf (guards).
func TestOneBanPerPerson(t *testing.T) {
	S, P, O := string(modtest.Spammer), string(modtest.SpammerPhone), string(modtest.Other1)
	entries := map[string]struct {
		ban   store.Ban
		purge bool // its ledger row purged since
	}{
		"lid-only":        {ban: store.Ban{Member: S, LID: S}},
		"number-only":     {ban: store.Ban{Member: P, Phone: P}},
		"lid-with-number": {ban: store.Ban{Member: S, LID: S, Phone: P}},
		"row-purged":      {ban: store.Ban{Member: S, LID: S, Phone: P}, purge: true},
		"other-lid":       {ban: store.Ban{Member: O, LID: O, Phone: P}},
	}
	// Each way runs its first part before the entry is on the list and
	// returns the rest. lid: the new ban knows their LID.
	ways := map[string]struct {
		lid bool
		run func(t *testing.T, h *harness) func()
	}{
		"ban-add-lid": {lid: true, run: func(t *testing.T, h *harness) func() {
			return func() { h.cliBanOf(client.Member{LID: modtest.Spammer}) }
		}},
		"ban-add-phone": {run: func(t *testing.T, h *harness) func() {
			return func() { h.cliBanOf(client.Member{Phone: modtest.SpammerPhone}) }
		}},
		"chat-ban-number": {lid: true, run: func(t *testing.T, h *harness) func() {
			return func() {
				h.command(telegramtest.ChatID, adminUser, "/ban "+spammerNumber, 0)
				if got := h.lastReply(); !strings.Contains(got, "phone…0123 is") {
					t.Fatalf("/ban replied %q", got)
				}
			}
		}},
		"button": {lid: true, run: func(t *testing.T, h *harness) func() {
			r := watchOnly(t, h)
			return func() {
				h.press(adminUser, "ban", r.ID)
				if got := h.lastReply(); !strings.HasPrefix(got, "Banned by Ann") {
					t.Fatalf("[Ban] replied %q", got)
				}
			}
		}},
		// The bot's ban of a post sent while a pause held bans, applied once
		// the pause ends: the entry went on the list meanwhile.
		"held-applied": {lid: true, run: func(t *testing.T, h *harness) func() {
			if err := h.k.Store.SetPause(h.k.Ctx, store.Pause{Source: store.SourceBreaker, Scope: store.ScopeRemoveBan,
				Reason: "too many removals", Since: h.k.Clock.Now()}); err != nil {
				t.Fatal(err)
			}
			h.k.Deliver(h.k.Spam("S1", modtest.G1))
			if held, err := h.k.Store.HeldBan(h.k.Ctx, []string{S}, ""); err != nil || !held {
				t.Fatalf("held %v (%v), want the bot's ban held", held, err)
			}
			return func() {
				if err := h.k.Store.ClearPause(h.k.Ctx, store.SourceBreaker); err != nil {
					t.Fatal(err)
				}
				h.k.Fire()
				if held, err := h.k.Store.HeldBan(h.k.Ctx, []string{S}, ""); err != nil || held {
					t.Fatalf("held %v (%v) after the pause, want the bot's ban applied", held, err)
				}
			}
		}},
	}
	// The known limit: the new ban knows only the address the entry lacks.
	unpaired := map[string]bool{"lid-only/ban-add-phone": true, "number-only/ban-add-lid": true}
	// The cells 47b8ebf fails; every other cell is a guard (the baseline run
	// against 47b8ebf checks both). The two unpaired cells fail there after
	// the sweep, which asked WhatsApp alone and not the group list.
	fixed := map[string]bool{"number-only/button": true, "number-only/held-applied": true,
		"lid-with-number/ban-add-phone": true, "row-purged/ban-add-phone": true, "other-lid/ban-add-phone": true,
		"other-lid/chat-ban-number": true, "lid-only/ban-add-phone": true, "number-only/ban-add-lid": true}
	for en, e := range entries {
		for wn, w := range ways {
			name := en + "/" + wn
			t.Run(name, func(t *testing.T) {
				if !fixed[name] {
					t.Log("guard: passes at 47b8ebf too")
				}
				h := newHarness(t, "")
				rest := w.run(t, h)
				putEntry(h, e.ban, e.purge)
				rest()
				want := 1
				switch {
				case en == "other-lid" && w.lid, unpaired[name]:
					want = 2
				}
				if got := h.bans(); len(got) != want {
					t.Fatalf("bans %+v, want %d", got, want)
				}
				if unpaired[name] {
					if _, err := h.app.Sweep.Run(h.k.Ctx, "r1"); err != nil {
						t.Fatal(err)
					}
					want = 1
					if got := h.bans(); len(got) != want {
						t.Fatalf("bans %+v after the sweep, want them merged", got)
					}
				}
				oneBanCounts(t, h, want, en != "other-lid")
			})
		}
	}
	// The bot's own ban of a post sent under the LID alone, after a ban of
	// their number nothing pairs with that LID yet: two entries until the
	// sweep finds the LID (here in the saved WhatsApp IDs) and merges them.
	t.Run("number-only/bot-ban-lid-only-post", func(t *testing.T) {
		t.Log("guard: passes at 47b8ebf too")
		h := newHarness(t, "")
		recentJoiner(h)
		putEntry(h, entries["number-only"].ban, false)
		s := h.k.Spam("S1", modtest.G1)
		s.SenderAlt = ""
		h.k.Deliver(s)
		if got := h.bans(); len(got) != 2 {
			t.Fatalf("bans %+v, want the number's and the LID's", got)
		}
		h.k.Fake.Saved = map[client.JID]client.JID{modtest.SpammerPhone: modtest.Spammer}
		if _, err := h.app.Sweep.Run(h.k.Ctx, "r1"); err != nil {
			t.Fatal(err)
		}
		if got := h.bans(); len(got) != 1 {
			t.Fatalf("bans %+v after the sweep, want them merged", got)
		}
		oneBanCounts(t, h, 1, true)
	})
}

// putEntry puts b on the ban list everywhere as the bot wrote it (linked to a
// ban row of a post's trigger), with no ledger plan, so a ban a pause holds is
// not settled by it; purge deletes that row afterwards.
func putEntry(h *harness, b store.Ban, purge bool) {
	h.t.Helper()
	now := h.k.Clock.Now()
	if err := h.k.Store.Write(h.k.Ctx, func(tx *sql.Tx) error {
		id, _, err := store.InsertLedger(h.k.Ctx, tx, store.LedgerRow{Action: store.ActBan, Chat: store.BanEverywhere,
			Target: b.Member, Address: b.Phone, TriggerID: "M0", Community: string(modtest.Community),
			Mode: store.ModeEnforce, Status: store.Requested, Reason: "spam post"}, now)
		if err != nil {
			return err
		}
		if purge {
			if _, err := tx.ExecContext(h.k.Ctx, `DELETE FROM ledger WHERE id = ?`, id); err != nil {
				return err
			}
		}
		b.Scope, b.LedgerID, b.Reason = store.BanEverywhere, id, "spam post"
		return store.AddBan(h.k.Ctx, tx, b, now)
	}); err != nil {
		h.t.Fatal(err)
	}
}

// oneBanCounts checks what the admins see: /status counts total bans, and
// [Undo] on a report about the kit spammer (when undo) lifts their one ban.
func oneBanCounts(t *testing.T, h *harness, total int, undo bool) {
	t.Helper()
	h.command(telegramtest.ChatID, adminUser, "/status", 0)
	if got := h.lastReply(); !strings.Contains(got, fmt.Sprintf("Bans: %d;", total)) {
		t.Fatalf("/status %q, want Bans: %d", got, total)
	}
	if !undo {
		return
	}
	id := h.report(store.Report{Kind: ledger.KindAction, Community: string(modtest.Community), Subject: string(modtest.Spammer),
		Buttons: []string{ledger.ButtonUndo}, Text: "Spam in general matched rule pitch."})
	h.drain()
	h.press(adminUser, "undo", id)
	if got := h.lastReply(); !strings.Contains(got, "(1 ban(s) lifted,") {
		t.Fatalf("[Undo] replied %q, want one ban lifted", got)
	}
}

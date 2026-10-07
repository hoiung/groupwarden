package telegram_test

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config/configtest"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// TestStatusCommand: /status shows the connection, the last event's age,
// each community's mode and groups (covered / absent / not admin, human
// admins per group), pauses, the config hash with the last sync, the sync
// and backup timers, the ban count and stale ledger rows.
func TestStatusCommand(t *testing.T) {
	h := newHarness(t, "")
	// The community also has a group the bot is not in, and the bot is not an
	// admin in "jobs".
	h.k.Fake.Linked = map[client.JID][]client.GroupRef{modtest.Community: {
		{JID: modtest.G1, Name: "general"}, {JID: modtest.G2, Name: "jobs"}, {JID: "99999000000333@g.us", Name: "events"}}}
	h.k.Fake.Groups = modtest.Groups()
	h.k.Fake.Groups[2].Participants[0].IsAdmin = false
	h.app.Executor = nil // the test fires nothing
	ctx, cancel := context.WithCancel(h.k.Ctx)
	done := make(chan error, 1)
	go func() { done <- h.app.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(10 * time.Second)
	for h.app.Sweep.Coverage().At.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("no sweep after connecting")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.k.Ban(modtest.Other2M)
	if err := h.k.Store.SetPause(h.k.Ctx, store.Pause{Source: store.SourceBreaker, Scope: store.ScopeRemoveBan,
		Reason: "too many removals", Since: h.k.Clock.Now()}); err != nil {
		t.Fatal(err)
	}
	// A ban the pause holds is counted apart from the ban list.
	if err := h.k.Store.Write(h.k.Ctx, func(tx *sql.Tx) error {
		_, err := ledger.Write(h.k.Ctx, tx, ledger.Plan{Trigger: "H1", Target: modtest.SpammerM, ConfigHash: "h",
			Ban: []string{store.BanEverywhere}, BanEnforce: true, BanHeld: true, BanCommunity: string(modtest.Community),
			Reason: "spam post"}, h.k.Clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.k.Store.SetStatus(h.k.Ctx, map[string]string{
		store.StatusSyncLastRun: strconv.FormatInt(h.k.Clock.Now().Add(-3*time.Minute).UnixMilli(), 10),
		store.StatusSyncResult:  "ok at abc1234"}); err != nil {
		t.Fatal(err)
	}
	h.command(telegramtest.ChatID, adminUser, "/status", 0)
	got := h.lastReply()
	want := []string{"WhatsApp: connected; last event", "Paused: removals and bans (deletes continue) since",
		"(breaker): too many removals", "Config: v" + h.k.Holder.Current().Hash + "; last sync 3m0s ago (ok at abc1234)",
		"Backup: last run never", "Bans: 1 (+1 held by a pause: they apply after [Resume]);",
		"stale ledger rows (queued over 1h0m0s): 0",
		"community a (enforce): 1 covered, 1 absent, 1 not admin", "general (group…0111): covered, 1 human admin(s)",
		"jobs (group…0222): not admin, 1 human admin(s)", "events (group…0333): absent",
		"set b (enforce): 1 covered, 0 absent, 0 not admin"}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Fatalf("/status lacks %q:\n%s", w, got)
		}
	}
}

// TestPauseResume: /pause stops every action, deletes included; /resume
// lifts every pause and the queued actions go (each checked again).
func TestPauseResume(t *testing.T) {
	h := newHarness(t, "")
	h.command(telegramtest.ChatID, adminUser, "/pause", 0)
	if !strings.Contains(h.lastReply(), "every action stops, deletes included") {
		t.Fatalf("reply %q", h.lastReply())
	}
	h.k.Deliver(h.k.Spam("P1", modtest.G1))
	h.k.Fire()
	if n := len(h.k.Fake.Calls()); n != 0 {
		t.Fatalf("calls while paused: %v", h.k.Fake.Calls())
	}
	h.command(telegramtest.ChatID, adminUser, "/resume", 0)
	if !strings.Contains(h.lastReply(), "Resumed by Ann: 1 pause(s) lifted") {
		t.Fatalf("reply %q", h.lastReply())
	}
	h.k.Fire()
	if h.k.Fake.Count("Revoke") != 1 || h.k.Fake.Count("Remove") != 3 {
		t.Fatalf("calls after /resume %v", h.k.Fake.Calls())
	}
}

// TestReloadCommand: /reload swaps in a valid config (the reply names its
// version) and refuses a broken one (REJECTED, the old one keeps running).
func TestReloadCommand(t *testing.T) {
	h := newHarness(t, "")
	old := h.k.Holder.Current().Hash
	write := func(cfg string) {
		if err := os.WriteFile(h.k.CfgPath, []byte(configtest.Base+cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(modtest.Config + "report:\n  attachment_show_hours: 12\n")
	h.command(telegramtest.ChatID, adminUser, "/reload", 0)
	now := h.k.Holder.Current().Hash
	if now == old || h.lastReply() != "config v"+now+" loaded" {
		t.Fatalf("reply %q (hash %s → %s)", h.lastReply(), old, now)
	}
	write(modtest.Config + "report:\n  attachment_show_hours: nope\n")
	h.command(telegramtest.ChatID, adminUser, "/reload", 0)
	if !strings.Contains(h.lastReply(), "REJECTED") || h.k.Holder.Current().Hash != now {
		t.Fatalf("reply %q, hash %s (want still %s)", h.lastReply(), h.k.Holder.Current().Hash, now)
	}
}

// TestJoinRefusesForeignCommunity: /join joins only a group whose parent is
// a configured community.
func TestJoinRefusesForeignCommunity(t *testing.T) {
	h := newHarness(t, "")
	h.k.Fake.Invites = map[string]client.Group{
		"FoReIgN1": {JID: "99999000000333@g.us", Name: "elsewhere", Parent: "99999000000777@g.us"},
		"OuRs1":    {JID: "99999000000333@g.us", Name: "events", Parent: modtest.Community},
	}
	h.command(telegramtest.ChatID, adminUser, "/join https://chat.whatsapp.com/FoReIgN1", 0)
	if h.k.Fake.Count("JoinWithLink") != 0 || !strings.Contains(h.lastReply(), "Not joined") {
		t.Fatalf("calls %v reply %q", h.k.Fake.Calls(), h.lastReply())
	}
	h.command(telegramtest.ChatID, adminUser, "/join https://chat.whatsapp.com/OuRs1", 0)
	if h.k.Fake.Count("JoinWithLink OuRs1") != 1 || !strings.Contains(h.lastReply(), "promote the bot to admin") {
		t.Fatalf("calls %v reply %q", h.k.Fake.Calls(), h.lastReply())
	}
	h.command(telegramtest.ChatID, adminUser, "/join https://example.org/x", 0)
	if !strings.Contains(h.lastReply(), "not a WhatsApp group invite link") {
		t.Fatalf("reply %q", h.lastReply())
	}
}

// TestBanUnbanCommands: /ban as a reply to a report acts as [Ban] (same
// press key); /unban <report> acts as [Undo].
func TestBanUnbanCommands(t *testing.T) {
	h := newHarness(t, "")
	r := watchOnly(t, h)
	h.command(telegramtest.ChatID, adminUser, "/ban", h.head(r.ID))
	h.k.Fire()
	if !h.k.Banned(modtest.SpammerM, "") || h.k.Fake.Count("Remove") != 3 {
		t.Fatalf("/ban: banned=%v calls %v", h.k.Banned(modtest.SpammerM, ""), h.k.Fake.Calls())
	}
	h.press(secondAdm, "ban", r.ID)
	if !strings.Contains(h.answers()[0], "already done by Ann") {
		t.Fatalf("[Ban] after /ban answered %q", h.answers())
	}
	h.command(telegramtest.ChatID, adminUser, "/unban "+strconv.FormatInt(r.ID, 10), 0)
	if h.k.Banned(modtest.SpammerM, "") {
		t.Fatal("still banned after /unban")
	}
	h.command(telegramtest.ChatID, adminUser, "/ban", 0)
	if !strings.Contains(h.lastReply(), "as a reply to a report") {
		t.Fatalf("bare /ban reply %q", h.lastReply())
	}
	h.command(telegramtest.ChatID, otherUser, "/unban "+strconv.FormatInt(r.ID, 10), 0)
	if !strings.Contains(h.lastReply(), "Only admins") {
		t.Fatalf("/unban by a non-admin: %q", h.lastReply())
	}
	if rows := rowsOf(h, modtest.SpammerM); rows["unban/requested"] != 1 {
		t.Fatalf("rows %v", rows)
	}
}

// TestBanCommandStoreReadFailure: when the store cannot say which report a
// /ban replies to, the admin is told to try again (not that the message is not
// a report) and the error is logged.
func TestBanCommandStoreReadFailure(t *testing.T) {
	h := newHarness(t, "")
	r := watchOnly(t, h)
	head := h.head(r.ID)
	_ = h.k.Store.Close()
	h.command(telegramtest.ChatID, adminUser, "/ban", head)
	if got := h.lastReply(); got != "Could not read the report store; try again." {
		t.Fatalf("reply %q", got)
	}
	if !strings.Contains(h.logs.String(), "read the report a /ban replies to") {
		t.Fatalf("no log line for the failed read:\n%s", h.logs)
	}
}

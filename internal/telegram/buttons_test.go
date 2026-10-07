package telegram_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// rowsOf counts member's ledger rows by action and status.
func rowsOf(h *harness, m client.Member) map[string]int {
	out := map[string]int{}
	for _, r := range h.k.Rows(m) {
		out[string(r.Action)+"/"+string(r.Status)]++
	}
	return out
}

// TestUndoUnbansAndOverturns: [Undo] lifts the ban everywhere, marks the
// removals and the ban overturned (the delete, already sent, stays), and
// records who pressed it.
func TestUndoUnbansAndOverturns(t *testing.T) {
	h := newHarness(t, "")
	r := h.spam("U1")
	h.drain()
	if !h.k.Banned(modtest.SpammerM, "") {
		t.Fatal("the spammer was not banned")
	}
	h.press(adminUser, "undo", r.ID)
	if h.k.Banned(modtest.SpammerM, "") {
		t.Fatal("still banned after [Undo]")
	}
	got := rowsOf(h, modtest.SpammerM)
	if got["remove/overturned"] != 3 || got["ban/overturned"] != 1 || got["revoke/requested"] != 1 ||
		got["remove/requested"] != 0 || got["unban/requested"] != 1 {
		t.Fatalf("rows %v, want 3 removals + the ban overturned, the delete kept, one unban", got)
	}
	for _, row := range h.k.Rows(modtest.SpammerM) {
		if row.Action == store.ActUnban && (row.TriggerID != "tg:"+strconv.FormatInt(r.ID, 10)+":undo" ||
			row.Actor != "telegram:501 (Ann)") {
			t.Fatalf("unban row trigger %q actor %q", row.TriggerID, row.Actor)
		}
	}
	first, claimed, err := h.k.Store.ClaimPress(h.k.Ctx, store.Press{ReportID: r.ID, Button: ledger.ButtonUndo, UserID: 9})
	if err != nil || claimed || first.UserID != adminUser || first.Result == "" {
		t.Fatalf("press record %+v claimed=%v (%v), want Ann's press with its result", first, claimed, err)
	}
}

// TestUndoListsGroups: the [Undo] reply lists every group the person was
// removed from, to re-invite them by hand.
func TestUndoListsGroups(t *testing.T) {
	h := newHarness(t, "")
	r := h.spam("U2")
	h.drain()
	h.press(adminUser, "undo", r.ID)
	reply := h.lastReply()
	if !contains(reply, "Re-invite them by hand to:", "general (group…0111)", "jobs (group…0222)",
		"community a (group…0999)", "cannot be restored") {
		t.Fatalf("reply %q", reply)
	}
	if !strings.HasPrefix(h.answers()[0], "Undone by Ann") {
		t.Fatalf("the press was answered %q", h.answers())
	}
}

// watchOnly delivers a match of the watch-only "lure" rule and posts it.
func watchOnly(t *testing.T, h *harness) store.Report {
	t.Helper()
	m := h.k.Msg("W1", modtest.G1, modtest.Spammer, "crypto: inbox me")
	m.SenderAlt = modtest.SpammerPhone
	h.k.Deliver(m)
	h.k.Fire()
	h.drain()
	r := h.only(ledger.KindWouldHaveActed)
	if !slices.Contains(r.Buttons, ledger.ButtonBan) {
		t.Fatalf("watch-only report buttons %v", r.Buttons)
	}
	if h.k.Fake.Count("Revoke") != 0 || h.k.Banned(modtest.SpammerM, "") {
		t.Fatal("a watch-only match acted")
	}
	return r
}

// TestBanFromWatchOnlyReport: [Ban] on a "would have acted" report deletes
// the message, removes the sender everywhere and bans them.
func TestBanFromWatchOnlyReport(t *testing.T) {
	h := newHarness(t, "")
	r := watchOnly(t, h)
	h.press(adminUser, "ban", r.ID)
	h.k.Fire()
	if !h.k.Banned(modtest.SpammerM, "") {
		t.Fatal("not banned after [Ban]")
	}
	if h.k.Fake.Count("Revoke "+string(modtest.G1)) != 1 || h.k.Fake.Count("Remove") != 3 {
		t.Fatalf("calls %v, want the delete and 3 removals", h.k.Fake.Calls())
	}
	if !contains(h.lastReply(), "Banned by Ann", "the message is being deleted", "removed from 3 group(s) and banned") {
		t.Fatalf("reply %q", h.lastReply())
	}
}

// TestBanDuringPauseSaysWhatWaits: [Ban] during /pause bans at once, and its
// reply says the delete and the removals wait for the pause to end.
func TestBanDuringPauseSaysWhatWaits(t *testing.T) {
	h := newHarness(t, "")
	r := watchOnly(t, h)
	if err := h.k.Store.SetPause(h.k.Ctx, store.Pause{Source: store.SourceAdmin, Scope: store.ScopeAll, Reason: "test",
		Since: h.k.Clock.Now()}); err != nil {
		t.Fatal(err)
	}
	h.press(adminUser, "ban", r.ID)
	h.k.Fire()
	if !h.k.Banned(modtest.SpammerM, "") || h.k.Fake.Count("Revoke")+h.k.Fake.Count("Remove") != 0 {
		t.Fatalf("banned %v, calls %v: want the ban at once and nothing sent", h.k.Banned(modtest.SpammerM, ""),
			h.k.Fake.Calls())
	}
	if !contains(h.lastReply(), "the message is deleted once the pause ends",
		"is banned; their removal from 3 group(s) waits for the pause to end") {
		t.Fatalf("reply %q", h.lastReply())
	}
}

// TestWatchOnlyBanDeletesOnlyWithinReplayWindow: past act_on_replay_max_age
// [Ban] removes and bans but leaves the message alone.
func TestWatchOnlyBanDeletesOnlyWithinReplayWindow(t *testing.T) {
	h := newHarness(t, "")
	r := watchOnly(t, h)
	h.k.Clock.Advance(48 * time.Hour) // act_on_replay_max_age is 47h
	h.press(adminUser, "ban", r.ID)
	h.k.Fire()
	if h.k.Fake.Count("Revoke") != 0 {
		t.Fatalf("an old message was deleted: %v", h.k.Fake.Calls())
	}
	if h.k.Fake.Count("Remove") != 3 || !h.k.Banned(modtest.SpammerM, "") {
		t.Fatalf("calls %v banned=%v, want 3 removals and the ban", h.k.Fake.Calls(), h.k.Banned(modtest.SpammerM, ""))
	}
	for _, row := range h.k.Rows(modtest.SpammerM) {
		if row.Action == store.ActRevoke && strings.HasPrefix(row.TriggerID, store.AdminTrigger) {
			t.Fatalf("[Ban] planned a delete of an old message: %+v", row)
		}
	}
	if !strings.Contains(h.lastReply(), "older than act_on_replay_max_age") {
		t.Fatalf("reply %q", h.lastReply())
	}
}

// TestWatchOnlyBanOfAnEditAgedByTheOriginal: [Ban] on a watch-only report
// about an edit measures act_on_replay_max_age from the original post's
// server time, as the inbox does: pressed when the original is past it and
// the edit is not, the original is left alone; pressed earlier, it is
// deleted, unless the delete fires only once the original is past it.
func TestWatchOnlyBanOfAnEditAgedByTheOriginal(t *testing.T) {
	for _, c := range []struct {
		name     string
		wait     time.Duration // after the edit; the original is 10 minutes older
		late     time.Duration // from the press to the delete firing
		deleted  bool
		replyOld bool
	}{
		{"original past the window", 47*time.Hour - 5*time.Minute, 0, false, true},
		{"original within the window", 47*time.Hour - 15*time.Minute, 0, true, false},
		{"delete fires once the original is past it", 47*time.Hour - 15*time.Minute, 10 * time.Minute, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, "")
			orig := h.k.Msg("O1", modtest.G1, modtest.Spammer, "hello everyone")
			orig.SenderAlt = modtest.SpammerPhone
			h.k.Deliver(orig)
			h.k.Clock.Advance(10 * time.Minute)
			edit := h.k.Msg("E1", modtest.G1, modtest.Spammer, "crypto: inbox me")
			edit.SenderAlt, edit.TargetID, edit.IsEdit = modtest.SpammerPhone, "O1", true
			h.k.Deliver(edit)
			h.k.Fire()
			h.drain()
			r := h.only(ledger.KindWouldHaveActed)
			h.k.Clock.Advance(c.wait)
			h.press(adminUser, "ban", r.ID)
			reply := h.lastReply()
			h.k.Clock.Advance(c.late)
			h.k.Fire()
			if got := h.k.Fake.Count("Revoke "+string(modtest.G1)+" "+string(modtest.Spammer)+" O1") == 1; got != c.deleted {
				t.Fatalf("original deleted %v, want %v: %v", got, c.deleted, h.k.Fake.Calls())
			}
			if !h.k.Banned(modtest.SpammerM, "") {
				t.Fatal("not banned after [Ban]")
			}
			if old := strings.Contains(reply, "older than act_on_replay_max_age"); old != c.replyOld {
				t.Fatalf("reply %q", reply)
			}
		})
	}
}

// TestCallbackFromNonAdminRejected: a press by someone who is not an admin
// of the chat (checked at press time) does nothing; a demoted admin is
// refused too.
func TestCallbackFromNonAdminRejected(t *testing.T) {
	h := newHarness(t, "")
	r := h.spam("N1")
	h.drain()
	h.press(otherUser, "undo", r.ID)
	h.srv.SetAdmin(secondAdm, false) // demoted since the report went out
	h.press(secondAdm, "undo", r.ID)
	if !h.k.Banned(modtest.SpammerM, "") {
		t.Fatal("a non-admin's [Undo] unbanned the spammer")
	}
	for _, a := range h.answers() {
		if !strings.Contains(a, "Only admins") {
			t.Fatalf("answers %q", h.answers())
		}
	}
	if _, claimed, _ := h.k.Store.ClaimPress(h.k.Ctx, store.Press{ReportID: r.ID, Button: ledger.ButtonUndo}); !claimed {
		t.Fatal("a refused press was recorded")
	}
}

// TestCommandFromOtherChatIgnored: a command in another group or in a direct
// message is ignored without a reply, and a button pressed outside the admin
// chat does nothing; the same command in the admin chat works.
func TestCommandFromOtherChatIgnored(t *testing.T) {
	h := newHarness(t, "")
	h.command(-1009999000555, adminUser, "/pause", 0)
	h.command(adminUser, adminUser, "/pause", 0) // a direct message
	if paused, _ := h.k.Store.PausedFor(h.k.Ctx, store.ScopeAll); paused {
		t.Fatal("a command from another chat paused the bot")
	}
	if n := len(h.srv.Posted()); n != 0 {
		t.Fatalf("%d replies to ignored chats", n)
	}
	// A button pressed on a copy of a report in another chat does nothing.
	r := h.spam("O1")
	h.drain()
	h.pressIn(-1009999000555, adminUser, "undo", r.ID)
	if a := h.answers(); len(a) != 1 || !strings.Contains(a[0], "not the groupwarden admin chat") {
		t.Fatalf("answers %q", a)
	}
	if rows := rowsOf(h, modtest.SpammerM); rows["unban/requested"] != 0 || !h.k.Banned(modtest.SpammerM, "") {
		t.Fatalf("a press from another chat acted: rows %v", rows)
	}
	h.command(telegramtest.ChatID, adminUser, "/pause@groupwarden_test_bot", 0)
	if paused, _ := h.k.Store.PausedFor(h.k.Ctx, store.ScopeAll); !paused {
		t.Fatal("/pause in the admin chat did nothing")
	}
}

// TestButtonIdempotent: presses are keyed report + button; a repeat (by
// anyone) is answered with the first press and does nothing again.
func TestButtonIdempotent(t *testing.T) {
	h := newHarness(t, "")
	r := h.spam("I1")
	h.drain()
	h.press(adminUser, "undo", r.ID)
	h.press(secondAdm, "undo", r.ID)
	h.command(telegramtest.ChatID, secondAdm, "/unban "+strconv.FormatInt(r.ID, 10), 0)
	if got := rowsOf(h, modtest.SpammerM)["unban/requested"]; got != 1 {
		t.Fatalf("%d unban rows, want 1", got)
	}
	answers := h.answers()
	if len(answers) != 2 || !strings.Contains(answers[1], "already done by Ann") {
		t.Fatalf("answers %q", answers)
	}
	if !strings.Contains(h.lastReply(), "already done by Ann") {
		t.Fatalf("/unban reply %q", h.lastReply())
	}
	// A press for a button the report does not carry is refused.
	h.press(adminUser, "ban", r.ID)
	if !strings.Contains(h.answers()[2], "has no [Ban] button") {
		t.Fatalf("answer %q", h.answers()[2])
	}
}

// stopping is the app's controls with an [Undo] that is cut short by a stop:
// it cancels the press's context and fails.
type stopping struct {
	telegram.Controls
	cancel func()
}

func (s stopping) Undo(ctx context.Context, _ int64, _ telegram.Actor) (string, error) {
	s.cancel()
	return "", ctx.Err()
}

// TestUnfinishedPressCanBePressedAgain: a press a run claimed and never
// finished does not answer "already done" for good. One whose action failed
// as the run stopped (its context cancelled) is released at once; one the run
// stopped in the middle of (no result recorded) is released when the next run
// starts.
func TestUnfinishedPressCanBePressedAgain(t *testing.T) {
	t.Run("failed as the run stopped", func(t *testing.T) {
		h := newHarness(t, "")
		r := h.spam("I1")
		h.drain()
		ctx, cancel := context.WithCancel(h.k.Ctx)
		controls := h.chat.Controls
		h.chat.Controls = stopping{Controls: controls, cancel: cancel}
		h.pressWith(ctx, telegramtest.ChatID, adminUser, "undo", r.ID)
		h.chat.Controls = controls
		h.press(adminUser, "undo", r.ID)
		if h.k.Banned(modtest.SpammerM, "") {
			t.Fatalf("still banned after [Undo] was pressed again; answers %q", h.answers())
		}
	})
	t.Run("stopped mid-action", func(t *testing.T) {
		h := newHarness(t, "")
		r := h.spam("I1")
		// A press that finished keeps answering "already done" after the restart.
		finished := h.report(store.Report{Kind: string(alert.PhoneReminder), Text: "Weekly check: open WhatsApp on the bot phone.",
			Buttons: []string{ledger.ButtonDone}})
		h.drain()
		h.press(adminUser, "done", finished)
		if _, claimed, err := h.k.Store.ClaimPress(h.k.Ctx, store.Press{ReportID: r.ID, Button: ledger.ButtonUndo,
			UserID: adminUser, UserName: "Ann"}); err != nil || !claimed {
			t.Fatalf("claim: %v %v", claimed, err)
		}
		h.restart()
		ctx, cancel := context.WithCancel(h.k.Ctx)
		done := make(chan struct{})
		go func() {
			h.chat.Run(ctx)
			close(done)
		}()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) &&
			!strings.Contains(h.logs.String(), "released presses"); {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		<-done
		h.press(adminUser, "undo", r.ID)
		if h.k.Banned(modtest.SpammerM, "") {
			t.Fatalf("still banned after [Undo] in the next run; answers %q", h.answers())
		}
		h.press(secondAdm, "done", finished)
		if a := h.answers(); !strings.Contains(a[len(a)-1], "already done by Ann") {
			t.Fatalf("a finished press after the restart answered %q, want already done", a[len(a)-1])
		}
	})
}

// TestDoneButtonRecordsPhoneOpened: [Done] on the phone reminder records when
// the bot phone was opened (the app counts the next reminder from it); a
// second press is answered "already done".
func TestDoneButtonRecordsPhoneOpened(t *testing.T) {
	h := newHarness(t, "")
	id := h.report(store.Report{Kind: string(alert.PhoneReminder), Text: "Weekly check: open WhatsApp on the bot phone.",
		Buttons: []string{ledger.ButtonDone}})
	h.drain()
	pressed := h.k.Clock.Now() // the reply after it waits its send slot, moving the clock on
	h.press(adminUser, "done", id)
	st, err := h.k.Store.Status(h.k.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := st[store.StatusPhoneDone].Value, strconv.FormatInt(pressed.UnixMilli(), 10); got != want {
		t.Fatalf("phone_done %q, want %q (the press time)", got, want)
	}
	if !strings.Contains(h.lastReply(), "Thanks, Ann") {
		t.Fatalf("reply %q", h.lastReply())
	}
	h.press(secondAdm, "done", id)
	if answers := h.answers(); len(answers) != 2 || !strings.Contains(answers[1], "already done by Ann") {
		t.Fatalf("answers %q", answers)
	}
}

// TestEveryButtonHasACode: every report button has a callback code, and the
// codes are unique (a button without one would never be pressable).
func TestEveryButtonHasACode(t *testing.T) {
	buttons := []string{ledger.ButtonUndo, ledger.ButtonBan, ledger.ButtonResume, ledger.ButtonAddToBanList,
		ledger.ButtonNo, ledger.ButtonShowAttachment, ledger.ButtonDone}
	seen := map[string]bool{}
	for _, b := range buttons {
		code, ok := telegram.ButtonCodes[b]
		if !ok || seen[code] || strings.Contains(code, ":") {
			t.Fatalf("button %q code %q", b, code)
		}
		seen[code] = true
	}
}

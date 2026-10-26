package telegram_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// humanRemoval has the admin remove the member from G1 by hand and posts the
// "Add them to the ban list?" report it offers.
func humanRemoval(t *testing.T, h *harness) store.Report {
	t.Helper()
	h.k.Clock.Advance(time.Minute)
	h.k.Deliver(&client.GroupChange{Group: modtest.G1, Actor: modtest.Admin, Time: h.k.Clock.Now(),
		Left: []client.JID{modtest.Member}})
	h.drain()
	return h.only(ledger.KindHumanRemoval)
}

// TestAddToBanListBansAndRemovesEverywhere: [Add to ban list] on a human
// removal bans the member in every community the ban reaches and removes them
// from the groups they are still in (set b's group here); a second press is
// answered with the first.
func TestAddToBanListBansAndRemovesEverywhere(t *testing.T) {
	h := newHarness(t, "")
	r := humanRemoval(t, h)
	if h.k.Banned(modtest.MemberM, "") {
		t.Fatal("banned before anyone pressed [Add to ban list]")
	}
	h.press(adminUser, "addban", r.ID)
	if !contains(h.lastReply(), "Banned by Ann", "removed from 1 group(s) and banned", "/unban #") {
		t.Fatalf("reply %q", h.lastReply())
	}
	for _, c := range []string{string(modtest.Community), modtest.SetB} {
		if !h.k.Banned(modtest.MemberM, c) {
			t.Fatalf("not banned in %s", c)
		}
	}
	h.k.Fire()
	if h.k.Fake.Count("Remove "+string(modtest.GB)+" "+string(modtest.Member)) != 1 ||
		h.k.Fake.Count("Remove "+string(modtest.G1)) != 0 {
		t.Fatalf("calls %v, want one removal from set b's group (the admin already removed them from G1)", h.k.Fake.Calls())
	}
	for _, row := range h.k.Rows(modtest.MemberM) {
		if row.Action == store.ActBan && !strings.HasSuffix(row.TriggerID, ":add_to_ban_list") {
			t.Fatalf("ban row trigger %q", row.TriggerID)
		}
	}

	h.press(secondAdm, "addban", r.ID)
	if a := h.answers(); len(a) != 2 || !strings.Contains(a[1], "already done by Ann") {
		t.Fatalf("answers %q", a)
	}
	if h.k.Fake.Count("Remove") != 1 {
		t.Fatalf("a repeat press acted again: %v", h.k.Fake.Calls())
	}
}

// TestNoOnHumanRemovalBansNobody: [No] records who declined and bans no one.
func TestNoOnHumanRemovalBansNobody(t *testing.T) {
	h := newHarness(t, "")
	r := humanRemoval(t, h)
	h.press(adminUser, "no", r.ID)
	if got := h.lastReply(); got != "Not added to the ban list (Ann)." {
		t.Fatalf("reply %q", got)
	}
	if h.k.Banned(modtest.MemberM, "") || len(h.k.Rows(modtest.MemberM)) != 0 {
		t.Fatalf("[No] wrote rows %v", rowsOf(h, modtest.MemberM))
	}
}

// TestAddToBanListOnlyOnHumanRemovals: a report of any other kind cannot be
// acted on with [Add to ban list], even one that carries the button.
func TestAddToBanListOnlyOnHumanRemovals(t *testing.T) {
	h := newHarness(t, "")
	id := h.report(store.Report{Kind: ledger.KindExempt, Community: string(modtest.Community),
		Subject: string(modtest.Member), Text: "an admin matched a rule", Buttons: []string{ledger.ButtonAddToBanList}})
	h.drain()
	h.press(adminUser, "addban", id)
	if !contains(h.lastReply(), "Could not do that: only a human-removal report can be acted on with [Add to ban list]",
		"is a exempt report") {
		t.Fatalf("reply %q", h.lastReply())
	}
	if h.k.Banned(modtest.MemberM, "") {
		t.Fatal("banned from an exempt report")
	}
	// The refused press is released: it is not answered as already done.
	if _, claimed, err := h.k.Store.ClaimPress(h.k.Ctx, store.Press{ReportID: id, Button: ledger.ButtonAddToBanList}); err != nil || !claimed {
		t.Fatalf("the failed press stayed claimed (%v)", err)
	}
}

// TestBanAfterTheEvidenceIsForgotten: [Ban] on a "would have acted" report
// whose evidence copy is gone says to use `groupwarden ban add`, and bans no
// one.
func TestBanAfterTheEvidenceIsForgotten(t *testing.T) {
	h := newHarness(t, "")
	r := watchOnly(t, h)
	if _, err := h.k.Store.ForgetMember(h.k.Ctx, modtest.SpammerM.IDs()); err != nil {
		t.Fatal(err)
	}
	h.press(adminUser, "ban", r.ID)
	if !contains(h.lastReply(), "evidence copy of #", "is gone (purged or forgotten); use `groupwarden ban add`") {
		t.Fatalf("reply %q", h.lastReply())
	}
	h.k.Fire()
	if h.k.Banned(modtest.SpammerM, "") || h.k.Fake.Count("Revoke") != 0 || h.k.Fake.Count("Remove") != 0 {
		t.Fatalf("acted without evidence: calls %v", h.k.Fake.Calls())
	}
}

// rawPress is a press with any callback data, on message msgID of chat.
func (h *harness) rawPress(chat int64, msgID int, user int64, data string) {
	h.t.Helper()
	h.chat.Handle(h.k.Ctx, &models.Update{ID: 3, CallbackQuery: &models.CallbackQuery{ID: "raw",
		From:    models.User{ID: user, FirstName: name(user)},
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: msgID, Chat: models.Chat{ID: chat, Type: "supergroup"}}},
		Data:    data}})
}

// TestButtonDataMustBeKnown: a press whose data is not "<code>:<report
// number>" for a known code is answered "Unknown button." and does nothing.
func TestButtonDataMustBeKnown(t *testing.T) {
	h := newHarness(t, "")
	r := h.spam("K1")
	h.drain()
	msg := h.head(r.ID)
	for _, data := range []string{"", "undo", "undo:", "undo:abc", "undo:1.5", "zap:1", ":1", "UNDO:1"} {
		h.rawPress(telegramtest.ChatID, msg, adminUser, data)
	}
	for i, a := range h.answers() {
		if a != "Unknown button." {
			t.Fatalf("answer %d %q", i, a)
		}
	}
	if len(h.answers()) != 8 || !h.k.Banned(modtest.SpammerM, "") {
		t.Fatalf("answers %q, banned %v", h.answers(), h.k.Banned(modtest.SpammerM, ""))
	}
}

// TestPressOnAPurgedReport: a press naming a report the store no longer
// holds says so.
func TestPressOnAPurgedReport(t *testing.T) {
	h := newHarness(t, "")
	h.rawPress(telegramtest.ChatID, 77, adminUser, "undo:424242")
	if a := h.answers(); len(a) != 1 || a[0] != "There is no report #424242 (it may have been purged)." {
		t.Fatalf("answers %q", a)
	}
}

// TestPressOnAnOldMessage: a button on a message Telegram no longer shows the
// bot in full (older than 48 hours) still works in the admin chat; a press
// that names no message at all is not from the admin chat.
func TestPressOnAnOldMessage(t *testing.T) {
	h := newHarness(t, "")
	r := humanRemoval(t, h)
	press := func(m models.MaybeInaccessibleMessage) {
		h.chat.Handle(h.k.Ctx, &models.Update{ID: 4, CallbackQuery: &models.CallbackQuery{ID: "old",
			From: models.User{ID: adminUser, FirstName: name(adminUser)}, Message: m, Data: "no:" + itoa(r.ID)}})
	}
	press(models.MaybeInaccessibleMessage{})
	if a := h.answers(); len(a) != 1 || !strings.Contains(a[0], "not the groupwarden admin chat") {
		t.Fatalf("a press on no message: answers %q", a)
	}
	press(models.MaybeInaccessibleMessage{InaccessibleMessage: &models.InaccessibleMessage{
		Chat: models.Chat{ID: telegramtest.ChatID, Type: "supergroup"}, MessageID: h.head(r.ID)}})
	if a := h.answers(); len(a) != 2 || a[1] != "Not added to the ban list (Ann)." {
		t.Fatalf("a press on an old message: answers %q", a)
	}
	replies := h.srv.Requests("sendMessage")
	if got := replies[len(replies)-1].Params["reply_parameters"]; !strings.Contains(got, `"message_id":`+itoa(int64(h.head(r.ID)))) {
		t.Fatalf("the reply is not under the report's post: %q", got)
	}
}

// TestChatTalkIsIgnored: messages in the admin chat that are not commands,
// and a message with no sender, get no reply.
func TestChatTalkIsIgnored(t *testing.T) {
	h := newHarness(t, "")
	for _, text := range []string{"", "thanks all", " hello /status", "status"} {
		h.command(telegramtest.ChatID, adminUser, text, 0)
	}
	h.chat.Handle(h.k.Ctx, &models.Update{ID: 5, Message: &models.Message{ID: 9001, Text: "/status",
		Chat: models.Chat{ID: telegramtest.ChatID, Type: "supergroup"}}})
	if n := len(h.srv.Posted()); n != 0 {
		t.Fatalf("%d replies to chat talk", n)
	}
}

// TestBanReplyMustBeToAReport: /ban as a reply to a message the bot did not
// post as a report says so and does nothing.
func TestBanReplyMustBeToAReport(t *testing.T) {
	h := newHarness(t, "")
	h.command(telegramtest.ChatID, adminUser, "/ban", 31337)
	if got := h.lastReply(); got != "That message is not a report I posted." {
		t.Fatalf("reply %q", got)
	}
}

// TestActorNamedWithoutAName: someone with no first or last name is named by
// their @username, or else by their user ID, in the reply and the ledger.
func TestActorNamedWithoutAName(t *testing.T) {
	h := newHarness(t, "")
	h.srv.SetAdmin(601, true)
	h.srv.SetAdmin(602, true)
	r := humanRemoval(t, h)
	h.chat.Handle(h.k.Ctx, &models.Update{ID: 6, CallbackQuery: &models.CallbackQuery{ID: "u",
		From:    models.User{ID: 601, Username: "night_mod"},
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: h.head(r.ID), Chat: models.Chat{ID: telegramtest.ChatID, Type: "supergroup"}}},
		Data:    "no:" + itoa(r.ID)}})
	if got := h.lastReply(); got != "Not added to the ban list (@night_mod)." {
		t.Fatalf("reply %q", got)
	}
	m := &models.Message{ID: 9002, Chat: models.Chat{ID: telegramtest.ChatID, Type: "supergroup"}, Text: "/pause",
		From: &models.User{ID: 602}}
	h.chat.Handle(h.k.Ctx, &models.Update{ID: 7, Message: m})
	if got := h.lastReply(); !strings.HasPrefix(got, "Paused by user 602:") {
		t.Fatalf("reply %q", got)
	}
	pauses, err := h.k.Store.Pauses(h.k.Ctx)
	if err != nil || len(pauses) != 1 || pauses[0].Reason != "paused by user 602 with /pause" {
		t.Fatalf("pauses %+v (%v)", pauses, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestResumeWithNothingPaused: /resume when no pause is in force says so.
func TestResumeWithNothingPaused(t *testing.T) {
	h := newHarness(t, "")
	h.command(telegramtest.ChatID, adminUser, "/resume", 0)
	if got := h.lastReply(); got != "Nothing was paused." {
		t.Fatalf("reply %q", got)
	}
}

// TestUnbanOfAMemberTheBotNeverRemoved: /unban of a report about someone the
// bot banned but never removed (an [Add to ban list] after a human removal,
// before any sweep) lifts the ban and says there is no group to re-invite
// them to.
func TestUnbanOfAMemberTheBotNeverRemoved(t *testing.T) {
	h := newHarness(t, "")
	h.k.Dir.Apply(&client.GroupChange{Group: modtest.GB, Left: []client.JID{modtest.Member}})
	r := humanRemoval(t, h)
	h.press(adminUser, "addban", r.ID)
	if !contains(h.lastReply(), "removed from 0 group(s) and banned") || !h.k.Banned(modtest.MemberM, "") {
		t.Fatalf("[Add to ban list]: %q", h.lastReply())
	}
	h.command(telegramtest.ChatID, adminUser, "/unban #"+itoa(r.ID), 0)
	if !contains(h.lastReply(), "Undone by Ann (#"+itoa(r.ID)+")", "unbanned everywhere",
		"The bot had not removed them from any group.") {
		t.Fatalf("/unban reply %q", h.lastReply())
	}
	if h.k.Banned(modtest.MemberM, "") {
		t.Fatal("still banned after /unban")
	}
}

// TestUnbanUsage: /unban needs a report number, with or without its "#".
func TestUnbanUsage(t *testing.T) {
	h := newHarness(t, "")
	for _, text := range []string{"/unban", "/unban abc", "/unban #", "/unban 1.5"} {
		h.command(telegramtest.ChatID, adminUser, text, 0)
		if got := h.lastReply(); got != "Usage: /unban <report number> (the #number in a report or digest)." {
			t.Errorf("%q: reply %q", text, got)
		}
	}
	h.command(telegramtest.ChatID, adminUser, "/unban #424242", 0)
	if got := h.lastReply(); got != "There is no report #424242 (it may have been purged)." {
		t.Errorf("/unban of a purged report: %q", got)
	}
}

// TestLongPressAnswerClipped: Telegram shows at most 200 characters when it
// answers a press, so a longer result is clipped there with "…" and posted
// in full as a reply under the report.
func TestLongPressAnswerClipped(t *testing.T) {
	h := newHarness(t, "")
	r := h.spam("L1")
	h.drain()
	h.press(adminUser, "undo", r.ID)
	full := h.lastReply()
	if len([]rune(full)) <= 190 || !strings.HasPrefix(full, "Undone by Ann") {
		t.Fatalf("the [Undo] reply is not long enough to clip: %q", full)
	}
	a := h.answers()
	if len(a) != 1 {
		t.Fatalf("answers %q", a)
	}
	got := []rune(a[0])
	if len(got) != 190 || got[189] != '…' || string(got[:189]) != string([]rune(full)[:189]) {
		t.Fatalf("answer %q (%d runes), reply %q", a[0], len(got), full)
	}
}

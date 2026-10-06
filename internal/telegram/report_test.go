package telegram_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/go-telegram/bot/models"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// TestReportFormatMasked: a report names the group, the sender's display name
// and the last 4 digits of their phone number, the rule, the action, the
// config version and the message; no full phone number, LID or group ID.
func TestReportFormatMasked(t *testing.T) {
	h := newHarness(t, "")
	r := h.spam("M1")
	h.drain()
	post := h.srv.Posted()[0]
	text := post.Params["text"]
	want := []string{"general (group…0111)", "Sender: Crypto King (phone…0123)", "Rule: pitch",
		"Action: deleted for everyone; sender removed and banned", "Config: v" + h.k.Holder.Current().Hash,
		"Message:\n" + modtest.SpamText}
	if !contains(text, want...) {
		t.Fatalf("report text %q lacks one of %q", text, want)
	}
	for _, leak := range []string{"447700900123", string(modtest.Spammer), string(modtest.G1), "99999000000999"} {
		if strings.Contains(text, leak) {
			t.Fatalf("report text %q shows %s in full", text, leak)
		}
	}
	if !strings.Contains(post.Params["reply_markup"], `"callback_data":"undo:`+strconv.FormatInt(r.ID, 10)+`"`) {
		t.Fatalf("report buttons %s, want [Undo]", post.Params["reply_markup"])
	}
	if !strings.Contains(post.Params["link_preview_options"], `"is_disabled":true`) {
		t.Fatalf("link previews not disabled: %s", post.Params["link_preview_options"])
	}
	// No phone number known: the last 4 of the LID.
	m := h.k.Msg("M2", modtest.G1, modtest.Member, modtest.SpamText)
	h.k.Deliver(m)
	h.drain()
	posted := h.srv.Posted()
	if text := posted[len(posted)-1].Params["text"]; !strings.Contains(text, "Sender: (no display name) (lid…0666)") {
		t.Fatalf("report for a LID-only sender: %q", text)
	}
}

// TestReportCarriesFullText: every sender-written field goes out in full;
// text longer than one Telegram message continues in follow-ups (replies to
// the report), each within 4096 UTF-16 units and never splitting a
// character; only the report message carries the buttons.
func TestReportCarriesFullText(t *testing.T) {
	h := newHarness(t, "")
	long := strings.Repeat("crypto 🚀 signals ", 600) // 4-byte runes: 2 UTF-16 units each
	m := h.k.Msg("L1", modtest.G1, modtest.Spammer, "join https://example.org/x")
	m.SenderAlt = modtest.SpammerPhone
	m.Fields = append(m.Fields, client.Field{Name: "caption", Text: long, Match: long},
		client.Field{Name: "link.title", Text: "Free crypto", Match: "Free crypto"})
	h.k.Deliver(m)
	h.drain()
	posts := h.srv.Posted()
	if len(posts) < 3 {
		t.Fatalf("%d messages, want the report and at least 2 follow-ups", len(posts))
	}
	var all strings.Builder
	for i, p := range posts {
		text := p.Params["text"]
		if n := len(utf16.Encode([]rune(text))); n > telegram.MaxUnits {
			t.Fatalf("part %d is %d UTF-16 units", i, n)
		}
		if strings.ContainsRune(text, '�') {
			t.Fatalf("part %d split a character", i)
		}
		hasButtons := p.Params["reply_markup"] != ""
		if (i == 0) != hasButtons {
			t.Fatalf("part %d buttons %q", i, p.Params["reply_markup"])
		}
		if i > 0 && !strings.Contains(p.Params["reply_parameters"], `"message_id":`+strconv.Itoa(h.head(h.only(ledger.KindAction).ID))) {
			t.Fatalf("follow-up %d is not a reply to the report: %s", i, p.Params["reply_parameters"])
		}
		all.WriteString(text)
	}
	for _, f := range []string{"body: join https://example.org/x", "caption: " + long, "link.title: Free crypto"} {
		if !strings.Contains(all.String(), f) {
			t.Fatalf("the report lost a field (%.60q…)", f)
		}
	}
}

// TestKeywordOnlyDailySummary: a keyword no rule acts on, and a log-rule
// match, are never reported on their own; one summary per day lists them.
func TestKeywordOnlyDailySummary(t *testing.T) {
	cfg := strings.Replace(modtest.Config, "  lures: [\"inbox me\"]\n", "  lures: [\"inbox me\"]\n  stocks: [\"nasdaq\"]\n", 1)
	cfg = strings.Replace(cfg, "rules:\n  list:\n", "rules:\n  list:\n    - name: stocktalk\n      action: log\n      when:\n        words: stocks\n", 1)
	h := newHarnessKit(t, modtest.NewConfig(t, cfg))
	h.drain() // the first run starts the summaries with today
	h.k.Deliver(h.k.Msg("K1", modtest.G1, modtest.Member, "anyone into crypto?"))
	h.k.Deliver(h.k.Msg("K2", modtest.G2, modtest.Member, "crypto again"))
	h.k.Deliver(h.k.Msg("S1", modtest.G1, modtest.Member, "nasdaq today"))
	h.drain()
	if n := len(h.srv.Posted()); n != 0 {
		t.Fatalf("%d messages before the day ended, want none", n)
	}
	h.k.Clock.Advance(24 * time.Hour)
	h.drain()
	posted := h.srv.Posted()
	if len(posted) != 1 {
		t.Fatalf("%d messages after the day ended, want one summary", len(posted))
	}
	text := posted[0].Params["text"]
	if !contains(text, "Daily summary for 2026-10-06", "Keyword-only posts not acted on in community a: crypto 2",
		"Rule stocktalk matched a post in community a (report only). (1)") {
		t.Fatalf("summary %q", text)
	}
	if rest, _ := h.k.Store.UnsentReportsOf(h.k.Ctx, ledger.KindLog, 10); len(rest) != 0 {
		t.Fatalf("%d log reports left unsent", len(rest))
	}
	h.k.Clock.Advance(time.Hour)
	h.drain()
	if n := len(h.srv.Posted()); n != 1 {
		t.Fatalf("%d messages, want still one summary", n)
	}
}

// TestOutboxPersistedAcrossRestart: a report Telegram never took, and one
// cut off part-way, go out after a restart, each part exactly once.
func TestOutboxPersistedAcrossRestart(t *testing.T) {
	h := newHarness(t, "")
	h.srv.FailAlways("sendMessage", telegramtest.Reply{Code: 500, Description: "Internal Server Error"})
	first := h.report(store.Report{Kind: "would_remove", Text: "first", Buttons: []string{ledger.ButtonUndo}})
	if _, err := h.chat.Step(h.k.Ctx); err == nil {
		t.Fatal("a failed send reported no error")
	}
	h.srv.Clear()
	long := strings.Repeat("crypto https://example.org/x ", 400)
	m := h.k.Msg("P1", modtest.G1, modtest.Spammer, long)
	m.SenderAlt = modtest.SpammerPhone
	h.k.Deliver(m)
	h.drain() // "first" goes out now
	h.k.Deliver(h.k.Msg("P2", modtest.G1, modtest.Other1, long))
	h.srv.Fail("sendMessage", telegramtest.Reply{})                                         // the report message goes
	h.srv.Fail("sendMessage", telegramtest.Reply{Code: 500, Description: "Internal Error"}) // its follow-up fails
	if _, err := h.chat.Step(h.k.Ctx); err == nil {
		t.Fatal("the cut-off report reported no error")
	}
	h.restart()
	h.drain()
	reps := h.k.Reports(ledger.KindAction)
	cut := reps[len(reps)-1]
	var heads, follow int
	for _, msg := range h.messages(cut.ID) {
		switch msg.Role {
		case store.RoleReport:
			heads++
		case store.RoleFollowup:
			follow++
		}
	}
	if heads != 1 || follow < 1 {
		t.Fatalf("cut-off report posted %d report message(s) and %d follow-up(s), want 1 and ≥1", heads, follow)
	}
	if got := len(h.messages(first)); got != 1 {
		t.Fatalf("the first report was posted %d times", got)
	}
	if left, _ := h.k.Store.UnsentReports(h.k.Ctx, 10); len(left) != 0 {
		t.Fatalf("%d reports never sent", len(left))
	}
}

// TestOutboxUnderGroupLimit: in any minute at most 20 messages go to the
// group, and routine ones leave 5 of them free for priority alerts.
func TestOutboxUnderGroupLimit(t *testing.T) {
	h := newHarness(t, "")
	routine := map[string]bool{}
	for i := 0; i < 45; i++ {
		text := "routine " + strconv.Itoa(i)
		routine[text] = true
		h.report(store.Report{Kind: ledger.KindBannedRejoin, Text: text, Buttons: []string{ledger.ButtonUndo}})
	}
	for i := 0; i < 15; i++ {
		h.report(store.Report{Kind: "admin_spared", Priority: true, Text: "priority " + strconv.Itoa(i)})
	}
	h.drain()
	posts := h.srv.Posted()
	if len(posts) != 60 {
		t.Fatalf("%d posts, want 60", len(posts))
	}
	for i, p := range posts {
		all, plain := 0, 0
		for _, q := range posts[i:] {
			if q.At.Sub(p.At) < time.Minute {
				all++
				if routine[q.Params["text"]] {
					plain++
				}
			}
		}
		if all > 20 || plain > 15 {
			t.Fatalf("from post %d: %d messages (%d routine) within a minute, want ≤ 20 (≤ 15 routine)", i, all, plain)
		}
	}
}

// TestRetryAfterHonoured: after "429, retry after N", nothing is sent for N
// seconds.
func TestRetryAfterHonoured(t *testing.T) {
	h := newHarness(t, "")
	h.report(store.Report{Kind: "would_remove", Text: "x", Buttons: []string{ledger.ButtonUndo}})
	h.srv.Fail("sendMessage", telegramtest.Reply{Code: 429, Description: "Too Many Requests: retry after 17", RetryAfter: 17})
	if _, err := h.chat.Step(h.k.Ctx); err == nil {
		t.Fatal("429 reported no error")
	}
	h.drain()
	sends := h.srv.Requests("sendMessage")
	if len(sends) != 2 {
		t.Fatalf("%d sends, want the refused one and its retry", len(sends))
	}
	if gap := sends[1].At.Sub(sends[0].At); gap < 17*time.Second {
		t.Fatalf("retried after %s, want ≥ 17s", gap)
	}
}

// TestDigestKeepsRemoveBanReports: with a backlog, reports without buttons
// go out combined, but every remove/ban report and every report with a
// button still goes out alone with its buttons.
func TestDigestKeepsRemoveBanReports(t *testing.T) {
	h := newHarness(t, "")
	var kept []int64
	for i := 0; i < 15; i++ {
		h.report(store.Report{Kind: "would_remove", Text: "info " + strconv.Itoa(i)})
	}
	kept = append(kept, h.spam("A1").ID)
	kept = append(kept, h.report(store.Report{Kind: ledger.KindWouldHaveActed, Text: "watch", Buttons: []string{ledger.ButtonBan}}))
	kept = append(kept, h.report(store.Report{Kind: ledger.KindBanCLI, Text: "ban added by the ban command"}))
	kept = append(kept, h.report(store.Report{Kind: ledger.KindBannedRejoin, Text: "rejoin", Buttons: []string{ledger.ButtonUndo}}))
	h.drain()
	digests := 0
	for _, p := range h.srv.Posted() {
		if strings.Contains(p.Params["text"], "reports while the chat was busy") {
			digests++
			for _, id := range kept {
				if strings.Contains(p.Params["text"], "#"+strconv.FormatInt(id, 10)+" ") {
					t.Fatalf("report %d was digested: %q", id, p.Params["text"])
				}
			}
		}
	}
	if digests == 0 {
		t.Fatal("a backlog of 15 plain reports was not digested")
	}
	for _, id := range kept {
		if h.head(id) == 0 {
			t.Fatalf("report %d was not posted on its own", id)
		}
	}
	for _, p := range h.srv.Posted() {
		if strings.Contains(p.Params["text"], "watch") && !strings.Contains(p.Params["reply_markup"], `"ban:`) {
			t.Fatalf("the [Ban] report lost its button: %s", p.Params["reply_markup"])
		}
	}
}

// TestPrioritySetJumpsQueue: every member of the priority set (whatever its
// sender said) and every priority report goes ahead of a routine backlog; a
// routine alert does not.
func TestPrioritySetJumpsQueue(t *testing.T) {
	set := []alert.Kind{alert.FatalDisconnect, alert.TemporaryBan, alert.Breaker, alert.Paused, alert.Deafness,
		alert.ProlongedDisconnect, alert.BotDemoted, alert.BotRemoved, alert.CoverageLost, alert.ExtraCompanion,
		alert.ConfigRejected, alert.SyncFailed, alert.Overdue, alert.DecryptError}
	for _, kind := range set {
		h := newHarness(t, "")
		for i := 0; i < 5; i++ {
			h.report(store.Report{Kind: ledger.KindBannedRejoin, Text: "backlog", Buttons: []string{ledger.ButtonUndo}})
		}
		ctx, cancel := context.WithTimeout(h.k.Ctx, 5*time.Second)
		if err := h.chat.Alert(ctx, alert.Alert{Kind: kind, Text: "alert " + string(kind)}); err != nil {
			t.Fatal(err)
		}
		cancel()
		if kind != alert.FatalDisconnect { // a fatal alert is flushed by Alert itself
			if _, err := h.chat.Step(h.k.Ctx); err != nil {
				t.Fatal(err)
			}
		}
		if got := h.srv.Posted()[0].Params["text"]; got != "alert "+string(kind) {
			t.Fatalf("%s: first post %q, want the alert", kind, got)
		}
	}
	h := newHarness(t, "")
	h.report(store.Report{Kind: ledger.KindBannedRejoin, Text: "backlog", Buttons: []string{ledger.ButtonUndo}})
	h.report(store.Report{Kind: ledger.KindAdminSpared, Priority: true, Text: "spared"})
	if err := h.chat.Alert(h.k.Ctx, alert.Alert{Kind: alert.ConfigLoaded, Text: "loaded"}); err != nil {
		t.Fatal(err)
	}
	h.drain()
	var order []string
	for _, p := range h.srv.Posted() {
		order = append(order, p.Params["text"])
	}
	if strings.Join(order, "|") != "spared|backlog|loaded" {
		t.Fatalf("order %q, want the priority report first and the routine alert in turn", order)
	}
}

// TestFatalAlertFlushedBeforeExit: a fatal alert is delivered before Alert
// returns, ahead of the routine backlog (which waits for the restart); when
// Telegram cannot take it, Alert gives up at its deadline.
func TestFatalAlertFlushedBeforeExit(t *testing.T) {
	h := newHarness(t, "")
	for i := 0; i < 30; i++ {
		h.report(store.Report{Kind: ledger.KindBannedRejoin, Text: "backlog", Buttons: []string{ledger.ButtonUndo}})
	}
	ctx, cancel := context.WithTimeout(h.k.Ctx, 5*time.Second)
	defer cancel()
	if err := h.chat.Alert(ctx, alert.Alert{Kind: alert.FatalDisconnect, Text: "logged out: re-pair"}); err != nil {
		t.Fatal(err)
	}
	posted := h.srv.Posted()
	if len(posted) != 1 || posted[0].Params["text"] != "logged out: re-pair" {
		t.Fatalf("posted %d messages before Alert returned, want just the fatal alert", len(posted))
	}
	h.srv.FailAlways("sendMessage", telegramtest.Reply{Code: 502, Description: "Bad Gateway"})
	ctx2, cancel2 := context.WithTimeout(h.k.Ctx, 2*time.Second)
	defer cancel2()
	start := time.Now()
	if err := h.chat.Alert(ctx2, alert.Alert{Kind: alert.Stopping, Priority: true, Text: "stopping"}); err == nil {
		t.Fatal("an undeliverable stopping alert reported success")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Alert took %s, past its deadline", took)
	}
}

// TestFollowsChatMigration: when the group becomes a supergroup, the bot
// sends to the new ID, remembers it across a restart, and follows the
// migration message too.
func TestFollowsChatMigration(t *testing.T) {
	h := newHarness(t, "")
	const moved int64 = -1009999000077
	h.report(store.Report{Kind: "would_remove", Text: "x"})
	h.srv.Fail("sendMessage", telegramtest.Reply{Code: 400, Description: "Bad Request: group chat was upgraded to a supergroup chat",
		MigrateTo: moved})
	h.drain()
	sends := h.srv.Requests("sendMessage")
	if len(sends) != 2 || sends[1].Params["chat_id"] != strconv.FormatInt(moved, 10) {
		t.Fatalf("sends %+v, want a retry to the new chat", sends)
	}
	h.restart()
	if err := runOnce(h); err != nil {
		t.Fatal(err)
	}
	if h.chat.ChatID() != moved {
		t.Fatalf("after a restart the chat is %d, want %d", h.chat.ChatID(), moved)
	}
	// The migration message itself, seen in the chat the bot is in.
	const again int64 = -1009999000078
	h.chat.Handle(h.k.Ctx, migrationUpdate(moved, again))
	if h.chat.ChatID() != again {
		t.Fatalf("after the migration message the chat is %d, want %d", h.chat.ChatID(), again)
	}
}

// TestTelegramForbiddenPausesBans: Telegram refusing the bot for 10 minutes
// pauses removals and bans (deletes continue) and marks it unhealthy; the
// first message through lifts both.
func TestTelegramForbiddenPausesBans(t *testing.T) {
	h := newHarness(t, "")
	h.srv.FailAlways("sendMessage", telegramtest.Reply{Code: 403, Description: "Forbidden: bot was kicked from the supergroup chat"})
	h.report(store.Report{Kind: "would_remove", Text: "x"})
	for i := 0; i < 3; i++ {
		_, _ = h.chat.Step(h.k.Ctx)
		h.k.Clock.Advance(5 * time.Minute)
	}
	if paused, _ := h.k.Store.PausedFor(h.k.Ctx, store.ScopeRemoveBan); !paused {
		t.Fatal("removals and bans not paused after 10 minutes of 403")
	}
	if paused, _ := h.k.Store.PausedFor(h.k.Ctx, store.ScopeAll); paused {
		t.Fatal("deletes paused too")
	}
	st, _ := h.k.Store.Status(h.k.Ctx)
	if st[store.StatusTelegramOK].Value != "0" {
		t.Fatalf("tg_ok %q, want 0", st[store.StatusTelegramOK].Value)
	}
	h.k.Deliver(h.k.Spam("F1", modtest.G1))
	h.k.Fire()
	if h.k.Fake.Count("Revoke") != 1 || h.k.Fake.Count("Remove") != 0 {
		t.Fatalf("calls %v, want the delete only", h.k.Fake.Calls())
	}
	h.srv.Clear()
	h.drain()
	if paused, _ := h.k.Store.PausedFor(h.k.Ctx, store.ScopeRemoveBan); paused {
		t.Fatal("still paused after Telegram took a message")
	}
	st, _ = h.k.Store.Status(h.k.Ctx)
	if st[store.StatusTelegramOK].Value != "1" {
		t.Fatalf("tg_ok %q, want 1", st[store.StatusTelegramOK].Value)
	}
	h.k.Fire()
	if h.k.Fake.Count("Remove") == 0 {
		t.Fatal("removals did not resume")
	}
}

// TestFullTextStrippedAfterEvidenceWindow: after retention.evidence_days the
// bot edits its report and follow-ups to drop the member's text, keeping the
// report's buttons; not a day before.
func TestFullTextStrippedAfterEvidenceWindow(t *testing.T) {
	h := newHarness(t, "")
	m := h.k.Msg("E1", modtest.G1, modtest.Spammer, strings.Repeat("crypto https://example.org/x ", 300))
	m.SenderAlt = modtest.SpammerPhone
	h.k.Deliver(m)
	h.drain()
	r := h.only(ledger.KindAction)
	parts := len(h.messages(r.ID))
	h.k.Clock.Advance(29 * 24 * time.Hour)
	h.drain()
	if n := len(h.srv.Requests("editMessageText")); n != 0 {
		t.Fatalf("%d edits before the window ended", n)
	}
	h.k.Clock.Advance(2 * 24 * time.Hour)
	h.drain()
	edits := h.srv.Requests("editMessageText")
	if len(edits) != parts {
		t.Fatalf("%d edits, want one per posted part (%d)", len(edits), parts)
	}
	for i, e := range edits {
		text := e.Params["text"]
		if strings.Contains(text, "example.org") {
			t.Fatalf("edit %d still carries the member's text", i)
		}
		if i == 0 && (!strings.HasSuffix(text, "Message: (message text removed)") || !strings.Contains(e.Params["reply_markup"], "undo:")) {
			t.Fatalf("report edit %q markup %q, want the header, the note and [Undo]", text, e.Params["reply_markup"])
		}
		if i > 0 && text != "(message text removed)" {
			t.Fatalf("follow-up edit %q", text)
		}
	}
	h.drain()
	if n := len(h.srv.Requests("editMessageText")); n != parts {
		t.Fatalf("%d edits after another pass, want %d (each once)", n, parts)
	}
}

// image adds a saved-able image attachment to m.
func image(m *client.Message) *client.Message {
	m.Media = &client.Media{Kind: "image", MimeType: "image/jpeg", FileName: "offer.jpg", Size: 7, Raw: []byte("raw")}
	return m
}

// attachmentReport delivers spam with an image, saves the image and posts
// the report and the attachment.
func attachmentReport(t *testing.T, h *harness) store.Report {
	t.Helper()
	h.k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
		return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
	}
	m := h.k.Spam("I1", modtest.G1)
	h.k.Deliver(image(m))
	h.k.Fire()
	if n, err := h.k.Media.Fetch(h.k.Ctx); err != nil || n != 1 {
		t.Fatalf("fetched %d (%v)", n, err)
	}
	h.drain()
	r := h.only(ledger.KindAction)
	docs := h.srv.Requests("sendDocument")
	if len(docs) != 1 || string(docs[0].Files["document"]) != "JPEGDATA" ||
		!strings.Contains(docs[0].Params["reply_parameters"], `"message_id":`+strconv.Itoa(h.head(r.ID))) {
		t.Fatalf("attachment posts %+v, want the file as a reply to the report", docs)
	}
	return r
}

// TestAttachmentPostDeletedAfterShowWindow: the saved attachment is posted
// as a reply and deleted after report.attachment_show_hours.
func TestAttachmentPostDeletedAfterShowWindow(t *testing.T) {
	h := newHarness(t, "")
	r := attachmentReport(t, h)
	h.k.Clock.Advance(23 * time.Hour)
	h.drain()
	if n := len(h.srv.Requests("deleteMessage")); n != 0 {
		t.Fatalf("%d deletes before 24 hours", n)
	}
	h.k.Clock.Advance(2 * time.Hour)
	h.drain()
	dels := h.srv.Requests("deleteMessage")
	var att store.TGMessage
	for _, m := range h.messages(r.ID) {
		if m.Role == store.RoleAttachment {
			att = m
		}
	}
	if len(dels) != 1 || dels[0].Params["message_id"] != strconv.Itoa(att.MessageID) || att.GoneAt.IsZero() {
		t.Fatalf("deletes %+v (attachment %+v), want the attachment post deleted once", dels, att)
	}
}

// TestAttachmentDeleteFailureEditedToPlaceholder: when Telegram will not
// delete the post (over 48 hours), it is replaced by a placeholder file.
func TestAttachmentDeleteFailureEditedToPlaceholder(t *testing.T) {
	h := newHarness(t, "")
	attachmentReport(t, h)
	h.srv.Fail("deleteMessage", telegramtest.Reply{Code: 400, Description: "Bad Request: message can't be deleted for everyone"})
	h.k.Clock.Advance(25 * time.Hour)
	h.drain()
	edits := h.srv.Requests("editMessageMedia")
	if len(edits) != 1 || !strings.Contains(edits[0].Params["media"], "attach://attachment-removed.txt") ||
		!strings.Contains(string(edits[0].Files["attachment-removed.txt"]), "taken down after 24 hours") {
		t.Fatalf("placeholder edits %+v", edits)
	}
	h.drain()
	if n := len(h.srv.Requests("deleteMessage")); n != 1 {
		t.Fatalf("%d delete attempts, want 1", n)
	}
}

// TestShowAttachmentUntilEvidencePurged: [Show attachment] posts the file
// again (any number of times) until the evidence copy is purged.
func TestShowAttachmentUntilEvidencePurged(t *testing.T) {
	h := newHarness(t, "")
	r := attachmentReport(t, h)
	if !strings.Contains(strings.Join(r.Buttons, ","), ledger.ButtonShowAttachment) {
		t.Fatalf("report buttons %v lack [Show attachment]", r.Buttons)
	}
	h.press(adminUser, "show", r.ID)
	h.press(secondAdm, "show", r.ID)
	if n := len(h.srv.Requests("sendDocument")); n != 3 {
		t.Fatalf("%d attachment posts, want the first and two more", n)
	}
	// An evidence copy whose file is not saved (here: the download failed)
	// is not posted, and the reply says why.
	full, ok, err := h.k.Store.Report(h.k.Ctx, r.ID) // the kit's listing does not read evidence_id
	if err != nil || !ok || full.EvidenceID == 0 {
		t.Fatalf("report %d: %+v %v %v", r.ID, full, ok, err)
	}
	if err := h.k.Store.SetMedia(h.k.Ctx, full.EvidenceID, store.MediaFailed, "", "download failed"); err != nil {
		t.Fatal(err)
	}
	h.press(adminUser, "show", r.ID)
	if got := h.lastReply(); got != "The attachment is no longer kept (the evidence copy was purged, forgotten or never saved)." {
		t.Fatalf("reply for an attachment not saved %q", got)
	}
	if _, _, err := h.k.Store.PurgeEvidence(h.k.Ctx, h.k.Clock.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.press(adminUser, "show", r.ID)
	if n := len(h.srv.Requests("sendDocument")); n != 3 {
		t.Fatalf("%d attachment posts after the purge, want still 3", n)
	}
	if got := h.lastReply(); !strings.Contains(got, "no longer kept") {
		t.Fatalf("reply after the purge %q", got)
	}
}

// TestTokenRedactedInErrors: the bot token never reaches an error or a log
// line, even when an answer quotes the request URL.
func TestTokenRedactedInErrors(t *testing.T) {
	h := newHarness(t, "")
	h.report(store.Report{Kind: "would_remove", Text: "x"})
	h.srv.Fail("sendMessage", telegramtest.Reply{EchoPath: true})
	_, err := h.chat.Step(h.k.Ctx)
	if err == nil || strings.Contains(err.Error(), h.srv.Token) || !strings.Contains(err.Error(), "<token>") {
		t.Fatalf("error %v: want it, with the token replaced by <token>", err)
	}
	r := h.only("would_remove")
	h.drain()
	h.srv.Fail("getChatMember", telegramtest.Reply{EchoPath: true})
	h.press(adminUser, "undo", r.ID)
	if strings.Contains(h.logs.String(), h.srv.Token) {
		t.Fatal("the token is in the logs")
	}
	if !strings.Contains(h.logs.String(), "<token>") {
		t.Fatalf("the admin check's failure was not logged (redacted): %s", h.logs.String())
	}
}

// runOnce runs the chat for one delivery round (Run restores a recorded
// migration first).
func runOnce(h *harness) error {
	ctx, cancel := context.WithCancel(h.k.Ctx)
	done := make(chan struct{})
	go func() {
		h.chat.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for h.chat.ChatID() == telegramtest.ChatID && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	return nil
}

// migrationUpdate is the service message a group sends when it becomes a
// supergroup.
func migrationUpdate(from, to int64) *models.Update {
	raw, _ := json.Marshal(map[string]any{"update_id": 9, "message": map[string]any{"message_id": 1, "date": 1,
		"chat": map[string]any{"id": from, "type": "group"}, "migrate_to_chat_id": to}})
	var u models.Update
	_ = json.Unmarshal(raw, &u)
	return &u
}

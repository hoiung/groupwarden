package telegram_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
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

// TestDigestOfTooLongReport: in a backlog holding reports too long to share
// a message, a report that cannot share a digest with another goes on its
// own (a long one in parts), the rest are digested, and each digest counts
// what it holds, never fewer than two.
func TestDigestOfTooLongReport(t *testing.T) {
	h := newHarness(t, "")
	longText := strings.Repeat("general (group…0111): checked\n", 200)
	short := func(s string) int64 { return h.report(store.Report{Kind: "would_remove", Text: "info " + s}) }
	alone := short("alone") // only it fits before the first long report
	long := h.report(store.Report{Kind: "would_remove", Text: longText})
	short("a") // these two fit before the second long one
	short("b")
	long2 := h.report(store.Report{Kind: "would_remove", Text: longText})
	for i := 0; i < 15; i++ {
		short(strconv.Itoa(i))
	}
	h.drain()
	for _, id := range []int64{alone, long, long2} {
		if h.head(id) == 0 {
			t.Fatalf("report %d was not posted on its own", id)
		}
	}
	digests := 0
	for _, p := range h.srv.Posted() {
		text := p.Params["text"]
		if n, rest, ok := strings.Cut(text, " reports while the chat was busy:"); ok {
			digests++
			holds := strings.Count(rest, "\n\n#")
			if want := strconv.Itoa(holds); n != want || holds < 2 {
				t.Fatalf("digest says %s reports and holds %d", n, holds)
			}
			for _, id := range []int64{alone, long, long2} {
				if strings.Contains(rest, "#"+strconv.FormatInt(id, 10)+" ") {
					t.Fatalf("report %d went in a digest", id)
				}
			}
		}
	}
	if digests < 2 {
		t.Fatalf("%d digests, want the two short ones together and the last 15", digests)
	}
}

// TestTextRemovalUsesTheCommunityWindow: a community's own
// retention.evidence_days decides when the member's text leaves its reports.
func TestTextRemovalUsesTheCommunityWindow(t *testing.T) {
	cfg := strings.Replace(modtest.Config, "    name: community a\n",
		"    name: community a\n    retention:\n      evidence_days: 7\n", 1)
	h := newHarnessKit(t, modtest.NewConfig(t, cfg))
	h.k.Deliver(h.k.Spam("W1", modtest.G1))
	h.drain()
	h.k.Clock.Advance(6 * 24 * time.Hour)
	h.drain()
	if n := len(h.srv.Requests("editMessageText")); n != 0 {
		t.Fatalf("%d edits before the community's 7 days", n)
	}
	h.k.Clock.Advance(2 * 24 * time.Hour)
	h.drain()
	if len(h.srv.Requests("editMessageText")) == 0 {
		t.Fatal("the member's text was not removed after the community's 7 days (the default is 30)")
	}
}

// TestLongFileNameClippedInTheHeader: a file name too long for a message is
// clipped in the report's header, so the header still goes whole in the
// first message (the name is in full under "Message:").
func TestLongFileNameClippedInTheHeader(t *testing.T) {
	h := newHarness(t, "")
	m := h.k.Spam("N1", modtest.G1)
	m.Media = &client.Media{Kind: "document", MimeType: "application/pdf",
		FileName: strings.Repeat("Earn-5000-weekly-", 300) + ".pdf", Size: 7, Raw: []byte("raw")}
	h.k.Deliver(m)
	h.drain()
	first := h.srv.Posted()[0].Params["text"]
	line := ""
	for _, l := range strings.Split(first, "\n") {
		if strings.HasPrefix(l, "Attachment: ") {
			line = l
		}
	}
	if !strings.Contains(first, "\nMessage:") || line == "" || len(line) > 300 || !strings.Contains(line, "… (") {
		t.Fatalf("the header did not go whole with the file name clipped; attachment line %.120q…", line)
	}
}

// TestRoutineWaitFreesTheSlot: a routine report waiting out the routine
// budget does not hold the delivery slot, so a fatal alert posts at once.
func TestRoutineWaitFreesTheSlot(t *testing.T) {
	h := newHarness(t, "")
	for i := 0; i < 16; i++ {
		h.report(store.Report{Kind: ledger.KindBannedRejoin, Text: "routine " + strconv.Itoa(i),
			Buttons: []string{ledger.ButtonUndo}})
	}
	for i := 0; i < 15; i++ { // the routine budget for this minute
		if _, err := h.chat.Step(h.k.Ctx); err != nil {
			t.Fatal(err)
		}
	}
	waiting, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.k.Clock.OnSleep = func(d time.Duration) {
		if d > 5*time.Second { // the routine budget, not the 3-second shared bucket
			once.Do(func() { close(waiting) })
			<-release
		}
	}
	done := make(chan error, 1)
	go func() { _, err := h.chat.Step(h.k.Ctx); done <- err }()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatalf("the 16th routine report did not wait for the routine budget (%d posts, slept %v)",
			len(h.srv.Posted()), h.k.Clock.Slept())
	}
	ctx, cancel := context.WithTimeout(h.k.Ctx, 2*time.Second)
	defer cancel()
	err := h.chat.Alert(ctx, alert.Alert{Kind: alert.FatalDisconnect, Text: "logged out: re-pair"})
	close(release)
	if err != nil {
		t.Fatalf("the fatal alert waited behind a routine sleep: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	posted := h.srv.Posted()
	if len(posted) != 17 || posted[15].Params["text"] != "logged out: re-pair" ||
		!strings.Contains(posted[16].Params["text"], "routine 15") {
		t.Fatalf("%d posts; want the alert then the waiting routine report last", len(posted))
	}
}

// TestWhitespacePaddingKeepsTheTail: a member padding a post with blank lines
// cannot make a part Telegram refuses as empty, so the end of their message
// still reaches the admins.
func TestWhitespacePaddingKeepsTheTail(t *testing.T) {
	text := "hello crypto https://example.org/x" + strings.Repeat("\n", 9000) + "the hidden part"
	parts, starts := telegram.Split(text, "")
	for i, p := range parts {
		if strings.TrimSpace(p) != p || p == "" {
			t.Fatalf("part %d is not trimmed or is empty: %q…", i, p[:min(len(p), 20)])
		}
		if got := string(utf16.Decode(utf16.Encode([]rune(text))[starts[i]:][:len(utf16.Encode([]rune(p)))])); got != p {
			t.Fatalf("part %d does not start where split says", i)
		}
	}
	h := newHarness(t, "")
	h.k.Deliver(h.k.Msg("W1", modtest.G1, modtest.Spammer, text))
	h.drain()
	var tail bool
	for _, p := range h.srv.Posted() {
		if p.MessageID == 0 {
			t.Fatalf("a part was refused: %q", p.Params["text"])
		}
		tail = tail || strings.Contains(p.Params["text"], "the hidden part")
	}
	if !tail {
		t.Fatal("the end of the message was never posted")
	}
}

// TestRefusedFollowupSkipped: Telegram refusing one follow-up part does not
// lose the parts after it.
func TestRefusedFollowupSkipped(t *testing.T) {
	h := newHarness(t, "")
	text := "crypto https://example.org/x " + strings.Repeat("filler line\n", 900) + "THE END"
	h.srv.Fail("sendMessage", telegramtest.Reply{})
	h.srv.Fail("sendMessage", telegramtest.Reply{Code: 400, Description: "Bad Request: something odd"})
	h.k.Deliver(h.k.Msg("F1", modtest.G1, modtest.Spammer, text))
	h.drain()
	posts := h.srv.Posted()
	if len(posts) < 3 || posts[1].MessageID != 0 || !strings.Contains(posts[len(posts)-1].Params["text"], "THE END") {
		t.Fatalf("%d posts; want the report, the refused part, then the rest through THE END", len(posts))
	}
	if r, ok, err := h.k.Store.Report(h.k.Ctx, h.only(ledger.KindAction).ID); err != nil || !ok || r.SentAt.IsZero() {
		t.Fatalf("report not marked sent (%v)", err)
	}
}

// TestRefusedAttachmentDoesNotBlock: an attachment Telegram will never take
// (an empty file: 400; one over its limit: 413) is marked failed with its
// file kept for the purge, and the text removal behind it still happens.
func TestRefusedAttachmentDoesNotBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		fail *telegramtest.Reply
	}{
		{"empty file", []byte{}, nil},
		{"over the limit", []byte("JPEGDATA"), &telegramtest.Reply{Code: 413, Description: "Request Entity Too Large"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "")
			h.k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
				return tc.data, "image/jpeg", "offer.jpg", nil
			}
			if tc.fail != nil {
				h.srv.FailAlways("sendDocument", *tc.fail)
			}
			h.k.Deliver(image(h.k.Spam("I1", modtest.G1)))
			h.k.Fire()
			if _, err := h.k.Media.Fetch(h.k.Ctx); err != nil {
				t.Fatal(err)
			}
			h.drain()
			r, _, err := h.k.Store.Report(h.k.Ctx, h.only(ledger.KindAction).ID)
			if err != nil {
				t.Fatal(err)
			}
			ev, ok, err := h.k.Store.Evidence(h.k.Ctx, r.EvidenceID)
			if err != nil || !ok || ev.MediaState != store.MediaFailed || ev.MediaPath == "" ||
				!strings.Contains(ev.MediaError, "refused") {
				t.Fatalf("evidence %+v (%v), want failed with its file kept", ev, err)
			}
			if n := len(h.srv.Requests("sendDocument")); n != 1 {
				t.Fatalf("%d sendDocument calls, want 1", n)
			}
			if n := saidNotPosted(h, r.ID, "refused by Telegram"); n != 1 {
				t.Fatalf("%d replies under the report saying the attachment was refused, want 1", n)
			}
			h.press(adminUser, "show", r.ID)
			if got := h.lastReply(); !strings.Contains(got, "could not be posted: Telegram refused the file") {
				t.Fatalf("[Show attachment] answered %q, want why it could not be posted", got)
			}
			h.k.Clock.Advance(31 * 24 * time.Hour)
			h.drain()
			if len(h.srv.Requests("editMessageText")) == 0 {
				t.Fatal("the member's text was not removed after the evidence window")
			}
		})
	}
}

// TestStrippedHeaderKeepsNothingTheSenderWrote: after `member forget` the
// report keeps neither the sender's display name nor the attachment's file
// name, in the chat or in the database.
func TestStrippedHeaderKeepsNothingTheSenderWrote(t *testing.T) {
	h := newHarness(t, "")
	m := h.k.Spam("D1", modtest.G1)
	m.SenderAlt = modtest.SpammerPhone
	m.PushName = "Crypto King Mike"
	m.Media = &client.Media{Kind: "document", MimeType: "application/pdf", FileName: "Earn-5000-weekly-DM-me.pdf",
		Size: 7, Raw: []byte("raw")}
	h.k.Deliver(m)
	h.drain()
	r := h.only(ledger.KindAction)
	if first := h.srv.Posted()[0].Params["text"]; !contains(first, "Crypto King Mike", "Earn-5000-weekly-DM-me.pdf") {
		t.Fatalf("the report lost the display name or file name: %q", first)
	}
	if _, err := h.k.Store.ForgetMember(h.k.Ctx, []string{string(modtest.Spammer), string(modtest.SpammerPhone)}); err != nil {
		t.Fatal(err)
	}
	h.drain()
	edits := h.srv.Requests("editMessageText")
	if len(edits) == 0 || !strings.Contains(edits[0].Params["text"], "Sender: (display name removed) (") {
		t.Fatalf("edits %d, first %q", len(edits), edits[0].Params["text"])
	}
	for _, e := range edits {
		if strings.Contains(e.Params["text"], "Crypto King") || strings.Contains(e.Params["text"], "Earn-5000") {
			t.Fatalf("the stripped report still holds what the sender wrote: %q", e.Params["text"])
		}
	}
	for _, msg := range h.messages(r.ID) {
		if msg.Stripped != "" {
			t.Fatalf("message %d keeps a stripped copy after the strip: %q", msg.MessageID, msg.Stripped)
		}
	}
}

// TestMemberTextNotLinked: the member's text goes as a pre entity covering
// exactly that text, so a "/resume" or a link in it is not one tap away.
func TestMemberTextNotLinked(t *testing.T) {
	h := newHarness(t, "")
	text := "crypto https://example.org/x 🚀 tap /resume to fix " + strings.Repeat("more text ", 500) + "/pause"
	h.k.Deliver(h.k.Msg("Q1", modtest.G1, modtest.Spammer, text))
	h.drain()
	var quoted strings.Builder
	for i, p := range h.srv.Posted() {
		var ents []struct {
			Type           string
			Offset, Length int
		}
		if err := json.Unmarshal([]byte(p.Params["entities"]), &ents); err != nil || len(ents) != 1 || ents[0].Type != "pre" {
			t.Fatalf("part %d entities %q (%v), want one pre", i, p.Params["entities"], err)
		}
		u := utf16.Encode([]rune(p.Params["text"]))
		quoted.WriteString(string(utf16.Decode(u[ents[0].Offset : ents[0].Offset+ents[0].Length])))
		if i == 0 && strings.Contains(string(utf16.Decode(u[ents[0].Offset:])), "Group:") {
			t.Fatal("the pre entity covers the report's header")
		}
	}
	if !strings.HasPrefix(quoted.String(), "crypto https://example.org/x 🚀 tap /resume") ||
		!strings.HasSuffix(quoted.String(), "/pause") {
		t.Fatalf("the quoted spans do not run from the start to the end of the member's text: %.80q…", quoted.String())
	}
}

// summaryCounters gives the day the clock is on enough counters for a daily
// summary in several parts.
func summaryCounters(t *testing.T, h *harness) {
	t.Helper()
	day := h.k.Clock.Now().UTC().Format(time.DateOnly)
	if err := h.k.Store.Write(h.k.Ctx, func(tx *sql.Tx) error {
		for i := 0; i < 300; i++ {
			if err := store.IncrCounter(h.k.Ctx, tx, day, fmt.Sprintf("a counter the summary lists by name, number %03d", i)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// postedOnce fails when two posts that went through carry the same text, and
// returns the texts that went through, in order.
func postedOnce(t *testing.T, posts []telegramtest.Request) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	for _, p := range posts {
		if p.MessageID == 0 {
			continue // refused, or told to wait
		}
		text := p.Params["text"]
		if seen[text] {
			t.Fatalf("a part was posted twice: %.80q…", text)
		}
		seen[text] = true
		out = append(out, text)
	}
	return out
}

// TestLongSummaryPostsEachPartOnce: a daily summary in several parts that
// Telegram interrupts (one part refused, a later one told to wait) resumes
// after the last part posted: the refused part does not hold back the rest,
// no part is posted twice, and the day is marked done.
func TestLongSummaryPostsEachPartOnce(t *testing.T) {
	h := newHarness(t, "")
	h.drain() // the first run starts the summaries with today
	summaryCounters(t, h)
	h.k.Clock.Advance(24 * time.Hour)
	h.srv.Fail("sendMessage", telegramtest.Reply{})
	h.srv.Fail("sendMessage", telegramtest.Reply{Code: 400, Description: "Bad Request: something odd"})
	h.srv.Fail("sendMessage", telegramtest.Reply{Code: 429, Description: "Too Many Requests: retry after 1", RetryAfter: 1})
	if _, err := h.chat.Step(h.k.Ctx); err == nil {
		t.Fatal("no error from the step Telegram told to wait")
	}
	if n := len(h.srv.Requests("sendMessage")); n != 3 {
		t.Fatalf("%d sends before the wait, want 3: a part, the refused part, the part told to wait", n)
	}
	h.drain()
	texts := postedOnce(t, h.srv.Requests("sendMessage"))
	if len(texts) < 3 || !strings.HasPrefix(texts[0], "Daily summary for 2026-10-06") ||
		!strings.Contains(texts[len(texts)-1], "number 299") {
		t.Fatalf("%d parts posted; want the whole summary, first to last", len(texts))
	}
	if got := statusOf(t, h, store.StatusSummaryDay); got != "2026-10-06" {
		t.Fatalf("summary day %q, want 2026-10-06", got)
	}
	n := len(h.srv.Requests("sendMessage"))
	h.k.Clock.Advance(time.Hour)
	h.drain()
	if got := len(h.srv.Requests("sendMessage")); got != n {
		t.Fatalf("%d more sends after the summary was done", got-n)
	}
	// The next day's summary starts from its first part, whatever the day
	// before had posted.
	h.k.Deliver(h.k.Msg("K1", modtest.G1, modtest.Member, "anyone into crypto?"))
	h.drain()
	h.k.Clock.Advance(24 * time.Hour)
	h.drain()
	if got := requests(h, "sendMessage", "Daily summary for 2026-10-07"); got != 1 {
		t.Fatalf("%d summaries for the next day, want 1", got)
	}
}

// TestSummaryCutShortFinishesOnALaterDay: a daily summary interrupted after
// its first part (a 500), retried only after the next UTC midnight, finishes
// the same summary from where it stopped: no part twice, none missing.
func TestSummaryCutShortFinishesOnALaterDay(t *testing.T) {
	h := newHarness(t, "")
	h.drain() // the first run starts the summaries with today
	summaryCounters(t, h)
	h.k.Clock.Advance(24 * time.Hour)
	h.srv.Fail("sendMessage", telegramtest.Reply{})
	h.srv.Fail("sendMessage", telegramtest.Reply{Code: 500, Description: "Internal Server Error"})
	if _, err := h.chat.Step(h.k.Ctx); err == nil {
		t.Fatal("no error from the step whose send failed")
	}
	h.k.Clock.Advance(24 * time.Hour)
	h.drain()
	texts := postedOnce(t, h.srv.Requests("sendMessage"))
	all := strings.Join(texts, "\n")
	for i := 0; i < 300; i++ {
		if n := strings.Count(all, fmt.Sprintf("number %03d\n", i)) + strings.Count(all, fmt.Sprintf("number %03d: ", i)); n != 1 {
			t.Fatalf("counter %03d posted %d times", i, n)
		}
	}
	if n := strings.Count(all, "Daily summary for 2026-10-06"); n != 1 {
		t.Fatalf("the 2026-10-06 heading posted %d times", n)
	}
}

// TestSummaryResumesTheTextItStarted: a community renamed between a daily
// summary cut short and its retry does not move where the retry resumes:
// the rest of the text it began goes out, every line exactly once, under the
// name it began with.
func TestSummaryResumesTheTextItStarted(t *testing.T) {
	h := newHarness(t, "")
	h.drain() // the first run starts the summaries with today
	summaryCounters(t, h)
	day := h.k.Clock.Now().UTC().Format(time.DateOnly)
	if err := h.k.Store.Write(h.k.Ctx, func(tx *sql.Tx) error {
		return store.IncrCounter(h.k.Ctx, tx, day, "keyword_only|"+string(modtest.Community)+"|crypto")
	}); err != nil {
		t.Fatal(err)
	}
	h.k.Clock.Advance(24 * time.Hour)
	h.srv.Fail("sendMessage", telegramtest.Reply{})
	h.srv.Fail("sendMessage", telegramtest.Reply{Code: 500, Description: "Internal Server Error"})
	if _, err := h.chat.Step(h.k.Ctx); err == nil {
		t.Fatal("no error from the step whose send failed")
	}
	h.k.ReloadConfig(strings.Replace(modtest.Config, "name: community a", "name: community a, renamed to a much longer name", 1))
	h.drain()
	texts := postedOnce(t, h.srv.Requests("sendMessage"))
	all := strings.Join(texts, "\n")
	for i := 0; i < 300; i++ {
		if n := strings.Count(all, fmt.Sprintf("number %03d\n", i)) + strings.Count(all, fmt.Sprintf("number %03d: ", i)); n != 1 {
			t.Fatalf("counter %03d posted %d times", i, n)
		}
	}
	if n := strings.Count(all, "Keyword-only posts not acted on in community a: crypto 1\n"); n != 1 {
		t.Fatalf("the keyword-only line posted %d times under the name the summary began with", n)
	}
	if strings.Contains(all, "renamed") {
		t.Fatal("the retry rebuilt the summary with the new name")
	}
	if got := statusOf(t, h, store.StatusSummaryText); got != "" {
		t.Fatalf("summary text %.40q kept after the day was done", got)
	}
}

// TestFollowupsResumeWhereTheTextStopped: a report's header changes between
// attempts (its attachment line, once the download settles). A retry posts
// the member's text from where the posted parts stopped, so every line goes
// exactly once.
func TestFollowupsResumeWhereTheTextStopped(t *testing.T) {
	h := newHarness(t, "")
	var b strings.Builder
	b.WriteString("crypto https://example.org/x\n")
	for i := 0; i < 1500; i++ {
		fmt.Fprintf(&b, "line %04d\n", i)
	}
	b.WriteString("THE END")
	h.srv.Fail("sendMessage", telegramtest.Reply{})
	h.srv.Fail("sendMessage", telegramtest.Reply{Code: 429, Description: "Too Many Requests: retry after 1", RetryAfter: 1})
	h.k.Deliver(image(h.k.Msg("F1", modtest.G1, modtest.Spammer, b.String())))
	if _, err := h.chat.Step(h.k.Ctx); err == nil {
		t.Fatal("no error from the step Telegram told to wait")
	}
	h.k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
		return nil, "", "", errors.New(strings.Repeat("the media host kept failing; ", 20))
	}
	if _, err := h.k.Media.Fetch(h.k.Ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	all := strings.Join(postedOnce(t, h.srv.Posted()), "\n")
	for i := 0; i < 1500; i++ {
		if n := strings.Count(all, fmt.Sprintf("line %04d\n", i)); n != 1 {
			t.Fatalf("line %04d posted %d times", i, n)
		}
	}
	if !strings.Contains(all, "THE END") {
		t.Fatal("the message's end was not posted")
	}
}

// TestLongDisplayNameKeepsTheHeaderInTheFirstPart: a sender's display name
// far longer than a Telegram message is clipped in the report's header, so a
// report whose second part Telegram tells to wait still shows the rule, the
// action and the config version once the retry posts the rest.
func TestLongDisplayNameKeepsTheHeaderInTheFirstPart(t *testing.T) {
	h := newHarness(t, "")
	var b strings.Builder
	b.WriteString("crypto https://example.org/x\n")
	for i := 0; i < 1500; i++ {
		fmt.Fprintf(&b, "line %04d\n", i)
	}
	b.WriteString("THE END")
	msg := h.k.Msg("N1", modtest.G1, modtest.Spammer, b.String())
	msg.PushName = strings.Repeat("n", 5000)
	h.srv.Fail("sendMessage", telegramtest.Reply{})
	h.srv.Fail("sendMessage", telegramtest.Reply{Code: 429, Description: "Too Many Requests: retry after 1", RetryAfter: 1})
	h.k.Deliver(msg)
	if _, err := h.chat.Step(h.k.Ctx); err == nil {
		t.Fatal("no error from the step Telegram told to wait")
	}
	h.drain()
	texts := postedOnce(t, h.srv.Posted())
	if len(texts) == 0 {
		t.Fatal("nothing posted")
	}
	for _, line := range []string{"\nRule: pitch\n", "\nAction: deleted for everyone; sender removed and banned\n", "\nConfig: v",
		"\nSender: " + strings.Repeat("n", nameMaxForTest-1) + "… ("} {
		if !strings.Contains(texts[0], line) {
			t.Fatalf("the report's first part lacks %.60q", line)
		}
	}
	all := strings.Join(texts, "\n")
	for i := 0; i < 1500; i++ {
		if n := strings.Count(all, fmt.Sprintf("line %04d\n", i)); n != 1 {
			t.Fatalf("line %04d posted %d times", i, n)
		}
	}
}

// nameMaxForTest is how much of a display name a report's header shows.
const nameMaxForTest = 200

// TestHeaderFieldsFromOutsideTheBotAreClipped: every header field the bot does
// not write (the group's name, the display name, the attachment's file name
// and its error) is clipped, so the header stays within the first message
// however long they are.
func TestHeaderFieldsFromOutsideTheBotAreClipped(t *testing.T) {
	long := func(c string) string { return strings.Repeat(c, 5000) }
	clipped := func(c string) string { return strings.Repeat(c, nameMaxForTest-1) + "…" }
	ev := &store.Evidence{Chat: string(modtest.G1), Sender: string(modtest.Spammer), PushName: long("n"), Rule: "pitch",
		ConfigHash: "abc", MediaKind: "document", MediaName: long("f"), MediaState: store.MediaFailed, MediaError: long("e")}
	h := telegram.Header(store.Report{Kind: ledger.KindAction, Text: "Spam deleted"}, ev, long("g"), true)
	for _, want := range []string{"\nGroup: " + clipped("g") + " (", "\nSender: " + clipped("n") + " (",
		"\nAttachment: document " + clipped("f") + ", not available: " + clipped("e") + "\n", "\nRule: pitch\n"} {
		if !strings.Contains(h, want) {
			t.Errorf("header lacks %.80q", want)
		}
	}
	if n := len(utf16.Encode([]rune(h))); n > telegram.MaxUnits/2 {
		t.Fatalf("header is %d UTF-16 units, want it well within one message", n)
	}
}

// TestRetryAfterARefusedFollowupPostsNoPartTwice: Telegram refuses one
// follow-up, then tells a later part to wait; the retry resumes after the
// last part posted, so no part goes twice and the message still ends.
func TestRetryAfterARefusedFollowupPostsNoPartTwice(t *testing.T) {
	h := newHarness(t, "")
	var b strings.Builder
	b.WriteString("crypto https://example.org/x\n")
	for i := 0; i < 1500; i++ {
		fmt.Fprintf(&b, "line %04d\n", i)
	}
	b.WriteString("THE END")
	for _, r := range []telegramtest.Reply{{}, {Code: 400, Description: "Bad Request: something odd"}, {},
		{Code: 429, Description: "Too Many Requests: retry after 1", RetryAfter: 1}} {
		h.srv.Fail("sendMessage", r)
	}
	h.k.Deliver(h.k.Msg("F1", modtest.G1, modtest.Spammer, b.String()))
	if _, err := h.chat.Step(h.k.Ctx); err == nil {
		t.Fatal("no error from the step Telegram told to wait")
	}
	h.drain()
	texts := postedOnce(t, h.srv.Posted())
	if len(texts) < 3 || !strings.Contains(texts[len(texts)-1], "THE END") {
		t.Fatalf("%d parts posted; want all but the refused one, through THE END", len(texts))
	}
	if r, ok, err := h.k.Store.Report(h.k.Ctx, h.only(ledger.KindAction).ID); err != nil || !ok || r.SentAt.IsZero() {
		t.Fatalf("report not marked sent (%v)", err)
	}
}

// TestFailingUploadDoesNotBlock: an attachment upload Telegram keeps failing
// (a 500, not a refusal) is tried again after a wait, not on every step, so
// the work behind it (the daily summary) still goes; after
// report.attachment_show_hours it is given up: marked failed, its file kept
// for the purge, and not tried again.
func TestFailingUploadDoesNotBlock(t *testing.T) {
	h := newHarness(t, "report:\n  attachment_show_hours: 47\n")
	h.drain() // the first run starts the summaries with today
	h.k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
		return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
	}
	h.srv.FailAlways("sendDocument", telegramtest.Reply{Code: 500, Description: "Internal Server Error"})
	h.k.Deliver(image(h.k.Spam("I1", modtest.G1)))
	h.k.Deliver(h.k.Msg("K1", modtest.G1, modtest.Member, "anyone into crypto?"))
	h.k.Fire()
	if _, err := h.k.Media.Fetch(h.k.Ctx); err != nil {
		t.Fatal(err)
	}
	h.drain()
	h.drain()
	uploads := func() int { return len(h.srv.Requests("sendDocument")) }
	if n := uploads(); n != 1 {
		t.Fatalf("%d uploads, want 1 and then a wait", n)
	}
	evidence := func() store.Evidence {
		t.Helper()
		r, _, err := h.k.Store.Report(h.k.Ctx, h.only(ledger.KindAction).ID)
		if err != nil {
			t.Fatal(err)
		}
		ev, ok, err := h.k.Store.Evidence(h.k.Ctx, r.EvidenceID)
		if err != nil || !ok {
			t.Fatalf("evidence of report %d: %v %v", r.ID, ok, err)
		}
		return ev
	}
	h.k.Clock.Advance(24 * time.Hour)
	h.drain()
	if n := requests(h, "sendMessage", "Daily summary for 2026-10-06"); n != 1 {
		t.Fatalf("%d daily summaries while an upload kept failing, want 1", n)
	}
	if n, ev := uploads(), evidence(); n != 2 || ev.MediaState != store.MediaSaved {
		t.Fatalf("%d uploads, attachment %s; want a second try, still saved", n, ev.MediaState)
	}
	h.k.Clock.Advance(24 * time.Hour)
	h.drain()
	if ev := evidence(); ev.MediaState != store.MediaFailed || ev.MediaPath == "" ||
		!strings.Contains(ev.MediaError, "Telegram did not take the file") {
		t.Fatalf("evidence %+v, want failed with its file kept", ev)
	}
	r := h.only(ledger.KindAction)
	if n := saidNotPosted(h, r.ID, "Telegram did not take the file in 47h0m0s"); n != 1 {
		t.Fatalf("%d replies under the report saying the attachment was given up, want 1", n)
	}
	h.press(adminUser, "show", r.ID)
	if got := h.lastReply(); !strings.Contains(got, "could not be posted: Telegram did not take the file") {
		t.Fatalf("[Show attachment] answered %q, want why it could not be posted", got)
	}
	h.srv.Clear()
	h.k.Clock.Advance(time.Hour)
	h.drain()
	if n := uploads(); n != 3 {
		t.Fatalf("%d uploads, want 3 (none after giving up)", n)
	}
}

// deliverOnly runs delivery (not attachment posting) until it has nothing
// left to do.
func deliverOnly(t *testing.T, h *harness) {
	t.Helper()
	for did := true; did; {
		var err error
		if did, err = h.chat.Step(h.k.Ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// saidNotPosted counts the replies under report reportID's post saying its
// attachment will not be posted, giving why.
func saidNotPosted(h *harness, reportID int64, why string) int {
	h.t.Helper()
	n := 0
	for _, req := range h.srv.Requests("sendMessage") {
		if strings.HasPrefix(req.Params["text"], "Attachment not posted: "+why) &&
			strings.Contains(req.Params["reply_parameters"], `"message_id":`+strconv.Itoa(h.head(reportID))) {
			n++
		}
	}
	return n
}

// TestPriorityAlertNotHeldByAnUpload: while the automatic post of an
// attachment uploads (held here by the fake API, as a slow uplink would), a
// priority alert raised meanwhile is posted at once: uploads run apart from
// delivery.
func TestPriorityAlertNotHeldByAnUpload(t *testing.T) {
	h := newHarness(t, "")
	h.k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
		return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
	}
	h.k.Deliver(image(h.k.Spam("I1", modtest.G1)))
	h.k.Fire()
	if n, err := h.k.Media.Fetch(h.k.Ctx); err != nil || n != 1 {
		t.Fatalf("fetched %d (%v)", n, err)
	}
	arrived, release := h.srv.Hold("sendDocument")
	defer release()
	ctx, cancel := context.WithCancel(h.k.Ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.chat.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the attachment upload never started")
	}
	if err := h.chat.Alert(h.k.Ctx, alert.Alert{Kind: alert.Breaker, Text: "too many removals"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); requests(h, "sendMessage", "too many removals") == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the priority alert waited behind the upload")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBackingOffUploadsDoNotFillTheQueue: with more uploads failing than one
// step looks at, an attachment behind them is still posted on the next step.
func TestBackingOffUploadsDoNotFillTheQueue(t *testing.T) {
	h := newHarness(t, "")
	h.k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
		return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
	}
	const failing = 10
	for i := 1; i <= failing+1; i++ {
		h.k.Deliver(image(h.k.Spam(fmt.Sprintf("I%d", i), modtest.G1)))
	}
	h.k.Fire()
	if n, err := h.k.Media.Fetch(h.k.Ctx); err != nil || n != failing+1 {
		t.Fatalf("fetched %d (%v), want %d", n, err, failing+1)
	}
	// The reports go first, and a minute passes: the uploads alone then fit
	// inside the first failure's backoff (a minute), with no message budget
	// wait moving the clock past it.
	deliverOnly(t, h)
	h.k.Clock.Advance(time.Minute + time.Second)
	for i := 0; i < failing; i++ {
		h.srv.Fail("sendDocument", telegramtest.Reply{Code: 500, Description: "Internal Server Error"})
	}
	h.drain()
	reps := h.k.Reports(ledger.KindAction)
	if len(reps) != failing+1 {
		t.Fatalf("%d action reports, want %d", len(reps), failing+1)
	}
	if n := len(h.srv.Requests("sendDocument")); n != failing+1 {
		t.Fatalf("%d uploads, want each of the %d failing ones once and then the last", n, failing)
	}
	for i, r := range reps {
		posted := false
		for _, m := range h.messages(r.ID) {
			posted = posted || m.Role == store.RoleAttachment
		}
		if want := i == failing; posted != want {
			t.Fatalf("report %d of %d: attachment posted %v, want %v", i+1, len(reps), posted, want)
		}
	}
}

// TestAttachmentForgottenDuringItsUpload: `member forget` runs while the
// member's attachment uploads, and its take-down finds nothing posted yet;
// once the upload lands the post is taken down at once, not left showing. A
// take-down Telegram fails then is retried (the record queued it), not left
// for the attachment_show_hours take-down.
func TestAttachmentForgottenDuringItsUpload(t *testing.T) {
	for _, failOnce := range []bool{false, true} {
		t.Run("take-down fails once "+strconv.FormatBool(failOnce), func(t *testing.T) {
			h := newHarness(t, "")
			h.k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
				return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
			}
			h.k.Deliver(image(h.k.Spam("I1", modtest.G1)))
			h.k.Fire()
			if n, err := h.k.Media.Fetch(h.k.Ctx); err != nil || n != 1 {
				t.Fatalf("fetched %d (%v)", n, err)
			}
			deliverOnly(t, h)
			arrived, release := h.srv.Hold("sendDocument")
			defer release()
			uploaded := make(chan error, 1)
			go func() {
				_, err := h.chat.PostAttachment(h.k.Ctx)
				uploaded <- err
			}()
			select {
			case <-arrived:
			case <-time.After(10 * time.Second):
				t.Fatal("the attachment upload never started")
			}
			if _, err := h.k.Store.ForgetMember(h.k.Ctx, []string{string(modtest.Spammer), string(modtest.SpammerPhone)}); err != nil {
				t.Fatal(err)
			}
			h.drain() // the forget's take-down runs while nothing is posted yet
			if n := len(h.srv.Requests("deleteMessage")); n != 0 {
				t.Fatalf("%d deletions before the upload landed", n)
			}
			deletions := 1
			if failOnce {
				h.srv.Fail("deleteMessage", telegramtest.Reply{Code: 500, Description: "Internal Server Error"})
				deletions = 2
			}
			release()
			if err := <-uploaded; (err != nil) != failOnce {
				t.Fatalf("upload: %v, want an error %v", err, failOnce)
			}
			h.drain()
			post := attachmentPost(t, h)
			if post.GoneAt.IsZero() || len(h.srv.Requests("deleteMessage")) != deletions {
				t.Fatalf("post %+v, %d deletions: want the forgotten member's attachment taken down", post,
					len(h.srv.Requests("deleteMessage")))
			}
			if !strings.Contains(h.logs.String(), "the evidence copy was forgotten during the upload") {
				t.Fatal("the take-down was not logged")
			}
		})
	}
}

// TestTakeDownSkipsAPostTakenDownWhileItWaited: a take-down waiting for its
// turn lets the delivery slot go; when another loop takes the same post down
// meanwhile, this one sends nothing once its turn comes: neither the delete
// (here told to wait) nor the placeholder that replaces a post Telegram will
// not delete (here waiting its turn in the shared bucket).
func TestTakeDownSkipsAPostTakenDownWhileItWaited(t *testing.T) {
	for _, waits := range []string{"deleteMessage", "editMessageMedia"} {
		t.Run(waits, func(t *testing.T) {
			h := newHarness(t, "")
			attachmentReport(t, h)
			post := attachmentPost(t, h)
			if _, err := h.k.Store.ForgetMember(h.k.Ctx, []string{string(modtest.Spammer), string(modtest.SpammerPhone)}); err != nil {
				t.Fatal(err)
			}
			goneOnce := func() {
				if err := h.k.Store.MarkTGGone(h.k.Ctx, post.ID); err != nil {
					t.Error(err)
				}
			}
			if waits == "deleteMessage" {
				h.srv.Fail("deleteMessage", telegramtest.Reply{Code: 429, Description: "Too Many Requests: retry after 30",
					RetryAfter: 30})
				for i := 0; ; i++ {
					if _, err := h.chat.Step(h.k.Ctx); err != nil {
						break // the take-down was told to wait
					}
					if i == 10 {
						t.Fatal("the take-down was never told to wait")
					}
				}
				// The next try waits out the retry_after; meanwhile another
				// loop takes the post down.
				h.k.Clock.OnSleep = func(time.Duration) { goneOnce() }
			} else {
				// Telegram will not delete it; while the placeholder waits its
				// turn, another loop takes the post down.
				h.srv.Fail("deleteMessage", telegramtest.Reply{Code: 400, Description: "Bad Request: message can't be deleted"})
				h.k.Clock.OnSleep = func(time.Duration) {
					if len(h.srv.Requests("deleteMessage")) == 1 {
						goneOnce()
					}
				}
			}
			h.drain()
			h.k.Clock.OnSleep = nil
			if n, m := len(h.srv.Requests("deleteMessage")), len(h.srv.Requests("editMessageMedia")); n != 1 || m != 0 {
				t.Fatalf("%d deletes and %d replacements, want only the one delete", n, m)
			}
			if !strings.Contains(h.logs.String(), "attachment post already taken down") {
				t.Fatal("the skipped take-down was not logged")
			}
			if edits, err := h.k.Store.ReportEdits(h.k.Ctx); err != nil || len(edits) != 0 {
				t.Fatalf("edits left %+v (%v)", edits, err)
			}
		})
	}
}

// TestUploadDoesNotHoldTheSlot: while an attachment uploads (here after a
// [Show attachment] press), a fatal alert still posts at once.
func TestUploadDoesNotHoldTheSlot(t *testing.T) {
	h := newHarness(t, "")
	r := attachmentReport(t, h)
	head := h.head(r.ID)
	arrived, release := h.srv.Hold("sendDocument")
	defer release()
	pressed := make(chan struct{})
	go func() {
		defer close(pressed)
		h.chat.Handle(h.k.Ctx, &models.Update{ID: 1, CallbackQuery: &models.CallbackQuery{ID: "q1",
			From: models.User{ID: adminUser, FirstName: "Ann"},
			Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: head,
				Chat: models.Chat{ID: telegramtest.ChatID, Type: "supergroup"}}},
			Data: "show:" + strconv.FormatInt(r.ID, 10)}})
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the [Show attachment] upload never started")
	}
	ctx, cancel := context.WithTimeout(h.k.Ctx, 2*time.Second)
	defer cancel()
	if err := h.chat.Alert(ctx, alert.Alert{Kind: alert.FatalDisconnect, Text: "logged out: re-pair"}); err != nil {
		t.Fatalf("the fatal alert waited behind an upload: %v", err)
	}
	release()
	<-pressed
	if n := len(h.srv.Requests("sendDocument")); n != 2 {
		t.Fatalf("%d uploads, want the first and the one shown again", n)
	}
}

// TestMovedChatSettlesItsOldMessages: once the admin chat became a supergroup
// its old messages can be neither edited nor deleted (Telegram answers with
// the new ID): the text removal and the attachment take-down settle them
// instead of failing on every step and holding up the work behind them.
func TestMovedChatSettlesItsOldMessages(t *testing.T) {
	const moved int64 = -1009999000077
	upgraded := telegramtest.Reply{Code: 400, Description: "Bad Request: group chat was upgraded to a supergroup chat",
		MigrateTo: moved}
	t.Run("text removal", func(t *testing.T) {
		h := newHarness(t, "")
		h.k.Deliver(h.k.Spam("E1", modtest.G1))
		h.drain()
		h.srv.FailAlways("editMessageText", upgraded)
		h.k.Clock.Advance(31 * 24 * time.Hour)
		h.drain()
		for _, m := range h.messages(h.only(ledger.KindAction).ID) {
			if m.StrippedAt.IsZero() {
				t.Fatalf("message %+v: its text removal never settled", m)
			}
		}
		if h.chat.ChatID() != moved {
			t.Fatalf("chat %d, want the new ID %d", h.chat.ChatID(), moved)
		}
	})
	t.Run("attachment take-down", func(t *testing.T) {
		h := newHarness(t, "")
		attachmentReport(t, h)
		h.srv.FailAlways("deleteMessage", upgraded)
		h.srv.FailAlways("editMessageMedia", upgraded)
		h.k.Clock.Advance(25 * time.Hour)
		h.drain()
		if attachmentPost(t, h).GoneAt.IsZero() {
			t.Fatal("the take-down never settled")
		}
	})
}

// TestUploadsGetTheirOwnTimeout: the bot's HTTP client gives a file upload
// five minutes (the largest attachment kept, at 1.5 Mbit/s) and every other
// request the minute the library gives it.
func TestUploadsGetTheirOwnTimeout(t *testing.T) {
	for method, want := range map[string]time.Duration{"sendDocument": 5 * time.Minute,
		"editMessageMedia": 5 * time.Minute, "sendMessage": time.Minute, "editMessageText": time.Minute,
		"getUpdates": time.Minute} {
		if got := telegram.RequestTimeout(method); got != want {
			t.Errorf("%s: timeout %s, want %s", method, got, want)
		}
	}
}

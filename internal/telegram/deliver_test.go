package telegram_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

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

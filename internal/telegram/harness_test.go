package telegram_test

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/reconcile"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// The admin chat's people (Telegram user IDs).
const (
	adminUser int64 = 501 // an administrator of the admin chat
	otherUser int64 = 502 // a member, not an administrator
	secondAdm int64 = 503 // another administrator
)

// syncBuf is a log sink safe for concurrent writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// appClock gives the app the kit's clock; its timers never fire (tests call
// what they need directly).
type appClock struct{ *modtest.Clock }

func (appClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

// harness is the admin chat over the real moderation path, a fake WhatsApp
// and a fake Bot API, with the app's real Controls behind the buttons.
type harness struct {
	t    *testing.T
	k    *modtest.Kit
	srv  *telegramtest.Server
	chat *telegram.Chat
	app  *app.App
	logs *syncBuf
}

func newHarness(t *testing.T, extra string) *harness {
	t.Helper()
	return newHarnessKit(t, modtest.New(t, extra))
}

func newHarnessKit(t *testing.T, k *modtest.Kit) *harness {
	t.Helper()
	h := &harness{t: t, k: k, srv: telegramtest.New(t, k.Clock.Now), logs: &syncBuf{}}
	h.srv.SetAdmin(adminUser, true)
	h.srv.SetAdmin(secondAdm, true)
	h.build()
	return h
}

// build (re)creates the chat and the app over the kit's current store.
func (h *harness) build() {
	h.t.Helper()
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	opts := telegram.Options{Token: h.srv.Token, ChatID: telegramtest.ChatID, ServerURL: h.srv.URL} // secret-allow (the fake API's run-time token)
	chat, err := telegram.New(opts, &telegram.Chat{Store: h.k.Store, Config: h.k.Holder, Groups: h.k.Dir, Log: log,
		Now: h.k.Clock.Now, Sleep: h.k.Clock.Sleep})
	if err != nil {
		h.t.Fatal(err)
	}
	h.k.Enforcer.Wake = chat.Wake
	h.app = &app.App{Adapter: h.k.Fake, Store: h.k.Store, Inbox: h.k.Worker.Inbox, Worker: h.k.Worker, Alerter: h.k.Alerts,
		Log: h.k.Log, Clock: appClock{h.k.Clock}, Config: h.k.Holder, Directory: h.k.Dir, Executor: h.k.Exec,
		Sweep: &reconcile.Sweep{Enforcer: h.k.Enforcer, Directory: h.k.Dir, Config: h.k.Holder, Log: h.k.Log,
			Sleep: h.k.Clock.Sleep, Now: h.k.Clock.Now},
		Admin: &pipeline.Admin{Store: h.k.Store, Config: h.k.Holder, Directory: h.k.Dir, Enforcer: h.k.Enforcer, Log: h.k.Log}}
	chat.Controls = h.app.Controls()
	h.chat = chat
}

// restart closes the store as a crash would and builds everything again.
func (h *harness) restart() {
	h.t.Helper()
	h.k.Reopen()
	h.build()
}

// drain runs delivery until nothing is left to do.
func (h *harness) drain() {
	h.t.Helper()
	for i := 0; i < 1000; i++ {
		did, err := h.chat.Step(h.k.Ctx)
		if err != nil {
			h.t.Fatalf("step %d: %v", i, err)
		}
		if !did {
			return
		}
	}
	h.t.Fatal("delivery never settled")
}

// report adds a report straight to the store.
func (h *harness) report(r store.Report) int64 {
	h.t.Helper()
	id, err := h.k.Store.AddReport(h.k.Ctx, r, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return id
}

// only returns the single report of kind.
func (h *harness) only(kind string) store.Report {
	h.t.Helper()
	reps := h.k.Reports(kind)
	if len(reps) != 1 {
		h.t.Fatalf("%d %s reports, want 1: %+v", len(reps), kind, reps)
	}
	return reps[0]
}

// messages lists what the bot posted for a report.
func (h *harness) messages(reportID int64) []store.TGMessage {
	h.t.Helper()
	ms, err := h.k.Store.TGMessagesFor(h.k.Ctx, reportID)
	if err != nil {
		h.t.Fatal(err)
	}
	return ms
}

// head is the message ID of a report's own post.
func (h *harness) head(reportID int64) int {
	h.t.Helper()
	for _, m := range h.messages(reportID) {
		if m.Role == store.RoleReport {
			return m.MessageID
		}
	}
	h.t.Fatalf("report %d was never posted", reportID)
	return 0
}

// press is a button press on a report's post by user.
func (h *harness) press(user int64, code string, reportID int64) {
	h.t.Helper()
	h.pressIn(telegramtest.ChatID, user, code, reportID)
}

// pressIn is a button press arriving from chat.
func (h *harness) pressIn(chat, user int64, code string, reportID int64) {
	h.t.Helper()
	h.chat.Handle(h.k.Ctx, &models.Update{ID: 1, CallbackQuery: &models.CallbackQuery{ID: "q" + strconv.Itoa(int(reportID)),
		From: models.User{ID: user, FirstName: name(user)},
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: h.head(reportID),
			Chat: models.Chat{ID: chat, Type: "supergroup"}}},
		Data: code + ":" + strconv.FormatInt(reportID, 10)}})
}

// command sends text in chat by user, optionally as a reply to replyTo.
func (h *harness) command(chat, user int64, text string, replyTo int) {
	h.t.Helper()
	m := &models.Message{ID: 9000, Chat: models.Chat{ID: chat, Type: "supergroup"}, Text: text,
		From: &models.User{ID: user, FirstName: name(user)}}
	if replyTo != 0 {
		m.ReplyToMessage = &models.Message{ID: replyTo, Chat: m.Chat}
	}
	h.chat.Handle(h.k.Ctx, &models.Update{ID: 2, Message: m})
}

func name(user int64) string {
	switch user {
	case adminUser:
		return "Ann"
	case secondAdm:
		return "Bea"
	}
	return "Zed"
}

// lastReply is the text of the last message posted.
func (h *harness) lastReply() string {
	h.t.Helper()
	posted := h.srv.Requests("sendMessage")
	if len(posted) == 0 {
		h.t.Fatal("nothing was posted")
	}
	return posted[len(posted)-1].Params["text"]
}

// answers lists the texts button presses were answered with.
func (h *harness) answers() []string {
	var out []string
	for _, r := range h.srv.Requests("answerCallbackQuery") {
		out = append(out, r.Params["text"])
	}
	return out
}

// spam delivers a confirmed "pitch" match from the spammer in G1 and fires
// what it queued.
func (h *harness) spam(id string) store.Report {
	h.t.Helper()
	m := h.k.Spam(id, modtest.G1)
	m.PushName = "Crypto King"
	h.k.Deliver(m)
	h.k.Fire()
	reps := h.k.Reports("action")
	return reps[len(reps)-1]
}

func contains(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

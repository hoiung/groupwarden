package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/config/configtest"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// TestBuildWiresEveryWake: the bot `run` builds (app.Build) wakes each worker
// when there is work for it, so nothing waits for its idle poll. A decided
// spam post with an attachment reaches the executor (idle 5s), the admin chat
// (30s) and the attachment fetcher (1 minute) at once; a message the worker
// set aside and a report the executor stored each wake the admin chat. The
// purge of message secrets waits for the group list.
func TestBuildWiresEveryWake(t *testing.T) {
	const prompt = 1500 * time.Millisecond // well under the shortest idle poll
	api := telegramtest.New(t, nil)
	holder, _ := configtest.Holder(t, modtest.Config)
	cfg := holder.Current().Config
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	st, err := store.Open(context.Background(), cfg.StoreDB(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := &clienttest.Fake{Groups: modtest.Groups(), SelfIDs: client.Self{Phone: modtest.BotPhone, LID: modtest.Bot},
		Download: func(context.Context, *client.Message) ([]byte, string, string, error) {
			return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
		}}
	a, err := Build(Parts{Adapter: fake, Store: st, Inbox: pipeline.NewInbox(st), Config: holder,
		Telegram: telegram.Options{Token: api.Token, ChatID: telegramtest.ChatID, ServerURL: api.URL}, // secret-allow (the fake API's run-time token)
		Alerts:   NewAlertSink(alert.Log{Logger: log}), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	closed := func(c <-chan struct{}) bool {
		select {
		case <-c:
			return true
		default:
			return false
		}
	}
	if a.Purger.Ready == nil || closed(a.Purger.Ready) {
		t.Fatal("the purge of message secrets does not wait for the group list")
	}
	a.AdminChat.(*telegram.Chat).Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	state := func() string {
		return fmt.Sprintf("WhatsApp calls %v; Telegram posts %d; log:\n%s", fake.Calls(), len(api.Posted()), logs.String())
	}
	waitFor(t, "the group list and the started message", func() bool {
		return a.Directory.IsLoaded() && len(api.Posted()) >= 1
	}, state)
	if !closed(a.Purger.Ready) {
		t.Fatal("the purge still waits after the group list was read")
	}
	if a.Executor.Connected == nil || !a.Executor.Connected() {
		t.Fatal("the executor is not told WhatsApp is connected")
	}
	// Every worker idle: the start-up posts are out and nothing has moved for
	// a second.
	for n := -1; n != len(api.Posted())+len(fake.Calls()); {
		n = len(api.Posted()) + len(fake.Calls())
		time.Sleep(time.Second)
	}
	promptly := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(prompt)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("%s not within %s: %s", what, prompt, state())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	posts := len(api.Posted())
	spam := &client.Message{Chat: modtest.G1, Sender: modtest.Spammer, SenderAlt: modtest.SpammerPhone, ID: "M1",
		TargetID: "M1", Time: time.Now(), Fields: []client.Field{{Name: "body", Text: modtest.SpamText, Match: modtest.SpamText}},
		Media: &client.Media{Kind: "image", MimeType: "image/jpeg", FileName: "offer.jpg", Size: 8, Raw: []byte("raw")}}
	if err := fake.Deliver(spam); err != nil {
		t.Fatal(err)
	}
	promptly("the delete", func() bool { return fake.Count("Revoke") == 1 })
	promptly("the report in the admin chat", func() bool { return len(api.Posted()) > posts })
	promptly("the attachment download", func() bool { return fake.Count("DownloadMedia") == 1 })

	for name, wake := range map[string]func(){"a set-aside message": a.Worker.Wake, "an executor report": a.Executor.Reported} {
		time.Sleep(time.Second)
		posts := len(api.Posted())
		if _, err := st.AddReport(ctx, store.Report{Kind: string(alert.Undecided), Text: "woken by " + name}, nil); err != nil {
			t.Fatal(err)
		}
		wake()
		promptly("the admin chat woken by "+name, func() bool { return len(api.Posted()) > posts })
	}
}

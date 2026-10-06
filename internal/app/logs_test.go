package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
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

// lockedBuffer is a log destination several goroutines write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestLogsMaskIDsNoBodyNoSecrets: the log of the bot `run` builds (app.Build),
// at debug level, through a spam post's whole path (decision, delete with a
// WhatsApp error that quotes the IDs, removal, ban, the report to the admin
// chat with a Telegram failure that quotes the token) and a clean post, is
// structured JSON; every decision names its rule and config hash; no phone
// number, LID or group ID appears unmasked; no message text appears above
// debug; and neither the Telegram token nor the heartbeat URL appears at all.
func TestLogsMaskIDsNoBodyNoSecrets(t *testing.T) {
	const (
		spamBody  = "cheap crypto signals https://example.org/join zqwobble"
		cleanBody = "see you all at the meetup on friday zqflump"
		pingMark  = "hb-7f3e-check-path"
	)
	api := telegramtest.New(t, nil)
	holder, _ := configtest.Holder(t, modtest.Config+"heartbeat_url: https://hc.example.org/ping/"+pingMark+"\n")
	cfg := holder.Current().Config
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	alerts := NewAlertSink(alert.Log{Logger: log})
	st, err := store.Open(context.Background(), cfg.StoreDB(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := &clienttest.Fake{Groups: modtest.Groups(), SelfIDs: client.Self{Phone: modtest.BotPhone, LID: modtest.Bot}}
	// WhatsApp's first refusal quotes the IDs, as its errors do.
	fake.FailNext("Revoke", fmt.Errorf("server refused to revoke %s from %s in %s", "M1", modtest.SpammerPhone, modtest.G1))
	// Telegram's first answer is a broken proxy quoting the request path,
	// which holds the token.
	api.Fail("sendMessage", telegramtest.Reply{EchoPath: true})
	a, err := Build(Parts{Adapter: fake, Store: st, Inbox: pipeline.NewInbox(st), Config: holder,
		Telegram: telegram.Options{Token: api.Token, ChatID: telegramtest.ChatID, ServerURL: api.URL}, // secret-allow (the fake API's run-time token)
		Alerts:   alerts, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	waitFor(t, "the group list", func() bool { return a.Directory.IsLoaded() }, logs.String)
	now := time.Now()
	spam := &client.Message{Chat: modtest.G1, Sender: modtest.Spammer, SenderAlt: modtest.SpammerPhone, ID: "M1",
		TargetID: "M1", Time: now, Fields: []client.Field{{Name: "body", Text: spamBody, Match: spamBody}}}
	clean := &client.Message{Chat: modtest.GB, Sender: modtest.Member, ID: "M2", TargetID: "M2", Time: now,
		Fields: []client.Field{{Name: "body", Text: cleanBody, Match: cleanBody}}}
	for _, m := range []*client.Message{spam, clean} {
		if err := fake.Deliver(m); err != nil {
			t.Fatal(err)
		}
	}
	// The refused delete is retried after a backoff longer than this test;
	// its logged retry is what is checked.
	waitFor(t, "the refused delete, the removals and the report", func() bool {
		return fake.Count("Revoke") >= 1 && fake.Count("Remove") >= 2 && len(api.Posted()) >= 2 &&
			strings.Contains(logs.String(), `"msg":"action failed; retrying"`)
	}, func() string {
		return fmt.Sprintf("WhatsApp calls %v; Telegram posts %d; log:\n%s", fake.Calls(), len(api.Posted()), logs.String())
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	type line struct {
		raw    string
		fields map[string]any
	}
	var lines []line
	sc := bufio.NewScanner(strings.NewReader(logs.String()))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var f map[string]any
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("a log line is not JSON (%v): %s", err, sc.Text())
		}
		if f["level"] == nil || f["msg"] == nil {
			t.Fatalf("a log line without level and msg: %s", sc.Text())
		}
		lines = append(lines, line{raw: sc.Text(), fields: f})
	}
	hash := holder.Current().Hash
	decisions, retried, refused := 0, false, false
	for _, l := range lines {
		switch l.fields["msg"] {
		case "decision":
			decisions++
			if l.fields["rule"] != "pitch" || l.fields["config"] != "v"+hash {
				t.Errorf("a decision without its rule and config hash: %s", l.raw)
			}
		case "action failed; retrying":
			retried = true
		}
		if strings.Contains(l.raw, "bad gateway") {
			refused = true
		}
	}
	// The premises: the paths whose logging is checked below did run.
	if decisions == 0 || !retried || !refused {
		t.Fatalf("decision logged %d times, WhatsApp retry logged %v, Telegram refusal logged %v:\n%s",
			decisions, retried, refused, logs.String())
	}
	ids := []client.JID{modtest.Spammer, modtest.SpammerPhone, modtest.Member, modtest.Admin, modtest.AdminPhone,
		modtest.Bot, modtest.BotPhone, modtest.G1, modtest.G2, modtest.GB, modtest.Community}
	secrets := []string{api.Token, api.Token[strings.Index(api.Token, ":")+1:], pingMark}
	for _, l := range lines {
		for _, id := range ids {
			if user := string(id[:strings.Index(string(id), "@")]); strings.Contains(l.raw, user) {
				t.Errorf("unmasked %s: %s", id, l.raw)
			}
		}
		for _, s := range secrets {
			if strings.Contains(l.raw, s) {
				t.Errorf("a secret in the log: %s", l.raw)
			}
		}
		if l.fields["level"] != "DEBUG" && (strings.Contains(l.raw, "zqwobble") || strings.Contains(l.raw, "zqflump")) {
			t.Errorf("message text above debug: %s", l.raw)
		}
	}
}

// waitFor waits for cond; on a timeout it fails with state's account.
func waitFor(t *testing.T, what string, cond func() bool, state func() string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %s", what, state())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

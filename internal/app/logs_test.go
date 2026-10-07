package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/config/configtest"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/mask"
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

// TestLogsMaskIDsNoBodyNoSecrets: the log of the bot `run` builds (app.Build,
// with the logger every command uses, mask.JSONLogger), at debug level,
// through a spam post's whole path (decision, delete with a WhatsApp error
// that quotes the IDs, removal, ban, the report to the admin chat with a
// Telegram failure that quotes the token), a clean post, a /reload refused
// for a config error that names a community, and a [Ban] on a report whose
// community was then removed from the config, is structured JSON; every
// decision names its rule and config hash; no phone number, LID or group ID
// appears unmasked, in the log or in the admin chat's replies; no message text
// appears above debug; and neither the Telegram token nor the heartbeat URL
// appears at all. The /reload and the [Ban] reach the bot through the
// getUpdates long poll, the way Telegram delivers them (the fake hands out
// only the update types the bot asked for).
func TestLogsMaskIDsNoBodyNoSecrets(t *testing.T) {
	const (
		spamBody  = "cheap crypto signals https://example.org/join zqwobble"
		cleanBody = "see you all at the meetup on friday zqflump"
		watchBody = "crypto: inbox me zqglimp"
		pingMark  = "hb-7f3e-check-path"
		adminUser = int64(501)
	)
	api := telegramtest.New(t, nil)
	extra := modtest.Config + "heartbeat_url: https://hc.example.org/ping/" + pingMark + "\n"
	holder, cfgPath := configtest.Holder(t, extra)
	cfg, hash := holder.Current().Config, holder.Current().Hash // the posts are decided on this one
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	log := mask.JSONLogger(&logs, slog.LevelDebug)
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

	// The admin chat, through the real update polling: a watch-only match gets
	// a report with [Ban]; a /reload of a config whose error names the
	// community by its group ID is refused; a /reload that drops the community
	// is taken; then [Ban] on the report fails naming the community. Both
	// errors leave their call sites unmasked; only the logger and the reply
	// sink mask them.
	api.SetAdmin(adminUser, true)
	from := map[string]any{"id": adminUser, "is_bot": false, "first_name": "Ann"}
	inChat := map[string]any{"id": telegramtest.ChatID, "type": "supergroup"}
	posted := func(sub string) string {
		for _, p := range api.Posted() {
			if strings.Contains(p.Params["text"], sub) {
				return p.Params["text"]
			}
		}
		return ""
	}
	state := func() string { return fmt.Sprintf("Telegram posts %+v; log:\n%s", api.Posted(), logs.String()) }
	watch := &client.Message{Chat: modtest.G1, Sender: modtest.Member, ID: "M3", TargetID: "M3", Time: now,
		Fields: []client.Field{{Name: "body", Text: watchBody, Match: watchBody}}}
	if err := fake.Deliver(watch); err != nil {
		t.Fatal(err)
	}
	// The report is pressed as soon as it is stored: the admin chat posts at
	// Telegram's pace (one routine post per 3s), and the press needs no post.
	var banData string
	waitFor(t, "the watch-only report with [Ban]", func() bool {
		for id := int64(1); id <= 20; id++ {
			if r, ok, err := st.Report(ctx, id); err == nil && ok && r.Kind == ledger.KindWouldHaveActed &&
				slices.Contains(r.Buttons, ledger.ButtonBan) {
				banData = "ban:" + strconv.FormatInt(id, 10)
				return true
			}
		}
		return false
	}, state)
	reload := func(cfg string) {
		t.Helper()
		if err := os.WriteFile(cfgPath, []byte(configtest.Base+cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		api.QueueUpdate(map[string]any{"message": map[string]any{"message_id": 9000, "date": 1, "chat": inChat,
			"from": from, "text": "/reload"}})
	}
	community := "  \"" + string(modtest.Community) + "\":\n    name: community a\n"
	if !strings.Contains(extra, community) {
		t.Fatalf("premise broken: the test config has no %q", community)
	}
	reload(strings.Replace(extra, community, community+"    mode: banana\n", 1))
	waitFor(t, "the refused /reload reply", func() bool { return posted("REJECTED") != "" }, state)
	reload(strings.Replace(extra, community, "", 1))
	waitFor(t, "the accepted /reload reply", func() bool { return posted(" loaded") != "" }, state)
	api.QueueUpdate(map[string]any{"callback_query": map[string]any{"id": "q1", "from": from, "data": banData,
		"message": map[string]any{"message_id": 1, "date": 1, "chat": inChat}}})
	waitFor(t, "the [Ban] reply", func() bool { return posted("no longer a configured community") != "" }, state)
	if r := posted("REJECTED"); !strings.Contains(r, "communities.group…0999.mode") {
		t.Errorf("the refused /reload reply does not name the community masked: %q", r)
	}
	if r := posted("no longer a configured community"); !strings.Contains(r, "Could not do that: group…0999 is no longer") {
		t.Errorf("the [Ban] reply does not name the community masked: %q", r)
	}
	answered := api.Requests("answerCallbackQuery")
	if len(answered) != 1 || !strings.Contains(answered[0].Params["text"], "group…0999 is no longer") {
		t.Errorf("the [Ban] press was answered %+v", answered)
	}
	for _, p := range api.Requests() {
		for _, v := range p.Params {
			if strings.Contains(v, string(modtest.Community[:strings.Index(string(modtest.Community), "@")])) {
				t.Errorf("an unmasked community ID sent to Telegram (%s): %s", p.Method, v)
			}
		}
	}
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
	decisions, retried, refused, rejected, banFailed := 0, false, false, false, false
	for _, l := range lines {
		switch l.fields["msg"] {
		case "decision":
			decisions++
			if rule := l.fields["rule"]; (rule != "pitch" && rule != "lure") || l.fields["config"] != "v"+hash {
				t.Errorf("a decision without its rule and config hash: %s", l.raw)
			}
		case "action failed; retrying":
			retried = true
		case "config reload rejected":
			rejected = strings.Contains(l.raw, "communities.group…0999.mode")
		case "admin action failed":
			banFailed = strings.Contains(l.raw, "group…0999 is no longer a configured community")
		}
		if strings.Contains(l.raw, "bad gateway") {
			refused = true
		}
	}
	// The premises: the paths whose logging is checked below did run.
	if decisions == 0 || !retried || !refused || !rejected || !banFailed {
		t.Fatalf("decision logged %d times, WhatsApp retry logged %v, Telegram refusal logged %v, "+
			"refused reload logged masked %v, failed [Ban] logged masked %v:\n%s",
			decisions, retried, refused, rejected, banFailed, logs.String())
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
		if l.fields["level"] != "DEBUG" && (strings.Contains(l.raw, "zqwobble") || strings.Contains(l.raw, "zqflump") ||
			strings.Contains(l.raw, "zqglimp")) {
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

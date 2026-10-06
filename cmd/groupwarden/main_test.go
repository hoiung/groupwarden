package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/store/sessiontest"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

type testEnv struct {
	*env
	out, errb *syncBuffer
	opens     atomic.Int32
	dir       string
	cfgPath   string
	ctx       context.Context
	cancel    context.CancelFunc
}

// syncBuffer is a bytes.Buffer safe for a command running in a goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	s.b.Reset()
	s.mu.Unlock()
}

// newTestEnv builds an env over a temp dir with a valid config and the fake adapter.
func newTestEnv(t *testing.T, fake *clienttest.Fake) *testEnv {
	t.Helper()
	dir := t.TempDir()
	te := &testEnv{out: &syncBuffer{}, errb: &syncBuffer{}, dir: dir}
	te.ctx, te.cancel = context.WithCancel(context.Background())
	t.Cleanup(te.cancel)
	te.env = &env{
		stdout: te.out, stderr: te.errb, getenv: func(string) string { return "" }, now: time.Now,
		openAdapter: func(_ context.Context, cfg *config.Config, _ *slog.Logger) (client.Adapter, error) {
			te.opens.Add(1)
			// The real adapter creates the session store when it opens.
			sessiontest.Create(t, cfg.WhatsmeowDB())
			return fake, nil
		},
		deviceOf: func(path string) (uint64, error) {
			if strings.HasPrefix(path, filepath.Join(dir, "backup")) {
				return 2, nil
			}
			return 1, nil
		},
		signals:        func() (context.Context, context.CancelFunc) { return context.WithCancel(te.ctx) },
		connectTimeout: 3 * time.Second,
	}
	te.cfgPath = te.writeConfig(t, "")
	return te
}

func (te *testEnv) writeConfig(t *testing.T, extra string) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	body := "data_dir: data\nsecrets_file: secrets.env\ndeploy_key_file: deploy_key\n" +
		"backup:\n  target_dir: backup\n  age_recipient: " + id.Recipient().String() + "\n" + extra
	p := filepath.Join(te.dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (te *testEnv) cmd(args ...string) int {
	te.out.Reset()
	te.errb.Reset()
	return te.run(append([]string{args[0], "--config", te.cfgPath}, args[1:]...))
}

// A BotFather-shaped token built at run time, so no token-shaped literal sits in the repo.
var fakeBot = "1234567890:AA" + strings.Repeat("Bc9_-dEf", 4) + "x"

func (te *testEnv) provision(t *testing.T) {
	t.Helper()
	for _, d := range []string{"data", "backup"} {
		if err := os.MkdirAll(filepath.Join(te.dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	te.writeFile(t, "secrets.env", config.KeyTelegramBot+"="+fakeBot+"\n"+config.KeyTelegramChatID+"=-1001234567890\n", 0o600)
	te.writeFile(t, "deploy_key", "-----BEGIN OPENSSH "+"PRIVATE KEY-----\nAAAA\n-----END OPENSSH "+"PRIVATE KEY-----\n", 0o600)
}

func (te *testEnv) writeFile(t *testing.T, name, body string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(te.dir, name)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

var items = []string{"data-dir", "config-dir", "telegram-token", "telegram-chat-id", "age-public-key", "deploy-key", "backup-target"}

func lineFor(out, item string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "OK "+item) || strings.HasPrefix(l, "MISSING "+item) {
			return l
		}
	}
	return ""
}

func TestCheckSecretsListsEachItem(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	if code := te.cmd("check", "--secrets"); code != 0 {
		t.Fatalf("fully provisioned: exit %d\n%s", code, te.out)
	}
	for _, item := range items {
		if l := lineFor(te.out.String(), item); l != "OK "+item {
			t.Fatalf("item %s: line %q\n%s", item, l, te.out)
		}
	}
	if !strings.HasPrefix(te.out.String(), "OK config v") {
		t.Fatalf("no config line:\n%s", te.out)
	}
	// Each input, broken on its own, is listed as MISSING and fails the check.
	cases := []struct {
		item    string
		breakFn func()
		fix     func()
	}{
		{"telegram-token", func() { te.writeFile(t, "secrets.env", config.KeyTelegramChatID+"=-1001234567890\n", 0o600) }, te.provisionFn(t)},
		{"telegram-chat-id", func() {
			te.writeFile(t, "secrets.env", config.KeyTelegramBot+"="+fakeBot+"\n"+config.KeyTelegramChatID+"=12345\n", 0o600)
		}, te.provisionFn(t)},
		{"deploy-key", func() { _ = os.Chmod(filepath.Join(te.dir, "deploy_key"), 0o644) }, te.provisionFn(t)},
		{"data-dir", func() { _ = os.RemoveAll(filepath.Join(te.dir, "data")) }, te.provisionFn(t)},
		{"backup-target", func() {
			te.deviceOf = func(string) (uint64, error) { return 1, nil } // same disk as the data dir
		}, func() {
			te.deviceOf = func(p string) (uint64, error) {
				if strings.HasPrefix(p, filepath.Join(te.dir, "backup")) {
					return 2, nil
				}
				return 1, nil
			}
		}},
	}
	for _, c := range cases {
		c.breakFn()
		code := te.cmd("check", "--secrets")
		if code != 1 || !strings.HasPrefix(lineFor(te.out.String(), c.item), "MISSING "+c.item) {
			t.Fatalf("%s broken: exit %d\n%s", c.item, code, te.out)
		}
		for _, other := range items {
			if other != c.item && lineFor(te.out.String(), other) != "OK "+other {
				t.Fatalf("%s broken also failed %s:\n%s", c.item, other, te.out)
			}
		}
		c.fix()
	}
	// A secrets file others can read fails both Telegram items.
	_ = os.Chmod(filepath.Join(te.dir, "secrets.env"), 0o644)
	te.cmd("check", "--secrets")
	for _, item := range []string{"telegram-token", "telegram-chat-id"} {
		if !strings.Contains(lineFor(te.out.String(), item), "readable by others") {
			t.Fatalf("world-readable secrets: %s line %q", item, lineFor(te.out.String(), item))
		}
	}
	te.provision(t)
	// A private age key pasted as the recipient is refused with a clear reason.
	id, _ := age.GenerateX25519Identity()
	te.cfgPath = te.writeConfigAge(t, id.String())
	te.cmd("check", "--secrets")
	if l := lineFor(te.out.String(), "age-public-key"); !strings.Contains(l, "PRIVATE key") {
		t.Fatalf("private key as recipient: %q", l)
	}
}

func (te *testEnv) provisionFn(t *testing.T) func() { return func() { te.provision(t) } }

func (te *testEnv) writeConfigAge(t *testing.T, recipient string) string {
	t.Helper()
	body := "data_dir: data\nsecrets_file: secrets.env\ndeploy_key_file: deploy_key\nbackup:\n  target_dir: backup\n  age_recipient: " + recipient + "\n"
	p := filepath.Join(te.dir, "config2.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func setStatus(t *testing.T, te *testEnv, kv map[string]string, at time.Time) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(te.dir, "data", "groupwarden.db"), store.Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SetStatus(context.Background(), kv); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestHealthcheckExitCodes(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	if code := te.cmd("healthcheck"); code != 1 || !strings.Contains(te.out.String(), "running: no") {
		t.Fatalf("no database: exit %d\n%s", code, te.out)
	}
	now := time.Now()
	healthy := map[string]string{store.StatusHeartbeat: "", store.StatusConnected: "1", store.StatusDeaf: "0", store.StatusConfigHash: "abc123def456",
		store.StatusTelegramOK: "1"}
	st := setStatus(t, te, healthy, now)
	if code := te.cmd("healthcheck"); code != 0 {
		t.Fatalf("healthy: exit %d\n%s", code, te.out)
	}
	for _, want := range []string{"running: yes", "connected: yes", "deaf: no", "config: vabc123def456", "telegram: ok", "paused: no"} {
		if !strings.Contains(te.out.String(), want) {
			t.Fatalf("healthy output lacks %q:\n%s", want, te.out)
		}
	}
	// Paused is shown but does not make the bot unhealthy.
	if err := st.SetPause(context.Background(), store.Pause{Source: store.SourceExtraCompanion, Scope: store.ScopeRemoveBan, Reason: "1 other linked device", Since: now}); err != nil {
		t.Fatal(err)
	}
	if code := te.cmd("healthcheck"); code != 0 || !strings.Contains(te.out.String(), "paused: removals and bans") {
		t.Fatalf("paused: exit %d\n%s", code, te.out)
	}
	// Each change makes the bot unhealthy and prints why.
	unhealthy := []struct {
		change map[string]string
		line   string
	}{
		{map[string]string{store.StatusConnected: "0"}, "connected: no"},
		{map[string]string{store.StatusDeaf: "1"}, "deaf: yes"},
		{map[string]string{store.StatusConfigHash: ""}, "config: not loaded"},
		{map[string]string{store.StatusTelegramOK: "0"}, "telegram: REFUSED (marked unhealthy at "},
		{map[string]string{store.StatusTelegramOK: ""}, "telegram: not reached yet"},
	}
	for _, c := range unhealthy {
		kv := map[string]string{}
		for k, v := range healthy {
			kv[k] = v
		}
		for k, v := range c.change {
			kv[k] = v
		}
		setStatus(t, te, kv, now)
		if code := te.cmd("healthcheck"); code != 1 || !strings.Contains(te.out.String(), c.line) {
			t.Fatalf("%v: exit %d, want 1 and %q\n%s", c.change, code, c.line, te.out)
		}
	}
	// A stale heartbeat (run died) is unhealthy even if the last state was good.
	setStatus(t, te, healthy, now)
	te.now = func() time.Time { return now.Add(5 * time.Minute) }
	if code := te.cmd("healthcheck"); code != 1 || !strings.Contains(te.out.String(), "running: no") {
		t.Fatalf("stale heartbeat: exit %d\n%s", code, te.out)
	}
}

func TestBootRefusesInvalidConfig(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	cases := map[string]string{
		"missing file":     "",
		"unknown key":      "surprise: 1\n",
		"replay above 47h": "act_on_replay_max_age: 48h\n",
		"duplicate key":    "deafness_alert_hours: 6\ndeafness_alert_hours: 7\n",
	}
	for name, extra := range cases {
		if name == "missing file" {
			te.cfgPath = filepath.Join(te.dir, "absent.yaml")
		} else {
			te.cfgPath = te.writeConfig(t, extra)
		}
		code := te.cmd("run")
		if code == 0 || !strings.Contains(te.errb.String(), "refusing to start") {
			t.Fatalf("%s: exit %d stderr %q", name, code, te.errb)
		}
	}
	if n := te.opens.Load(); n != 0 {
		t.Fatalf("WhatsApp opened %d times for a refused config", n)
	}
	if _, err := os.Stat(filepath.Join(te.dir, "data", "groupwarden.lock")); !os.IsNotExist(err) {
		t.Fatal("a refused config still took the data-dir lock")
	}
}

func TestGroupsListsIDs(t *testing.T) {
	fake := &clienttest.Fake{Groups: []client.Group{
		{JID: "99999000000333@g.us", Name: "Our Community", IsCommunity: true},
		{JID: "99999000000111@g.us", Name: "General", Parent: "99999000000333@g.us"},
		{JID: "99999000000222@g.us", Name: "Announcements", Parent: "99999000000333@g.us", IsAnnouncement: true},
		{JID: "99999000000777@g.us", Name: "Standalone"},
	}}
	te := newTestEnv(t, fake)
	if code := te.cmd("groups"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.errb)
	}
	want := "community 99999000000333@g.us \"Our Community\"\n" +
		"  group 99999000000222@g.us \"Announcements\" (announcement)\n" +
		"  group 99999000000111@g.us \"General\"\n" +
		"group 99999000000777@g.us \"Standalone\"\n"
	if te.out.String() != want {
		t.Fatalf("output\n%s\nwant\n%s", te.out, want)
	}
	if fake.Count("Connect") != 1 || fake.Count("Disconnect") != 1 || fake.Count("Close") != 1 {
		t.Fatalf("calls %v", fake.Calls())
	}
	lock, err := app.AcquireLock(filepath.Join(te.dir, "data"))
	if err != nil {
		t.Fatalf("lock not released after groups: %v", err)
	}
	lock.Release()
}

func TestResolveLinkDoesNotJoin(t *testing.T) {
	fake := &clienttest.Fake{Invites: map[string]client.Group{
		"INVITECODE": {JID: "99999000000111@g.us", Name: "General", Parent: "99999000000333@g.us"},
	}}
	te := newTestEnv(t, fake)
	if code := te.cmd("resolve-link", "https://chat.whatsapp.com/INVITECODE?mode=r_c"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.errb)
	}
	want := "group 99999000000111@g.us \"General\"\ncommunity 99999000000333@g.us\n"
	if te.out.String() != want {
		t.Fatalf("output %q, want %q", te.out, want)
	}
	if fake.Count("InviteInfo INVITECODE") != 1 || fake.Count("JoinWithLink") != 0 || fake.Count("JoinLinkedGroup") != 0 {
		t.Fatalf("calls %v: a lookup must never join", fake.Calls())
	}
	// Not an invite link: refused before connecting.
	before := fake.Count("Connect")
	if code := te.cmd("resolve-link", "https://example.org/INVITECODE"); code == 0 || fake.Count("Connect") != before {
		t.Fatalf("bad link: exit %d, connects %d", code, fake.Count("Connect")-before)
	}
}

func TestSingleInstanceLock(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	lock, err := app.AcquireLock(filepath.Join(te.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"pair"}, {"run"}, {"groups"}, {"resolve-link", "INVITECODE"}} {
		code := te.cmd(args...)
		if code == 0 || !strings.Contains(te.errb.String(), "another groupwarden is running") {
			t.Fatalf("%v while locked: exit %d stderr %q", args, code, te.errb)
		}
	}
	if n := te.opens.Load(); n != 0 {
		t.Fatalf("WhatsApp opened %d times while another instance held the lock", n)
	}
	if _, err := app.AcquireLock(filepath.Join(te.dir, "data")); err != app.ErrAlreadyRunning {
		t.Fatalf("second lock in-process: %v", err)
	}
	lock.Release()
	if code := te.cmd("groups"); code != 0 {
		t.Fatalf("groups after release: exit %d %s", code, te.errb)
	}
}

func TestHealthcheckRunsBesideRun(t *testing.T) {
	fake := &clienttest.Fake{Groups: []client.Group{{JID: "99999000000111@g.us", Name: "General"}}}
	te := newTestEnv(t, fake)
	te.provision(t)
	// run needs the admin chat: point it at a fake Bot API. Healthcheck passes
	// only once the "started" report reached the chat.
	tg := telegramtest.New(t, nil)
	te.writeFile(t, "secrets.env", config.KeyTelegramBot+"="+tg.Token+"\n"+
		config.KeyTelegramChatID+"="+strconv.FormatInt(telegramtest.ChatID, 10)+"\n", 0o600)
	runEnv := *te.env
	runEnv.stdout, runEnv.stderr = &syncBuffer{}, &syncBuffer{}
	runEnv.telegramURL = tg.URL
	done := make(chan int, 1)
	go func() { done <- runEnv.run([]string{"run", "--config", te.cfgPath}) }()

	deadline := time.Now().Add(5 * time.Second)
	for te.cmd("healthcheck") != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("healthcheck never passed beside run:\n%s\nrun stderr:\n%s", te.out, runEnv.stderr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(te.out.String(), "telegram: ok") {
		t.Fatalf("healthy beside run without the admin chat:\n%s", te.out)
	}
	if posted := tg.Posted(); len(posted) == 0 || !strings.Contains(posted[0].Params["text"], "started") {
		t.Fatalf("first admin-chat post %v, want the started report", posted)
	}
	if code := te.cmd("check", "--secrets"); code != 0 {
		t.Fatalf("check beside run: exit %d\n%s", code, te.out)
	}
	if code := te.cmd("groups"); code == 0 || !strings.Contains(te.errb.String(), "another groupwarden is running") {
		t.Fatalf("a second WhatsApp command beside run: exit %d", code)
	}
	te.cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run exited %d on shutdown", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
}

func TestFatalExitCodePrinted(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	if code := te.run([]string{"fatal-exit-code"}); code != 0 || strings.TrimSpace(te.out.String()) != "78" {
		t.Fatalf("exit %d output %q", code, te.out)
	}
}

// TestCorpusTestExitCodes runs `corpus test` on the shipped example config and
// corpus, then on a corpus where a legit post would be deleted.
func TestCorpusTestExitCodes(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	example := filepath.Join("..", "..", "examples", "config.yaml")
	run := func(dir string) int {
		te.out.Reset()
		te.errb.Reset()
		return te.run([]string{"corpus", "test", "--config", example, "--corpus", dir})
	}
	if code := run(filepath.Join("..", "..", "tests", "corpus")); code != 0 {
		t.Fatalf("shipped corpus: exit %d\n%s%s", code, te.out, te.errb)
	}
	out := te.out.String()
	total := regexp.MustCompile(`(?m)^TOTAL spam=[1-9][0-9]* legit=[1-9][0-9]* legit_hits=0 spam_missed=0$`)
	classes := regexp.MustCompile(`(?m)^CLASS legit/(in-stock|fintech-job|blockchain-meetup|mention-reply|quoted-reply|newcomer-phone|lure-phrase-only) [1-9][0-9]* hits=0$`)
	if !total.MatchString(out) || len(classes.FindAllString(out, -1)) != 7 {
		t.Fatalf("shipped corpus output:\n%s", out)
	}

	bad := t.TempDir()
	for rel, body := range map[string]string{
		"spam/1.yaml":          "text: \"bitcoin signals t.me/example_signals\"\n",
		"legit/meetup/1.yaml":  "text: \"crypto meetup tonight https://example-meetup.test/1\"\n",
		"legit/general/2.yaml": "text: \"see you on Sunday\"\n",
	} {
		p := filepath.Join(bad, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code := run(bad); code != exitFail {
		t.Fatalf("legit hit: exit %d\n%s", code, te.out)
	}
	if !strings.Contains(te.out.String(), "FALSE-HIT legit/meetup ") || !strings.Contains(te.out.String(), "legit_hits=1 spam_missed=0") ||
		!strings.Contains(te.errb.String(), "1 legit sample(s) would be deleted") {
		t.Fatalf("legit hit output:\n%s%s", te.out, te.errb)
	}
	if code := te.run([]string{"corpus", "test", "--config", example}); code != exitUsage {
		t.Fatalf("no --corpus: exit %d", code)
	}
}

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/configsync/configsynctest"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// TestSyncConfigReloadsTheRunningBot drives the config sync end to end: the
// first sync swaps the repo's config in; the real bot runs on it; a new
// commit is pulled, checked, swapped in and the running bot asked to reload,
// and the bot posts "config v<hash> loaded" in the admin chat; a rejected
// commit and a failed pull each alert once per episode; every run records
// its result and time for /status and the overdue check.
func TestSyncConfigReloadsTheRunningBot(t *testing.T) {
	fake := &clienttest.Fake{Groups: []client.Group{{JID: "99999000000111@g.us", Name: "General"}}}
	te := newTestEnv(t, fake)
	te.provision(t)
	tg := telegramtest.New(t, nil)
	te.writeFile(t, "secrets.env", config.KeyTelegramBot+"="+tg.Token+"\n"+
		config.KeyTelegramChatID+"="+strconv.FormatInt(telegramtest.ChatID, 10)+"\n", 0o600)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	abs := func(name string) string { return filepath.Join(te.dir, name) }
	base := "data_dir: " + abs("data") + "\nsecrets_file: " + abs("secrets.env") + "\ndeploy_key_file: " + abs("deploy_key") +
		"\nbackup:\n  target_dir: " + abs("backup") + "\n  age_recipient: " + id.Recipient().String() + "\n"
	repo := configsynctest.New(t, map[string]string{"config.yaml": base})
	live := filepath.Join(te.dir, "live", "config", "config.yaml")
	hup := make(chan struct{}, 1)
	te.env.git = configsynctest.Git
	te.env.signal = func(pid int) error {
		if pid != os.Getpid() {
			return fmt.Errorf("asked to signal %d, not the running bot", pid)
		}
		hup <- struct{}{}
		return nil
	}
	sync := func() int {
		te.out.Reset()
		te.errb.Reset()
		return te.run([]string{"sync-config", "--config", live, "--repo", repo.Staging})
	}
	if code := sync(); code != exitOK || !strings.Contains(te.out.String(), "swapped in; the bot is not running") {
		t.Fatalf("first sync: exit %d\n%s%s", code, te.out, te.errb)
	}

	runCtx, stopRun := context.WithCancel(context.Background())
	defer stopRun()
	runEnv := *te.env
	runEnv.stdout, runEnv.stderr = &syncBuffer{}, &syncBuffer{}
	runEnv.telegramURL = tg.URL
	runEnv.signals = func() (context.Context, context.CancelFunc) { return context.WithCancel(runCtx) }
	runEnv.hangups = func() (<-chan struct{}, func()) { return hup, func() {} }
	done := make(chan int, 1)
	go func() { done <- runEnv.run([]string{"run", "--config", live}) }()
	for deadline := time.Now().Add(10 * time.Second); te.run([]string{"healthcheck", "--config", live}) != exitOK; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the bot never became healthy on the synced config:\n%s", runEnv.stderr)
		}
	}

	c2 := repo.Commit(map[string]string{"config.yaml": base + "mode: enforce\n"}, "enforce")
	if code := sync(); code != exitOK || te.out.String() != "ok "+c2[:12]+": swapped in; the bot was asked to reload\n" {
		t.Fatalf("second sync: exit %d\n%s%s", code, te.out, te.errb)
	}
	l, err := config.Read(live)
	if err != nil || l.Config.Mode != "enforce" {
		t.Fatalf("live config after the swap: %v", err)
	}
	loaded := "config v" + l.Hash + " loaded"
	for deadline := time.Now().Add(40 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		found := false
		for _, p := range tg.Posted() {
			found = found || strings.Contains(p.Params["text"], loaded)
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the admin chat never got %q: %+v", loaded, tg.Posted())
		}
	}
	stopRun()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}

	st, err := store.Open(context.Background(), filepath.Join(te.dir, "data", "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	reports := func(kind alert.Kind) []store.Report {
		t.Helper()
		rs, err := st.UnsentReportsOf(context.Background(), string(kind), 100)
		if err != nil {
			t.Fatal(err)
		}
		return rs
	}
	c3 := repo.Commit(map[string]string{"config.yaml": base + "mode: banana\n"}, "bad")
	for i := 0; i < 2; i++ {
		if code := sync(); code != exitFail || !strings.HasPrefix(te.out.String(), "REJECTED "+c3[:12]+": ") {
			t.Fatalf("rejected sync %d: exit %d\n%s", i, code, te.out)
		}
	}
	if rs := reports(alert.ConfigRejected); len(rs) != 1 || !rs[0].Priority ||
		!strings.HasPrefix(rs[0].Text, "Config commit "+c3[:12]+" was REJECTED by the check: ") {
		t.Fatalf("config_rejected reports %+v, want one", rs)
	}
	status, err := st.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r := status[store.StatusSyncResult].Value; !strings.HasPrefix(r, "REJECTED "+c3[:12]) || status[store.StatusSyncLastRun].Value == "" {
		t.Fatalf("status after a rejected sync: %q / %q", r, status[store.StatusSyncLastRun].Value)
	}
	if err := os.RemoveAll(repo.Remote); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if code := sync(); code != exitFail || !strings.HasPrefix(te.out.String(), "FAILED -: git pull: ") {
			t.Fatalf("failed pull %d: exit %d\n%s", i, code, te.out)
		}
	}
	if rs := reports(alert.SyncFailed); len(rs) != 1 || !rs[0].Priority || !strings.Contains(rs[0].Text, "tries again every 5 minutes") {
		t.Fatalf("sync_failed reports %+v, want one", rs)
	}
	if l, err := config.Read(live); err != nil || l.Config.Mode != "enforce" {
		t.Fatalf("the live config moved after a rejected commit and a failed pull: %v", err)
	}
	if code := te.run([]string{"sync-config", "--config", live}); code != exitUsage {
		t.Fatalf("no --repo: exit %d", code)
	}
}

// TestScheduleRendersSyncTimer: the sync timer's OnCalendar never waits
// longer than config_sync_minutes.
func TestScheduleRendersSyncTimer(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	for _, c := range []struct {
		extra, want string
	}{
		{"", "*-*-* *:00/5:00"},
		{"config_sync_minutes: 7\n", "*-*-* *:00/7:00"},
		{"config_sync_minutes: 60\n", "*-*-* 00/1:00:00"},
		{"config_sync_minutes: 150\n", "*-*-* 00/2:00:00"},
		{"config_sync_minutes: 1439\n", "*-*-* 00/23:00:00"},
		{"config_sync_minutes: 1440\n", "*-*-* 00:00:00"},
	} {
		te.cfgPath = te.writeConfig(t, c.extra)
		if code := te.cmd("schedule", "sync"); code != exitOK || te.out.String() != c.want+"\n" {
			t.Errorf("%q: exit %d %q, want %q", c.extra, code, te.out, c.want)
		}
	}
	for _, args := range [][]string{{"schedule"}, {"schedule", "backup"}} {
		if code := te.cmd(args...); code != exitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

// TestProductionGitAndSignal: the binary's own git runner runs git in the
// given directory with the extra environment and returns git's message on
// failure, and its reload signal is a SIGHUP to the given process.
func TestProductionGitAndSignal(t *testing.T) {
	e := productionEnv()
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := e.git(ctx, dir, nil, "rev-parse", "--git-dir"); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("git outside a repo: %v", err)
	}
	if _, err := e.git(ctx, dir, []string{"GIT_CONFIG_GLOBAL=" + os.DevNull}, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	email := "probe" + "@" + "example.invalid"
	out, err := e.git(ctx, dir, []string{"GIT_AUTHOR_NAME=probe", "GIT_AUTHOR_EMAIL=" + email}, "var", "GIT_AUTHOR_IDENT")
	if err != nil || !strings.HasPrefix(out, "probe <"+email+">") {
		t.Fatalf("env not passed to git: %q %v", out, err)
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	if err := e.signal(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-hup:
	case <-time.After(5 * time.Second):
		t.Fatal("no SIGHUP arrived")
	}
}

// TestScheduleAcceptedBySystemd: systemd itself parses the OnCalendar value
// rendered for every config_sync_minutes the schema allows.
func TestScheduleAcceptedBySystemd(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is not installed")
	}
	seen := map[string]bool{}
	args := []string{"calendar"}
	for m := 1; m <= 1440; m++ {
		if c := onCalendar(m); !seen[c] {
			seen[c] = true
			args = append(args, c)
		}
	}
	out, err := exec.Command(analyze, args...).CombinedOutput() // #nosec G204 -- the test's own values
	if err != nil {
		t.Fatalf("systemd-analyze calendar refused a rendered schedule: %v\n%s", err, out)
	}
	if n := strings.Count(string(out), "Normalized form:"); n != len(seen) {
		t.Fatalf("systemd parsed %d of %d schedules:\n%s", n, len(seen), out)
	}
}

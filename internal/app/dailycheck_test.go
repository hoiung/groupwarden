package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// armed waits until the supervisor's next monitor tick is due one interval
// from now, so a following h.tick moves it one tick at a time.
func (h *harness) armed() {
	h.t.Helper()
	h.eventually("the next tick armed", func() bool {
		return h.clock.hasWaiter(h.clock.Now().Add(monitorEvery), monitorEvery)
	})
}

// TestDailyCheckOncePerDay: the daily check is posted at daily_check_time
// (default 12:00) and not before, once a local day whatever the restarts, as
// a routine report with the date and "alive and working" while healthy; the
// days missed while the bot was down are not back-filled.
func TestDailyCheckOncePerDay(t *testing.T) {
	ctx := context.Background()
	h := start(t, &clienttest.Fake{SelfIDs: botSelf}, Settings{DeafAfter: 72 * time.Hour})
	h.setStatus(store.StatusTelegramOK, "1") // the admin chat took a message (the harness has no Telegram)
	h.setStatus(store.StatusDailyCheckDay, "2026-10-03")
	checks := func() []alert.Alert { return h.rec.OfKind(alert.DailyCheck) }

	h.clock.step(3*time.Hour+59*time.Minute, time.Hour) // 08:00 → 11:59
	h.armed()
	h.tick(monitorEvery) // 11:59:30
	settle()
	if n := len(checks()); n != 0 {
		t.Fatalf("%d daily checks before 12:00: %+v", n, checks())
	}
	h.tick(monitorEvery) // 12:00:00
	h.eventually("the 12:00 daily check", func() bool { return len(checks()) == 1 })
	want := "Daily check, Tuesday 6 October 2026: groupwarden is alive and working."
	if a := checks()[0]; a.Text != want || a.Priority || a.Kind.Priority() {
		t.Fatalf("daily check %+v, want a routine report %q", a, want)
	}
	if got := h.status(store.StatusDailyCheckDay); got != "2026-10-06" {
		t.Fatalf("daily_check_day = %q", got)
	}

	// A restarted bot (nothing marked in memory) reads the day posted from
	// the store, and later ticks the same day post nothing.
	h.app.marksMu.Lock()
	h.app.marks = nil
	h.app.marksMu.Unlock()
	h.app.checkDaily(ctx, h.clock.Now())
	h.clock.step(6*time.Hour, time.Hour)
	settle()
	if n := len(checks()); n != 1 {
		t.Fatalf("%d daily checks on one day", n)
	}

	// The next day, at 12:00 again.
	h.clock.step(17*time.Hour+59*time.Minute, time.Hour) // → 7 October 11:59
	h.armed()
	h.tick(monitorEvery)
	settle()
	if n := len(checks()); n != 1 {
		t.Fatalf("%d daily checks before 12:00 on the second day", n)
	}
	h.tick(monitorEvery)
	h.eventually("the second day's check", func() bool { return len(checks()) == 2 })
	if got := checks()[1].Text; got != "Daily check, Wednesday 7 October 2026: groupwarden is alive and working." {
		t.Fatalf("second day's check %q", got)
	}
}

// TestDailyCheckNamesWhatIsNotWorking: when the healthcheck rule fails at the
// check, the post says the bot is alive but names each part that is not
// working; a bot running past daily_check_time (here set to 07:30 by a
// reload) posts at its next tick.
func TestDailyCheckNamesWhatIsNotWorking(t *testing.T) {
	h := start(t, &clienttest.Fake{SelfIDs: botSelf, ConnectErr: errors.New("offline")}, Settings{})
	h.writeConfig("daily_check_time: \"07:30\"\n")
	h.sighup()
	h.eventually("reload", func() bool { return h.app.Config.Current().Config.DailyCheckTime == "07:30" })
	h.tick(monitorEvery)
	h.eventually("the daily check", func() bool { return len(h.rec.OfKind(alert.DailyCheck)) == 1 })
	want := "Daily check, Tuesday 6 October 2026: groupwarden is alive but not working fully: WhatsApp is not " +
		"connected; the admin chat has not taken a message yet."
	if a := h.rec.OfKind(alert.DailyCheck)[0]; a.Text != want || a.Priority {
		t.Fatalf("daily check %+v, want %q", a, want)
	}
}

// refusing is an admin chat that takes nothing; it counts the tries.
type refusing struct{ tries int }

func (r *refusing) Alert(context.Context, alert.Alert) error {
	r.tries++
	return errors.New("store full")
}

// TestDailyCheckLocalTimeAndRetry: daily_check_time is read in the node's time
// zone (the date too), and a post the admin chat did not take is not marked
// as done, so the next tick tries again.
func TestDailyCheckLocalTimeAndRetry(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "g.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	holder, _ := configtest.Holder(t, testSet)
	ref := &refusing{}
	a := &App{Store: st, Config: holder, Alerter: ref, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Zone: time.FixedZone("UTC+13", 13*3600)}
	noon := time.Date(2026, 10, 6, 23, 0, 0, 0, time.UTC) // 12:00 on 7 October at UTC+13
	a.checkDaily(ctx, noon.Add(-time.Minute))
	if ref.tries != 0 {
		t.Fatalf("posted at 11:59 local")
	}
	a.checkDaily(ctx, noon)
	if ref.tries != 1 {
		t.Fatalf("%d tries at 12:00 local, want 1", ref.tries)
	}
	status := func() string {
		s, err := st.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s[store.StatusDailyCheckDay].Value
	}
	if got := status(); got != "" {
		t.Fatalf("daily_check_day = %q after a post the admin chat refused", got)
	}
	rec := &alert.Recorder{}
	a.Alerter = rec
	a.checkDaily(ctx, noon.Add(30*time.Second))
	if got := rec.OfKind(alert.DailyCheck); len(got) != 1 || !strings.HasPrefix(got[0].Text, "Daily check, Wednesday 7 October 2026: groupwarden is ") {
		t.Fatalf("after the retry: %+v", got)
	}
	if got := status(); got != "2026-10-07" {
		t.Fatalf("daily_check_day = %q, want the local day", got)
	}
}

// TestDailyCheckReachesTheAdminChat: the bot `run` builds (app.Build) posts
// the daily check through the real admin chat: at 12:00 the Telegram API gets
// "Daily check, <date>: groupwarden is alive and working." as a plain routine
// message, once.
func TestDailyCheckReachesTheAdminChat(t *testing.T) {
	api := telegramtest.New(t, nil)
	holder, _ := configtest.Holder(t, modtest.Config)
	cfg := holder.Current().Config
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 10, 6, 11, 59, 30, 0, time.UTC)}
	var logs lockedBuffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	st, err := store.Open(context.Background(), cfg.StoreDB(), store.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := &clienttest.Fake{Groups: modtest.Groups(), SelfIDs: client.Self{Phone: modtest.BotPhone, LID: modtest.Bot}}
	a, err := Build(Parts{Adapter: fake, Store: st, Inbox: pipeline.NewInbox(st), Config: holder,
		Telegram: telegram.Options{Token: api.Token, ChatID: telegramtest.ChatID, ServerURL: api.URL}, // secret-allow (the fake API's run-time token)
		Alerts:   NewAlertSink(alert.Log{Logger: log}), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	a.Clock, a.Zone = clock, time.UTC
	// No pause between Telegram posts: the daily check is queued behind the
	// start-up reports.
	a.AdminChat.(*telegram.Chat).Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	texts := func() []string {
		var out []string
		for _, p := range api.Posted() {
			out = append(out, p.Params["text"])
		}
		return out
	}
	// The "started" message reaching Telegram is what marks the admin chat
	// reached (part of the health rule).
	waitFor(t, "connected and the started message posted", func() bool {
		s, err := st.Status(context.Background())
		return err == nil && s[store.StatusConnected].Value == "1" && s[store.StatusTelegramOK].Value == "1" &&
			a.Directory.IsLoaded()
	}, func() string { return fmt.Sprintf("posts %q", texts()) })
	waitFor(t, "the first tick armed", func() bool { return clock.hasWaiter(clock.Now().Add(monitorEvery), monitorEvery) },
		func() string { return "" })
	clock.Advance(monitorEvery) // 12:00:00
	want := "Daily check, Tuesday 6 October 2026: groupwarden is alive and working."
	waitFor(t, "the daily check in the admin chat", func() bool { return slices.Contains(texts(), want) },
		func() string { return fmt.Sprintf("posts %q\nlog:\n%s", texts(), logs.String()) })
	for _, p := range api.Posted() {
		if p.Params["text"] == want && p.Params["reply_markup"] != "" {
			t.Fatalf("the daily check carries buttons: %v", p.Params)
		}
	}
	clock.Advance(monitorEvery)
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range texts() {
		if strings.HasPrefix(s, "Daily check, ") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d daily checks posted: %q", n, texts())
	}
}

// TestOncePostsWhileTheStoreCannotWrite: with groupwarden.db unwritable (a
// full disk) the daily check and the day-10 phone escalation still reach the
// admin chat, sent around the store, and each exactly once however many
// ticks pass: the day and the episode are marked in memory when the store
// cannot keep them. Each alert is in the journal as it is raised.
func TestOncePostsWhileTheStoreCannotWrite(t *testing.T) {
	api := telegramtest.New(t, nil)
	holder, _ := configtest.Holder(t, modtest.Config)
	cfg := holder.Current().Config
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 10, 6, 11, 59, 30, 0, time.UTC)}
	var logs lockedBuffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	st, err := store.Open(context.Background(), cfg.StoreDB(), store.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The phone was last confirmed opened 10 days ago: the escalation is due.
	if err := st.SetStatus(context.Background(), map[string]string{store.StatusPhoneDone: strconv.FormatInt(
		clock.Now().Add(-10*24*time.Hour-time.Hour).UnixMilli(), 10)}); err != nil {
		t.Fatal(err)
	}
	fake := &clienttest.Fake{Groups: modtest.Groups(), SelfIDs: client.Self{Phone: modtest.BotPhone, LID: modtest.Bot}}
	a, err := Build(Parts{Adapter: fake, Store: st, Inbox: pipeline.NewInbox(st), Config: holder,
		Telegram: telegram.Options{Token: api.Token, ChatID: telegramtest.ChatID, ServerURL: api.URL}, // secret-allow (the fake API's run-time token)
		Alerts:   NewAlertSink(alert.Log{Logger: log}), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	a.Clock, a.Zone = clock, time.UTC
	a.AdminChat.(*telegram.Chat).Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	texts := func() []string {
		var out []string
		for _, p := range api.Posted() {
			out = append(out, p.Params["text"])
		}
		return out
	}
	count := func(prefix string) int {
		n := 0
		for _, s := range texts() {
			if strings.HasPrefix(s, prefix) {
				n++
			}
		}
		return n
	}
	waitFor(t, "connected and the started message posted", func() bool {
		s, err := st.Status(context.Background())
		return err == nil && s[store.StatusConnected].Value == "1" && s[store.StatusTelegramOK].Value == "1" &&
			a.Directory.IsLoaded()
	}, func() string { return fmt.Sprintf("posts %q", texts()) })
	// Every write fails from here on, as on a full or read-only disk.
	readOnly(t, st)
	const daily, escalation = "Daily check, Tuesday 6 October 2026: ", "Day 10 without [Done]"
	for i := 0; i < 4; i++ { // 12:00:00, 12:00:30, 12:01:00, 12:01:30
		waitFor(t, "the next tick armed", func() bool {
			return clock.hasWaiter(clock.Now().Add(monitorEvery), monitorEvery)
		}, func() string { return "" })
		clock.Advance(monitorEvery)
		waitFor(t, "the daily check and the escalation in the admin chat", func() bool {
			return count(daily) >= 1 && count(escalation) >= 1
		}, func() string { return fmt.Sprintf("posts %q\nlog:\n%s", texts(), logs.String()) })
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if d, e := count(daily), count(escalation); d != 1 || e != 1 {
		t.Fatalf("%d daily checks and %d escalations over 4 ticks, want 1 each: %q", d, e, texts())
	}
	for _, kind := range []alert.Kind{alert.DailyCheck, alert.PhoneEscalation} {
		if !strings.Contains(logs.String(), "msg=\"alert raised\" kind="+string(kind)) {
			t.Fatalf("no \"alert raised\" log line for %s:\n%s", kind, logs.String())
		}
	}
}

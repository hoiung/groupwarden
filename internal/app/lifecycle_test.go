package app

import (
	"context"
	"errors"
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
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/config/configtest"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/store"
)

// botSelf is the bot's own number and LID in these tests.
var botSelf = client.Self{Phone: "447700900123@s.whatsapp.net", LID: "99999000000999@lid"}

// writeConfig replaces the harness's config file (a reload reads it).
func (h *harness) writeConfig(extra string) {
	h.t.Helper()
	if err := os.WriteFile(h.cfgPath, []byte(configtest.Base+testSet+extra), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// sighup asks for a reload the way the SIGHUP handler does.
func (h *harness) sighup() { h.reload <- struct{}{} }

// unsent returns the stored, not yet delivered reports of kind.
func (h *harness) unsent(kind alert.Kind) []store.Report {
	h.t.Helper()
	reps, err := h.st.UnsentReportsOf(context.Background(), string(kind), 10)
	if err != nil {
		h.t.Fatal(err)
	}
	return reps
}

// TestLifecycleMessages: the admins hear started (version, config hash),
// config loaded / REJECTED on a reload, the bot demoted in and removed from a
// moderated group (priority reports), stopping on a clean stop, and a fatal
// disconnect with its kind and next step, with no "stopping" after it.
func TestLifecycleMessages(t *testing.T) {
	h := start(t, &clienttest.Fake{SelfIDs: botSelf}, Settings{})
	boot := h.app.Config.Current().Hash
	h.eventually("started", func() bool { return len(h.rec.OfKind(alert.Started)) == 1 })
	if a := h.rec.OfKind(alert.Started)[0]; !strings.Contains(a.Text, "groupwarden "+Version()+" started") ||
		!strings.Contains(a.Text, "config v"+boot) {
		t.Fatalf("started %q, want the version %q and config v%s", a.Text, Version(), boot)
	}

	// A valid config file is swapped in and announced; a broken one is
	// refused (REJECTED, priority) and the running config stays.
	h.writeConfig("deafness_alert_hours: 7\n")
	h.sighup()
	h.eventually("config loaded", func() bool { return len(h.rec.OfKind(alert.ConfigLoaded)) == 1 })
	loaded := h.app.Config.Current().Hash
	if a := h.rec.OfKind(alert.ConfigLoaded)[0]; loaded == boot || a.Text != "config v"+loaded+" loaded" {
		t.Fatalf("config loaded %q (hash %s → %s)", a.Text, boot, loaded)
	}
	h.writeConfig("deafness_alert_hours: nope\n")
	h.sighup()
	h.eventually("config rejected", func() bool { return len(h.rec.OfKind(alert.ConfigRejected)) == 1 })
	if a := h.rec.OfKind(alert.ConfigRejected)[0]; !a.Priority || !strings.Contains(a.Text, "REJECTED") ||
		h.app.Config.Current().Hash != loaded {
		t.Fatalf("config rejected %+v, running v%s (want still v%s)", a, h.app.Config.Current().Hash, loaded)
	}

	// The bot demoted, then removed, in a moderated group.
	at := h.clock.Now()
	for i, ch := range []*client.GroupChange{
		{Group: testGroup, Time: at, Actor: "99999000000444@lid", Demoted: []client.JID{botSelf.LID}},
		{Group: testGroup, Time: at.Add(time.Second), Actor: "99999000000444@lid", Left: []client.JID{botSelf.LID}},
	} {
		if err := h.fake.Deliver(ch); err != nil {
			t.Fatalf("change %d: %v", i, err)
		}
	}
	for kind, want := range map[alert.Kind]string{
		alert.BotDemoted: "no longer an admin in", alert.BotRemoved: "was removed from"} {
		h.eventually(string(kind), func() bool { return len(h.unsent(kind)) == 1 })
		if r := h.unsent(kind)[0]; !r.Priority || !strings.Contains(r.Text, want) || !strings.Contains(r.Text, "test-set") {
			t.Fatalf("%s report %+v, want a priority report naming the group's community", kind, r)
		}
	}

	// A clean stop is announced (priority, so it is flushed before exit).
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("clean stop returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if s := h.rec.OfKind(alert.Stopping); len(s) != 1 || !s[0].Priority || !strings.Contains(s[0].Text, "stopping (config v"+loaded+")") {
		t.Fatalf("stopping alerts %+v", s)
	}

	// A fatal disconnect names the kind and the next step; no "stopping".
	f := start(t, &clienttest.Fake{SelfIDs: botSelf}, Settings{})
	f.fake.Emit(client.Lifecycle{Kind: client.LoggedOut, Detail: "401: logged out from another device"})
	var err error
	select {
	case err = <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on a fatal disconnect")
	}
	var fe *FatalError
	if !errors.As(err, &fe) {
		t.Fatalf("Run returned %v, want a FatalError", err)
	}
	if a := f.rec.OfKind(alert.FatalDisconnect); len(a) != 1 || !a[0].Priority ||
		!strings.Contains(a[0].Text, string(client.LoggedOut)) || !strings.Contains(a[0].Text, client.LoggedOut.NextStep()) {
		t.Fatalf("fatal alerts %+v", a)
	}
	if s := f.rec.OfKind(alert.Stopping); len(s) != 0 {
		t.Fatalf("a fatal stop also sent %+v", s)
	}
}

// TestOverdueTimerAlert: the config sync or the backup timer not running for
// twice its interval (counted from this start when it never ran) raises one
// priority alert per episode; a run ends the episode.
func TestOverdueTimerAlert(t *testing.T) {
	ctx := context.Background()
	h := start(t, &clienttest.Fake{SelfIDs: botSelf}, Settings{})
	sync := time.Duration(h.app.Config.Current().Config.ConfigSyncMinutes) * time.Minute
	overdue := func(name string) []alert.Alert {
		var out []alert.Alert
		for _, a := range h.rec.OfKind(alert.Overdue) {
			if strings.HasPrefix(a.Text, "The "+name+" timer has not run") {
				out = append(out, a)
			}
		}
		return out
	}
	h.clock.step(2*sync-time.Minute, 30*time.Second)
	settle()
	if n := len(overdue("config sync")); n != 0 {
		t.Fatalf("%d sync alerts before twice the interval", n)
	}
	h.clock.step(2*time.Minute, 30*time.Second)
	h.eventually("sync overdue", func() bool { return len(overdue("config sync")) == 1 })
	if a := overdue("config sync")[0]; !a.Priority || !strings.Contains(a.Text, "last run never") {
		t.Fatalf("sync alert %+v", a)
	}
	h.clock.step(time.Hour, 5*time.Minute)
	settle()
	if n := len(overdue("config sync")); n != 1 {
		t.Fatalf("%d sync alerts in one episode", n)
	}

	// The timer runs: the episode ends. Silent again for twice the interval:
	// a new episode alerts again.
	if err := h.st.SetStatus(ctx, map[string]string{
		store.StatusSyncLastRun: strconv.FormatInt(h.clock.Now().UnixMilli(), 10)}); err != nil {
		t.Fatal(err)
	}
	// One tick at a time, each handled before the next: the alert comes at
	// the first tick past twice the interval, whatever the test's speed.
	h.tick(2*sync - time.Minute)
	if n := len(overdue("config sync")); n != 1 {
		t.Fatalf("alerted again %d within twice the interval of a run", n)
	}
	h.tick(2 * time.Minute)
	h.eventually("second sync episode", func() bool { return len(overdue("config sync")) == 2 })
	if a := overdue("config sync")[1]; !strings.Contains(a.Text, "last run "+(2*sync+monitorEvery).String()+" ago") {
		t.Fatalf("second sync alert %q", a.Text)
	}

	// The nightly backup: overdue after 48 hours without a run.
	if n := len(overdue("backup")); n != 0 {
		t.Fatalf("backup alert after %s", h.clock.Now().Sub(h.app.started))
	}
	h.clock.step(48*time.Hour-h.clock.Now().Sub(h.app.started)+time.Minute, time.Hour)
	h.eventually("backup overdue", func() bool { return len(overdue("backup")) == 1 })
	if a := overdue("backup")[0]; !a.Priority || !strings.Contains(a.Text, "runs every 24h0m0s") {
		t.Fatalf("backup alert %+v", a)
	}
}

// TestPhoneReminderEscalates: every 7 days since [Done] (or the first start)
// the admins are asked to open WhatsApp on the bot phone, with [Done]; on day
// 10 without [Done] it escalates (priority); [Done] starts a new week.
func TestPhoneReminderEscalates(t *testing.T) {
	ctx := context.Background()
	h := start(t, &clienttest.Fake{SelfIDs: botSelf}, Settings{})
	h.clock.step(time.Minute, 30*time.Second)
	h.eventually("phone_done set at first start", func() bool {
		st, err := h.st.Status(ctx)
		return err == nil && st[store.StatusPhoneDone].Value != ""
	})
	reminders := func() []alert.Alert { return h.rec.OfKind(alert.PhoneReminder) }
	escalations := func() []alert.Alert { return h.rec.OfKind(alert.PhoneEscalation) }

	h.clock.step(7*24*time.Hour-time.Hour, time.Hour)
	settle()
	if n := len(reminders()); n != 0 {
		t.Fatalf("%d reminders before day 7", n)
	}
	h.clock.step(2*time.Hour, 30*time.Minute)
	h.eventually("day 7 reminder", func() bool { return len(reminders()) == 1 })
	if a := reminders()[0]; a.Priority || !slices.Equal(a.Buttons, []string{ledger.ButtonDone}) ||
		!strings.Contains(a.Text, "open WhatsApp on the bot phone") {
		t.Fatalf("reminder %+v, want a routine report with [Done]", a)
	}
	h.clock.step(3*24*time.Hour, time.Hour)
	h.eventually("day 10 escalation", func() bool { return len(escalations()) == 1 })
	if a := escalations()[0]; !a.Priority || !slices.Equal(a.Buttons, []string{ledger.ButtonDone}) ||
		!strings.HasPrefix(a.Text, "Day 10 without [Done]") {
		t.Fatalf("escalation %+v, want a priority report with [Done] on day 10", a)
	}
	h.clock.step(24*time.Hour, time.Hour)
	settle()
	if len(reminders()) != 1 || len(escalations()) != 1 {
		t.Fatalf("repeated within one episode: %d reminders, %d escalations", len(reminders()), len(escalations()))
	}

	// [Done] (what the button writes): the next reminder is 7 days later.
	if err := h.st.SetStatus(ctx, map[string]string{
		store.StatusPhoneDone: strconv.FormatInt(h.clock.Now().UnixMilli(), 10)}); err != nil {
		t.Fatal(err)
	}
	h.clock.step(7*24*time.Hour-time.Hour, time.Hour)
	settle()
	if n := len(reminders()); n != 1 {
		t.Fatalf("reminded %d times within 7 days of [Done]", n)
	}
	h.clock.step(2*time.Hour, 30*time.Minute)
	h.eventually("reminder a week after [Done]", func() bool { return len(reminders()) == 2 })
	if n := len(escalations()); n != 1 {
		t.Fatalf("%d escalations a week after [Done]", n)
	}
}

// TestPhoneRemindersOnceWhileTheStoreCannotWrite: with the store unwritable
// the weekly reminder and the day-10 escalation each go to the admin chat
// once per episode, however many ticks check them.
func TestPhoneRemindersOnceWhileTheStoreCannotWrite(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "g.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if err := st.SetStatus(ctx, map[string]string{
		store.StatusPhoneDone: strconv.FormatInt(now.Add(-8*24*time.Hour).UnixMilli(), 10)}); err != nil {
		t.Fatal(err)
	}
	readOnly(t, st)
	holder, _ := configtest.Holder(t, testSet)
	rec := &alert.Recorder{}
	a := &App{Store: st, Config: holder, Alerter: rec, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for i := 0; i < 3; i++ {
		a.checkPhone(ctx, now.Add(time.Duration(i)*monitorEvery))
	}
	for i := 0; i < 3; i++ {
		a.checkPhone(ctx, now.Add(2*24*time.Hour+time.Duration(i)*monitorEvery))
	}
	if r, e := len(rec.OfKind(alert.PhoneReminder)), len(rec.OfKind(alert.PhoneEscalation)); r != 1 || e != 1 {
		t.Fatalf("%d reminders and %d escalations over 3 ticks each, want 1 each", r, e)
	}
}

// TestPhoneReminderNotMarkedWhenRefused: a reminder the admin chat did not
// take is not marked as sent, so the next tick tries again; once taken, it
// is not sent again.
func TestPhoneReminderNotMarkedWhenRefused(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "g.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if err := st.SetStatus(ctx, map[string]string{
		store.StatusPhoneDone: strconv.FormatInt(now.Add(-8*24*time.Hour).UnixMilli(), 10)}); err != nil {
		t.Fatal(err)
	}
	holder, _ := configtest.Holder(t, testSet)
	ref := &refusing{}
	a := &App{Store: st, Config: holder, Alerter: ref, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.checkPhone(ctx, now)
	a.checkPhone(ctx, now.Add(monitorEvery))
	if ref.tries != 2 {
		t.Fatalf("%d tries over 2 ticks the admin chat refused, want 2", ref.tries)
	}
	rec := &alert.Recorder{}
	a.Alerter = rec
	a.checkPhone(ctx, now.Add(2*monitorEvery))
	a.checkPhone(ctx, now.Add(3*monitorEvery))
	if n := len(rec.OfKind(alert.PhoneReminder)); n != 1 {
		t.Fatalf("%d reminders once the admin chat took one, want 1", n)
	}
}

// TestBootRejectionLogged: a config refused at boot, the bot running on the
// last good copy, is logged (masked, like every line) when the bot starts, as
// its alert is raised.
func TestBootRejectionLogged(t *testing.T) {
	var logs lockedBuffer
	reason := errors.New("line 3: communities." + string(testGroup) + ".mode must be one of shadow, enforce (got banana)")
	h := startWith(t, &clienttest.Fake{SelfIDs: botSelf}, Settings{}, func(a *App) {
		a.Log = mask.JSONLogger(&logs, nil)
		a.BootRejected = &config.Rejected{Reason: reason, Running: "55786e8be51a"}
	})
	h.eventually("the rejection alert", func() bool { return len(h.rec.OfKind(alert.ConfigRejected)) == 1 })
	got := logs.String()
	if !strings.Contains(got, `"msg":"config rejected at boot; running the last good config"`) ||
		!strings.Contains(got, "communities.group…0111.mode") || !strings.Contains(got, "still running v55786e8be51a") ||
		strings.Contains(got, "99999000000111") {
		t.Fatalf("the boot rejection is not logged, masked, with its reason:\n%s", got)
	}
}

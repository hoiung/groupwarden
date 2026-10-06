package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/config/configtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
)

// fakeClock fires After channels only when the test advances it.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, waiter{at: c.now.Add(d), ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var keep []waiter
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
	c.mu.Unlock()
}

func (c *fakeClock) hasWaiterAt(at time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.waiters {
		if w.at.Equal(at) {
			return true
		}
	}
	return false
}

// step advances in small increments so every timer due in d fires in order.
func (c *fakeClock) step(d, by time.Duration) {
	for moved := time.Duration(0); moved < d; moved += by {
		c.Advance(min(by, d-moved))
		time.Sleep(time.Millisecond)
	}
}

type harness struct {
	t       *testing.T
	app     *App
	fake    *clienttest.Fake
	clock   *fakeClock
	rec     *alert.Recorder
	st      *store.Store
	done    chan error
	cancel  context.CancelFunc // stops Run cleanly
	cfgPath string             // the config file a reload reads
	reload  chan struct{}      // what SIGHUP sends
}

// testSet makes the harness's test group a configured standalone set, so its
// events count as moderated (deafness only counts moderated groups).
const testSet = "communities:\n  test-set:\n    groups: [\"99999000000111@g.us\"]\n"

const testGroup client.JID = "99999000000111@g.us"

func start(t *testing.T, fake *clienttest.Fake, s Settings) *harness {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "g.db"), store.Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if s.DeafAfter == 0 {
		s.DeafAfter = 6 * time.Hour
	}
	if s.DisconnectAlert == 0 {
		s.DisconnectAlert = 15 * time.Minute
	}
	if s.CompanionCheckEvery == 0 {
		s.CompanionCheckEvery = time.Hour
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &alert.Recorder{}
	inbox := pipeline.NewInbox(st)
	holder, cfgPath := configtest.Holder(t, testSet)
	dir := &pipeline.Directory{}
	reload := make(chan struct{}, 1)
	a := &App{
		Adapter: fake, Store: st, Inbox: inbox, Alerter: rec, Log: log, Clock: clock, Settings: s,
		Config: holder, Directory: dir, Reload: reload,
		Worker: &pipeline.Worker{Store: st, Inbox: inbox, Config: holder, Log: log,
			Decider: &pipeline.Moderator{Store: st, Config: holder, Directory: dir, Log: log,
				Enforcer: &pipeline.Enforcer{Store: st, Config: holder, Directory: dir, Log: log}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := &harness{t: t, app: a, fake: fake, clock: clock, rec: rec, st: st, done: make(chan error, 1), cancel: cancel,
		cfgPath: cfgPath, reload: reload}
	go func() { h.done <- a.Run(ctx) }()
	h.eventually("first connect", func() bool { return fake.Count("Connect") >= 1 })
	if fake.ConnectErr == nil && fake.OnConnect == nil {
		h.connected()
	}
	return h
}

func (h *harness) eventually(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.app.mon.mu.Lock()
			connected, since := h.app.mon.connected, h.app.mon.since
			h.app.mon.mu.Unlock()
			h.t.Fatalf("timed out waiting for %s (calls %v; alerts %+v; monitor connected=%v since %s; now %s)",
				what, h.fake.Calls(), h.rec.All(), connected, since.Format(time.TimeOnly), h.clock.Now().Format(time.TimeOnly))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// settle gives the supervisor time to act on what was just delivered.
func settle() { time.Sleep(50 * time.Millisecond) }

// disconnect delivers a transient disconnect and waits until the supervisor
// has seen it, so the test clock does not run ahead of it.
func (h *harness) disconnect() {
	h.t.Helper()
	h.fake.Emit(client.Lifecycle{Kind: client.Disconnected})
	h.eventually("disconnect seen", func() bool { connected, _, _ := h.app.mon.snapshot(); return !connected })
}

// connected waits until the supervisor has processed a Connected event.
func (h *harness) connected() {
	h.t.Helper()
	h.eventually("connected", func() bool { connected, _, _ := h.app.mon.snapshot(); return connected })
}

func TestFatalStopsAndAlerts(t *testing.T) {
	for _, kind := range []client.LifecycleKind{client.LoggedOut, client.StreamReplaced, client.ClientOutdated, client.CATRefreshFailed, client.ConnectFailure} {
		h := start(t, &clienttest.Fake{}, Settings{})
		h.fake.Emit(client.Lifecycle{Kind: kind, Detail: "detail-" + string(kind)})
		var err error
		select {
		case err = <-h.done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: Run did not stop", kind)
		}
		var fe *FatalError
		if !errors.As(err, &fe) || fe.Kind != kind {
			t.Fatalf("%s: Run returned %v, want a FatalError", kind, err)
		}
		alerts := h.rec.OfKind(alert.FatalDisconnect)
		if len(alerts) != 1 || !alerts[0].Priority || !strings.Contains(alerts[0].Text, string(kind)) ||
			!strings.Contains(alerts[0].Text, kind.NextStep()) {
			t.Fatalf("%s: alerts %+v, want one priority alert naming the kind and next step", kind, alerts)
		}
		// No reconnect loop after a fatal disconnect.
		h.clock.Advance(time.Hour)
		settle()
		if n := h.fake.Count("Connect"); n != 1 {
			t.Fatalf("%s: %d connects, want 1", kind, n)
		}
	}
	if ExitFatal == 0 || ExitFatal == 1 || ExitFatal == 2 {
		t.Fatalf("ExitFatal %d collides with an ordinary failure code", ExitFatal)
	}
}

func TestTempBanWaitsForExpiryThenPaused(t *testing.T) {
	h := start(t, &clienttest.Fake{}, Settings{})
	ctx := context.Background()
	h.fake.Emit(client.Lifecycle{Kind: client.TemporaryBan, Detail: "101: too many messages", Expiry: 2 * time.Hour})
	h.eventually("temp ban alert", func() bool { return len(h.rec.OfKind(alert.TemporaryBan)) == 1 })
	al := h.rec.OfKind(alert.TemporaryBan)[0]
	if !al.Priority || !strings.Contains(al.Text, "2h0m0s") || !strings.Contains(al.Text, "PAUSED") {
		t.Fatalf("alert %+v, want priority with the expiry and the pause", al)
	}
	if h.fake.Count("Disconnect") < 1 {
		t.Fatal("bot did not disconnect for the ban")
	}
	// A disconnect notice during the ban must not start reconnecting.
	h.fake.Emit(client.Lifecycle{Kind: client.Disconnected})
	h.clock.step(119*time.Minute, time.Minute)
	settle()
	if n := h.fake.Count("Connect"); n != 1 {
		t.Fatalf("reconnected during the ban (%d connects)", n)
	}
	h.clock.step(2*time.Minute, time.Minute)
	h.eventually("reconnect after expiry", func() bool { return h.fake.Count("Connect") == 2 })
	if paused, why := h.st.PausedFor(ctx, store.ScopeRemoveBan); !paused || !strings.HasPrefix(why, store.SourceTempBan) {
		t.Fatalf("removals not paused after the ban: %v %q", paused, why)
	}
	if paused, why := h.st.PausedFor(ctx, store.ScopeAll); paused {
		t.Fatalf("deletes paused after the ban: %q", why)
	}
}

func TestBackoffCapped(t *testing.T) {
	b := newBackoff(backoffBase, backoffMax)
	var got []time.Duration
	for i := 0; i < 12; i++ {
		got = append(got, b.Next())
	}
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second,
		64 * time.Second, 128 * time.Second, 256 * time.Second, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delay %d = %s, want %s (sequence %v)", i, got[i], want[i], got)
		}
	}
	b.Reset()
	if d := b.Next(); d != backoffBase {
		t.Fatalf("after reset %s", d)
	}
	// In the supervisor: a failing connect retries on that schedule and no slower.
	h := start(t, &clienttest.Fake{ConnectErr: errors.New("network unreachable")}, Settings{})
	for i, d := range want[:10] {
		at := h.clock.Now().Add(d)
		h.eventually("retry scheduled", func() bool { return h.clock.hasWaiterAt(at) })
		h.clock.Advance(d - time.Millisecond)
		settle()
		if n := h.fake.Count("Connect"); n != i+1 {
			t.Fatalf("attempt %d came early (%d connects before %s)", i+2, n, d)
		}
		h.clock.Advance(time.Millisecond)
		h.eventually("retry", func() bool { return h.fake.Count("Connect") == i+2 })
	}
}

func TestExtraCompanionPauses(t *testing.T) {
	ctx := context.Background()
	h := start(t, &clienttest.Fake{}, Settings{CompanionCheckEvery: time.Hour})
	h.eventually("startup device list", func() bool { return h.fake.Count("LinkedDevices") >= 1 })
	if paused, _ := h.st.PausedFor(ctx, store.ScopeRemoveBan); paused {
		t.Fatal("paused with no extra device")
	}
	h.fake.SetDevices([]client.JID{"99999000000777:12@lid"})
	h.clock.Advance(time.Hour)
	h.eventually("companion alert", func() bool { return len(h.rec.OfKind(alert.ExtraCompanion)) == 1 })
	if a := h.rec.OfKind(alert.ExtraCompanion)[0]; !a.Priority || !strings.Contains(a.Text, "[Resume]") {
		t.Fatalf("alert %+v", a)
	}
	if paused, why := h.st.PausedFor(ctx, store.ScopeRemoveBan); !paused || !strings.HasPrefix(why, store.SourceExtraCompanion) {
		t.Fatalf("removals not paused: %v %q", paused, why)
	}
	if paused, _ := h.st.PausedFor(ctx, store.ScopeAll); paused {
		t.Fatal("deletes paused by an extra device")
	}
	// The same device at the next sweep is not reported again.
	h.clock.Advance(time.Hour)
	h.eventually("second sweep", func() bool { return h.fake.Count("LinkedDevices") >= 3 })
	settle()
	if n := len(h.rec.OfKind(alert.ExtraCompanion)); n != 1 {
		t.Fatalf("%d companion alerts, want 1", n)
	}
}

func TestDeafnessAlertOncePerEpisode(t *testing.T) {
	h := start(t, &clienttest.Fake{}, Settings{DeafAfter: 6 * time.Hour})
	deaf := func() int { return len(h.rec.OfKind(alert.Deafness)) }
	h.clock.step(5*time.Hour+59*time.Minute, 30*time.Minute)
	settle()
	if deaf() != 0 {
		t.Fatal("deafness alert before deafness_alert_hours")
	}
	h.clock.step(2*time.Minute, time.Minute)
	h.eventually("deafness alert", func() bool { return deaf() == 1 })
	h.clock.step(3*time.Hour, 30*time.Minute)
	settle()
	if deaf() != 1 {
		t.Fatalf("%d deafness alerts in one episode", deaf())
	}
	// An event ends the episode; a new silence is a new episode.
	if err := h.fake.Deliver(&client.Message{Chat: testGroup, Sender: "99999000000444@lid", ID: "E1", TargetID: "E1",
		Time: h.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	h.clock.step(5*time.Hour, 30*time.Minute)
	settle()
	if deaf() != 1 {
		t.Fatal("alerted again within 6h of an event")
	}
	h.clock.step(90*time.Minute, 30*time.Minute)
	h.eventually("second episode", func() bool { return deaf() == 2 })
	if a := h.rec.OfKind(alert.Deafness)[0]; !a.Priority {
		t.Fatal("deafness alert is not priority")
	}
}

func TestProlongedDisconnectAlert(t *testing.T) {
	h := start(t, &clienttest.Fake{}, Settings{DisconnectAlert: 15 * time.Minute})
	disc := func() int { return len(h.rec.OfKind(alert.ProlongedDisconnect)) }
	h.fake.SetConnectErr(errors.New("network unreachable"))
	h.disconnect()
	h.clock.step(14*time.Minute, 30*time.Second)
	settle()
	if disc() != 0 {
		t.Fatal("alert before disconnect_alert_minutes")
	}
	h.clock.step(2*time.Minute, 30*time.Second)
	h.eventually("prolonged disconnect alert", func() bool { return disc() == 1 })
	h.clock.step(30*time.Minute, time.Minute)
	settle()
	if disc() != 1 {
		t.Fatalf("%d alerts in one disconnect", disc())
	}
	// Reconnect, drop again: a new episode alerts again.
	h.fake.SetConnectErr(nil)
	h.clock.step(6*time.Minute, 30*time.Second)
	h.connected()
	h.fake.SetConnectErr(errors.New("network unreachable"))
	h.disconnect()
	h.clock.step(16*time.Minute, 30*time.Second)
	h.eventually("second episode", func() bool { return disc() == 2 })
}

// TestNothingDecidedBeforeGroupsLoad: when WhatsApp's group list fails at
// connect, no message is decided (it waits in the inbox), the admins get one
// priority alert, and the list is tried again every 30 seconds; once it
// loads, the waiting message is decided.
func TestNothingDecidedBeforeGroupsLoad(t *testing.T) {
	ctx := context.Background()
	fake := &clienttest.Fake{}
	fake.FailNext("JoinedGroups", errors.New("connection reset"), errors.New("connection reset"))
	h := start(t, fake, Settings{})
	h.eventually("the alert", func() bool { return len(h.rec.OfKind(alert.CoverageLost)) == 1 })
	if a := h.rec.OfKind(alert.CoverageLost)[0]; !a.Priority || !strings.Contains(a.Text, "could not list its WhatsApp groups") {
		t.Fatalf("alert %+v", a)
	}
	if err := fake.Deliver(&client.Message{Chat: testGroup, Sender: "99999000000444@lid", ID: "M1", TargetID: "M1",
		Time: h.clock.Now(), Fields: []client.Field{{Name: "body", Text: "hello", Match: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	inbox := func() int {
		n, err := h.st.InboxLen(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	settle()
	if n := inbox(); n != 1 {
		t.Fatalf("inbox %d: decided before the group list loaded", n)
	}
	h.clock.Advance(monitorEvery) // the list fails again: no second alert
	h.eventually("the second try", func() bool { return fake.Count("JoinedGroups") == 2 })
	settle()
	if n := inbox(); n != 1 || len(h.rec.OfKind(alert.CoverageLost)) != 1 {
		t.Fatalf("inbox %d, alerts %+v after the second failure", n, h.rec.OfKind(alert.CoverageLost))
	}
	h.clock.Advance(monitorEvery) // the list loads
	h.eventually("the message decided", func() bool { return inbox() == 0 })
	if n := len(h.rec.OfKind(alert.CoverageLost)); n != 1 {
		t.Fatalf("%d alerts, want 1", n)
	}
}

// TestJoinedGroupRefreshesGroups: when the bot joins a group (by itself,
// through /join, or added by an admin) WhatsApp's group list is read again
// at once, so the group and its community are known before the next
// reconcile interval.
func TestJoinedGroupRefreshesGroups(t *testing.T) {
	h := start(t, &clienttest.Fake{}, Settings{})
	h.eventually("the list at connect", func() bool { return h.fake.Count("JoinedGroups") == 1 })
	h.fake.SetGroups([]client.Group{{JID: testGroup, Name: "test"}})
	if err := h.fake.Deliver(&client.JoinedGroup{Group: testGroup, Time: h.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	h.eventually("the list read again", func() bool { return h.fake.Count("JoinedGroups") == 2 })
	h.eventually("the group known", func() bool { return h.app.Directory.Known(testGroup) })
}

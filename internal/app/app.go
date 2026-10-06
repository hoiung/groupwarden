// Package app runs groupwarden: it keeps the WhatsApp connection up, drains
// the inbox, and raises alerts for anything a human must know about.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hoiung/groupwarden/internal/action"
	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/reconcile"
	"github.com/hoiung/groupwarden/internal/store"
)

// ExitFatal is the process exit code when WhatsApp needs a human (re-pair,
// update, a refused config). systemd's RestartPreventExitStatus names it, so
// a restart loop never hammers WhatsApp with a dead session.
const ExitFatal = 78

// Fixed timings (not tunables).
const (
	backoffBase    = 2 * time.Second
	backoffMax     = 5 * time.Minute
	monitorEvery   = 30 * time.Second
	alertTimeout   = 10 * time.Second
	unknownBanWait = time.Hour // a temporary ban whose length WhatsApp did not say
)

// Settings are the config values the supervisor reads.
type Settings struct {
	DeafAfter           time.Duration // deafness_alert_hours
	DisconnectAlert     time.Duration // disconnect_alert_minutes
	CompanionCheckEvery time.Duration // reconcile.interval_minutes
}

// SettingsFrom reads the supervisor's settings from a config.
func SettingsFrom(c *config.Config) Settings {
	return Settings{
		DeafAfter:           time.Duration(c.DeafnessAlertHours) * time.Hour,
		DisconnectAlert:     time.Duration(c.DisconnectAlertMinutes) * time.Minute,
		CompanionCheckEvery: time.Duration(c.Reconcile.IntervalMinutes) * time.Minute,
	}
}

// FatalError ends Run when the connection cannot continue without a human.
type FatalError struct {
	Kind   client.LifecycleKind
	Detail string
}

func (e *FatalError) Error() string {
	return fmt.Sprintf("WhatsApp connection ended (%s: %s); next step: %s", e.Kind, e.Detail, e.Kind.NextStep())
}

// App wires the adapter, inbox, worker and alerts together.
type App struct {
	Adapter  client.Adapter
	Store    *store.Store
	Inbox    *pipeline.Inbox
	Worker   *pipeline.Worker
	Alerter  alert.Alerter
	Log      *slog.Logger
	Clock    Clock
	Settings Settings
	// Config is the running config; Reload requests (SIGHUP, and later the
	// admin chat's /reload) swap it whole.
	Config    *config.Holder
	Directory *pipeline.Directory
	Reload    <-chan struct{}
	// BootRejected: the config file was refused at boot and the last good
	// copy runs instead; reported once connected to the supervisor loop.
	BootRejected *config.Rejected

	// The moderation workers (each optional: nil is not started).
	Executor *action.Executor     // fires the outbox
	Reporter *ledger.Reporter     // delivers stored reports
	Media    *action.MediaFetcher // saves evidence attachments
	Purger   *ledger.Purger       // applies retention
	Sweep    *reconcile.Sweep     // removes banned members found in groups
	sweepNow chan struct{}

	mon *monitor

	mu        sync.Mutex
	queue     []client.Lifecycle
	lifecycle chan struct{} // signals that queue is non-empty
}

// sink is what the adapter's handler talks to.
type sink struct{ a *App }

func (s sink) Persist(ev client.Event) error {
	if err := s.a.Inbox.Persist(ev); err != nil {
		return err
	}
	if s.a.moderated(ev) {
		s.a.mon.onEvent(s.a.Clock.Now())
	}
	return nil
}

// moderated reports whether ev comes from a group of a configured community
// (only those count against deafness). It reads memory only.
func (a *App) moderated(ev client.Event) bool {
	var group client.JID
	switch e := ev.(type) {
	case *client.Message:
		group = e.Chat
	case *client.Undecryptable:
		group = e.Chat
	case *client.GroupChange:
		group = e.Group
	case *client.JoinedGroup:
		group = e.Group
	}
	return group != "" && a.Directory.Community(group, a.Config.Current().Rules) != ""
}

func (s sink) Lifecycle(l client.Lifecycle) {
	s.a.mu.Lock()
	s.a.queue = append(s.a.queue, l)
	s.a.mu.Unlock()
	select {
	case s.a.lifecycle <- struct{}{}:
	default:
	}
}

func (a *App) init() {
	a.mon = &monitor{deafAfter: a.Settings.DeafAfter, disconnectAfter: a.Settings.DisconnectAlert}
	a.lifecycle = make(chan struct{}, 1)
	a.sweepNow = make(chan struct{}, 1)
	a.Adapter.Events(sink{a})
}

// workers starts the moderation workers and returns a function that waits
// for them to stop (after ctx ends).
func (a *App) workers(ctx context.Context) func() {
	var wg sync.WaitGroup
	start := func(run func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run(ctx)
		}()
	}
	if a.Reporter != nil {
		start(a.Reporter.Run)
	}
	if a.Executor != nil {
		start(a.Executor.Run)
	}
	if a.Media != nil {
		start(a.Media.Run)
	}
	if a.Purger != nil {
		start(a.Purger.Run)
	}
	if a.Sweep != nil {
		start(a.sweeper)
	}
	return wg.Wait
}

// sweeper runs a sweep each time one is asked for (at connect and every
// reconcile interval), one at a time.
func (a *App) sweeper(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.sweepNow:
		}
		runID := a.Clock.Now().UTC().Format("20060102T150405.000")
		res, err := a.Sweep.Run(ctx, runID)
		if err != nil && ctx.Err() == nil {
			a.Log.Error("sweep failed", "err", mask.IDs(err.Error()))
		}
		a.Log.Info("sweep", "run", runID, "groups", res.Groups, "rate_limited", res.RateLimited, "errors", res.Errors)
	}
}

// requestSweep asks for a sweep (one pending at most).
func (a *App) requestSweep() {
	select {
	case a.sweepNow <- struct{}{}:
	default:
	}
}

func (a *App) nextLifecycle() (client.Lifecycle, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.queue) == 0 {
		return client.Lifecycle{}, false
	}
	l := a.queue[0]
	a.queue = a.queue[1:]
	return l, true
}

// Run connects and supervises until ctx ends (nil) or the connection fails
// fatally (*FatalError).
func (a *App) Run(ctx context.Context) error {
	a.init()
	// Actions a crash left at intended stay queued: the executor re-checks
	// and retries them, and the startup report lists them.
	if _, err := ledger.Recover(ctx, a.Store, a.Log); err != nil {
		return fmt.Errorf("recover the ledger: %w", err)
	}
	workerDone := make(chan struct{})
	wctx, stopWorker := context.WithCancel(ctx)
	go func() {
		defer close(workerDone)
		if err := a.Worker.Run(wctx); err != nil {
			a.Log.Error("inbox worker stopped", "err", err)
		}
	}()
	waitWorkers := a.workers(wctx)
	err := a.supervise(ctx)
	stopWorker()
	<-workerDone
	waitWorkers()
	a.writeStatus(context.WithoutCancel(ctx))
	return err
}

func (a *App) supervise(ctx context.Context) error {
	b := newBackoff(backoffBase, backoffMax)
	var retry <-chan time.Time
	var bannedUntil time.Time
	nextCompanionCheck := time.Time{}

	connect := func() {
		if err := a.Adapter.Connect(ctx); err != nil {
			d := b.Next()
			a.Log.Warn("connect failed; retrying", "in", d, "err", mask.IDs(err.Error()))
			a.mon.onDisconnected(a.Clock.Now())
			retry = a.Clock.After(d)
		}
	}
	a.mon.onDisconnected(a.Clock.Now())
	if a.BootRejected != nil {
		a.alert(ctx, alert.Alert{Kind: alert.ConfigRejected, Priority: true, Text: a.BootRejected.Error()})
	}
	connect()
	tick := a.Clock.After(monitorEvery)
	for {
		select {
		case <-ctx.Done():
			a.Adapter.Disconnect()
			return nil
		case <-a.Reload:
			a.reload(ctx)
		case <-retry:
			retry = nil
			connect()
		case <-tick:
			tick = a.Clock.After(monitorEvery)
			now := a.Clock.Now()
			for _, al := range a.mon.check(now) {
				a.alert(ctx, al)
			}
			if connected, _, _ := a.mon.snapshot(); connected && !now.Before(nextCompanionCheck) {
				nextCompanionCheck = now.Add(a.Settings.CompanionCheckEvery)
				a.refreshDirectory(ctx)
				a.checkCompanions(ctx)
				a.requestSweep()
			}
			a.writeStatus(ctx)
		case <-a.lifecycle:
			for {
				l, ok := a.nextLifecycle()
				if !ok {
					break
				}
				now := a.Clock.Now()
				switch {
				case l.Kind == client.Connected:
					b.Reset()
					bannedUntil = time.Time{}
					a.mon.onConnected(now)
					a.Log.Info("connected to WhatsApp")
					nextCompanionCheck = now.Add(a.Settings.CompanionCheckEvery)
					a.Directory.SetSelf(a.Adapter.Self())
					a.refreshDirectory(ctx)
					a.checkCompanions(ctx)
					a.requestSweep()
					a.writeStatus(ctx)
				case l.Kind == client.Disconnected:
					a.mon.onDisconnected(now)
					if now.Before(bannedUntil) || retry != nil {
						continue // already waiting out a ban or a retry
					}
					d := b.Next()
					a.Log.Warn("disconnected; reconnecting", "in", d, "detail", l.Detail)
					retry = a.Clock.After(d)
				case l.Kind == client.TemporaryBan:
					wait := l.Expiry
					if wait <= 0 {
						wait = unknownBanWait
					}
					bannedUntil = now.Add(wait)
					retry = a.Clock.After(wait)
					a.mon.onDisconnected(now)
					a.Adapter.Disconnect()
					a.pause(ctx, store.Pause{Source: store.SourceTempBan, Scope: store.ScopeRemoveBan,
						Reason: "WhatsApp temporarily banned the bot number: " + l.Detail, Since: now})
					a.alert(ctx, alert.Alert{Kind: alert.TemporaryBan, Priority: true, Text: fmt.Sprintf(
						"WhatsApp temporarily banned the bot number (%s). It stays disconnected until %s (in %s), then reconnects with removals and bans PAUSED until an admin presses [Resume]; deletes continue.",
						l.Detail, bannedUntil.UTC().Format("2006-01-02 15:04 MST"), wait.Round(time.Minute))})
				case l.Kind.Fatal():
					a.mon.onDisconnected(now)
					a.Adapter.Disconnect()
					fe := &FatalError{Kind: l.Kind, Detail: l.Detail}
					a.alert(ctx, alert.Alert{Kind: alert.FatalDisconnect, Priority: true, Text: fe.Error()})
					return fe
				default:
					a.Log.Error("unknown lifecycle event", "kind", string(l.Kind))
				}
			}
		}
	}
}

// alert delivers al with a deadline, so a stuck channel never blocks shutdown.
func (a *App) alert(ctx context.Context, al alert.Alert) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), alertTimeout)
	defer cancel()
	if err := a.Alerter.Alert(actx, al); err != nil {
		a.Log.Error("alert delivery failed", "kind", string(al.Kind), "err", err)
	}
}

func (a *App) pause(ctx context.Context, p store.Pause) {
	if err := a.Store.SetPause(context.WithoutCancel(ctx), p); err != nil {
		a.Log.Error("could not record pause", "source", p.Source, "err", err)
	}
}

// checkCompanions pauses removals and bans when a linked device appears on the
// bot number that was not reported before (deletes continue).
func (a *App) checkCompanions(ctx context.Context) {
	devices, err := a.Adapter.LinkedDevices(ctx)
	if err != nil {
		a.Log.Warn("could not list linked devices", "err", mask.IDs(err.Error()))
		return
	}
	status, err := a.Store.Status(ctx)
	if err != nil {
		a.Log.Error("read status", "err", err)
		return
	}
	seen := map[string]bool{}
	for _, d := range strings.Split(status[store.StatusCompanionsSeen].Value, ",") {
		if d != "" {
			seen[d] = true
		}
	}
	var current, fresh []string
	for _, d := range devices {
		current = append(current, string(d))
		if !seen[string(d)] {
			fresh = append(fresh, string(d))
		}
	}
	sort.Strings(current)
	if err := a.Store.SetStatus(ctx, map[string]string{store.StatusCompanionsSeen: strings.Join(current, ",")}); err != nil {
		a.Log.Error("write status", "err", err)
	}
	if len(fresh) == 0 {
		return
	}
	now := a.Clock.Now()
	reason := fmt.Sprintf("%d other linked device(s) on the bot number", len(devices))
	a.pause(ctx, store.Pause{Source: store.SourceExtraCompanion, Scope: store.ScopeRemoveBan, Reason: reason, Since: now})
	a.alert(ctx, alert.Alert{Kind: alert.ExtraCompanion, Priority: true, Text: reason +
		" (new: " + mask.IDs(strings.Join(fresh, ", ")) + "). Removals and bans are PAUSED; deletes continue. " +
		"If you linked it yourself press [Resume]; if not, unlink it on the bot phone now."})
}

// reload swaps in the config file again (whole, or not at all) and applies
// its supervisor settings; the admins hear "config v<hash> loaded" or why it
// was REJECTED.
func (a *App) reload(ctx context.Context) {
	l, err := a.Config.Reload()
	if err != nil {
		a.Log.Error("config reload rejected", "err", err)
		a.alert(ctx, alert.Alert{Kind: alert.ConfigRejected, Priority: true, Text: err.Error()})
		return
	}
	a.Settings = SettingsFrom(l.Config)
	a.mon.setThresholds(a.Settings.DeafAfter, a.Settings.DisconnectAlert)
	a.Log.Info("config reloaded", "config", "v"+l.Hash)
	a.alert(ctx, alert.Alert{Kind: alert.ConfigLoaded, Text: "config v" + l.Hash + " loaded"})
	a.writeStatus(ctx)
}

// refreshDirectory relearns every group's community and admins.
func (a *App) refreshDirectory(ctx context.Context) {
	groups, err := a.Adapter.JoinedGroups(ctx)
	if err != nil {
		a.Log.Warn("could not list groups", "err", mask.IDs(err.Error()))
		return
	}
	a.Directory.Update(groups)
}

// writeStatus records the run state for `healthcheck` and other commands.
func (a *App) writeStatus(ctx context.Context) {
	connected, deaf, last := a.mon.snapshot()
	kv := map[string]string{
		store.StatusHeartbeat:  "",
		store.StatusConnected:  boolStr(connected),
		store.StatusDeaf:       boolStr(deaf),
		store.StatusConfigHash: a.Config.Current().Hash,
	}
	if !last.IsZero() {
		kv[store.StatusLastEvent] = strconv.FormatInt(last.UnixMilli(), 10)
	}
	if err := a.Store.SetStatus(ctx, kv); err != nil && !errors.Is(err, context.Canceled) {
		a.Log.Error("write status", "err", err)
	}
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

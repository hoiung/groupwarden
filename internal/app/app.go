// Package app runs groupwarden: it keeps the WhatsApp connection up, drains
// the inbox, and raises alerts for anything a human must know about.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
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
	// backlogWait bounds how long a sweep waits for WhatsApp to say it has
	// delivered what it held while the bot was offline; backlogPoll is how
	// often the sweep then checks the worker has decided all of it.
	backlogWait = 10 * time.Minute
	backlogPoll = time.Second
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
	Adapter client.Adapter
	Store   *store.Store
	Inbox   *pipeline.Inbox
	Worker  *pipeline.Worker
	Alerter alert.Alerter
	Log     *slog.Logger
	Clock   Clock
	// Zone is the time zone of daily_check_time (nil: the node's local time
	// zone, from TZ or /etc/localtime).
	Zone     *time.Location
	Settings Settings
	// Config is the running config; Reload requests (SIGHUP, and later the
	// admin chat's /reload) swap it whole.
	Config    *config.Holder
	Directory *pipeline.Directory
	Reload    <-chan struct{}
	// BootRejected: the config file was refused at boot and the last good
	// copy runs instead; reported once connected to the supervisor loop.
	BootRejected *config.Rejected
	// NoSyncTimer: this deployment has no config sync timer (the Docker
	// image, where a changed config is reloaded with SIGHUP), so the sync is
	// not watched for overdue runs. `run` sets it from GROUPWARDEN_NO_SYNC_TIMER.
	NoSyncTimer bool

	// The moderation workers (each optional: nil is not started).
	Executor *action.Executor     // fires the outbox
	Media    *action.MediaFetcher // saves evidence attachments
	Purger   *ledger.Purger       // applies retention
	Sweep    *reconcile.Sweep     // removes banned members found in groups
	// AdminChat delivers reports and alerts and takes the admins' buttons
	// and commands (Controls); Admin is what those buttons write.
	AdminChat Runner
	Admin     *pipeline.Admin
	sweepNow  chan struct{}
	// refreshNow asks the supervisor to relearn the groups (the bot joined
	// one) and sweep.
	refreshNow chan struct{}
	// dirAlerted: the admins were told the group list does not load;
	// listStale: the last group list read failed, so it is tried again
	// every tick (supervisor goroutine only, both).
	dirAlerted, listStale bool
	// caughtUp is closed once WhatsApp has delivered what it held while the
	// bot was offline (a fresh one at each connect after that); backlogMark
	// is the newest inbox row then. Sweeps wait for both (readyToSweep).
	backlogMu   sync.Mutex
	caughtUp    chan struct{}
	backlogMark int64

	mon     *monitor
	started time.Time
	// overdue: for each outside timer alerted as overdue, the last run the
	// alert was about (one alert per episode).
	overdue map[string]time.Time
	// marks: the once-markers this run set (status key → value; see mark).
	marksMu sync.Mutex
	marks   map[string]string

	mu        sync.Mutex
	queue     []client.Lifecycle
	lifecycle chan struct{} // signals that queue is non-empty

	// reloadMu makes a reload (SIGHUP or /reload) one at a time;
	// settingsMu guards Settings, which a reload replaces.
	reloadMu   sync.Mutex
	settingsMu sync.Mutex
}

// Runner is a worker that runs until ctx ends.
type Runner interface {
	Run(ctx context.Context)
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
	if _, ok := ev.(*client.JoinedGroup); ok {
		// The worker learns the group from the event itself; the group list
		// is read again now too, and a sweep follows, not at the next
		// reconcile interval.
		select {
		case s.a.refreshNow <- struct{}{}:
		default:
		}
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
	a.overdue = map[string]time.Time{}
	a.lifecycle = make(chan struct{}, 1)
	a.sweepNow = make(chan struct{}, 1)
	a.refreshNow = make(chan struct{}, 1)
	a.caughtUp = make(chan struct{})
	// Nothing is decided before the bot knows its groups: a message matched
	// against an empty directory would count as unmoderated and be dropped.
	a.Worker.Ready = a.Directory.Loaded()
	// An action WhatsApp refused as the bot is not an admin marks that group
	// not covered at once.
	if a.Executor != nil && a.Sweep != nil {
		a.Executor.NotAdmin = a.Sweep.LostAdmin
	}
	// Queued actions wait while WhatsApp is not connected (a temporary ban
	// can last a day) and go out when it is again.
	if a.Executor != nil {
		a.Executor.Connected = func() bool {
			connected, _, _ := a.mon.snapshot()
			return connected
		}
	}
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
	if a.AdminChat != nil {
		start(a.AdminChat.Run)
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
	start(a.pinger) // reads heartbeat_url at each ping, so a reload can turn it on
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
		if !a.readyToSweep(ctx) {
			return
		}
		runID := a.Clock.Now().UTC().Format("20060102T150405.000")
		res, err := a.Sweep.Run(ctx, runID)
		if err != nil && ctx.Err() == nil {
			a.Log.Error("sweep failed", "err", mask.IDs(err.Error()))
		}
		a.Log.Info("sweep", "run", runID, "groups", res.Groups, "joins", res.Joins, "reports", res.Reports,
			"rate_limited", res.RateLimited, "errors", res.Errors)
	}
}

// readyToSweep holds a sweep until the group list has loaded (against an
// empty directory every group looks like one the bot is not in) and the
// worker has decided everything WhatsApp delivered from while the bot was
// offline: a sweep removal must not overtake an offline event that changes it
// (a human admin who re-added a banned member lifted the ban). It waits at
// most backlogWait for WhatsApp to say the backlog is delivered. It returns
// false when ctx ends.
func (a *App) readyToSweep(ctx context.Context) bool {
	start := a.Clock.Now()
	if !a.Directory.IsLoaded() {
		a.Log.Info("sweep waiting for the group list")
		select {
		case <-ctx.Done():
			return false
		case <-a.Directory.Loaded():
		}
	}
	a.backlogMu.Lock()
	delivered := a.caughtUp
	a.backlogMu.Unlock()
	select {
	case <-delivered:
	default:
		timeout := a.Clock.After(backlogWait)
		a.Log.Info("sweep waiting for WhatsApp to deliver the offline backlog", "at_most", backlogWait)
		select {
		case <-ctx.Done():
			return false
		case <-delivered:
		case <-timeout:
			a.Log.Warn("WhatsApp has not said it delivered the offline backlog; sweeping anyway", "waited", backlogWait)
			return true
		}
	}
	a.backlogMu.Lock()
	mark := a.backlogMark
	a.backlogMu.Unlock()
	for waiting := false; ; waiting = true {
		undecided, err := a.Store.InboxUndecidedThrough(ctx, mark)
		switch {
		case ctx.Err() != nil:
			return false
		case err != nil:
			// The sweep's own writes go to the same store and fail loudly.
			a.Log.Error("could not read the inbox; sweeping without waiting for the offline backlog", "err", err)
			return true
		case !undecided:
			if waited := a.Clock.Now().Sub(start); waited > 0 {
				a.Log.Info("group list loaded and offline backlog decided; sweeping", "waited", waited)
			}
			return true
		}
		if !waiting {
			a.Log.Info("sweep waiting for the worker to decide the offline backlog", "inbox_through", mark)
		}
		select {
		case <-ctx.Done():
			return false
		case <-a.Clock.After(backlogPoll):
		}
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
	a.started = a.Clock.Now()
	// Actions a crash left at intended stay queued: the executor re-checks
	// and retries them, and the startup report lists them.
	if _, err := ledger.Recover(ctx, a.Store, a.Log); err != nil {
		return fmt.Errorf("recover the ledger: %w", err)
	}
	hash := a.Config.Current().Hash
	zone, offset := a.started.In(a.zone()).Zone()
	a.Log.Info("daily check time zone", "zone", a.zone().String(), "abbrev", zone, "utc_offset_s", offset,
		"daily_check_time", a.Config.Current().Config.DailyCheckTime)
	var watched []string
	for _, t := range a.watchedTimers() {
		watched = append(watched, t.name)
	}
	a.Log.Info("watching outside timers", "timers", watched)
	a.alert(ctx, alert.Alert{Kind: alert.Started, Text: fmt.Sprintf("groupwarden %s started (config v%s).", Version(), hash)})
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
	if err == nil {
		// A clean stop (signal): the admins hear it before the process
		// exits (a fatal stop already sent its own alert).
		a.alert(ctx, alert.Alert{Kind: alert.Stopping, Priority: true, Text: fmt.Sprintf(
			"groupwarden %s stopping (config v%s). Reports not yet sent go out after the restart.", Version(),
			a.Config.Current().Hash)})
	}
	a.writeStatus(context.WithoutCancel(ctx))
	return err
}

func (a *App) supervise(ctx context.Context) error {
	b := newBackoff(backoffBase, backoffMax)
	var retry <-chan time.Time
	var bannedUntil time.Time
	nextCompanionCheck := time.Time{}

	connect := func() {
		// Re-armed before the connection starts, not at Connected: the
		// library does not order Connected and CaughtUp, so a CaughtUp that
		// overtakes its own Connected still releases this connection's sweep.
		a.awaitingBacklog()
		if err := a.Adapter.Connect(ctx); err != nil {
			d := b.Next()
			a.Log.Warn("connect failed; retrying", "in", d, "err", mask.IDs(err.Error()))
			a.mon.onDisconnected(a.Clock.Now())
			retry = a.Clock.After(d)
		}
	}
	a.mon.onDisconnected(a.Clock.Now())
	if a.BootRejected != nil {
		a.Log.Error("config rejected at boot; running the last good config", "err", a.BootRejected)
		a.alert(ctx, alert.Alert{Kind: alert.ConfigRejected, Priority: true, Text: a.BootRejected.Error()})
	}
	connect()
	tick := a.Clock.After(monitorEvery)
	for {
		select {
		case <-ctx.Done():
			a.Adapter.Disconnect()
			// The last status write says so: a stopped bot is not connected,
			// and the install script's health check must not pass on the
			// status of the process it just restarted.
			a.mon.onDisconnected(a.Clock.Now())
			return nil
		case <-a.Reload:
			a.reload(ctx, true)
		case <-a.refreshNow:
			// A failed read is tried again at the next tick, which sweeps
			// once it succeeds.
			if connected, _, _ := a.mon.snapshot(); connected && a.refreshDirectory(ctx) {
				a.requestSweep()
			}
		case <-retry:
			retry = nil
			connect()
		case <-tick:
			// The time first, then the next tick: once that is armed, this
			// tick's checks have their time (tests step the clock on it).
			now := a.Clock.Now()
			tick = a.Clock.After(monitorEvery)
			for _, al := range a.mon.check(now) {
				a.alert(ctx, al)
			}
			a.checkOverdue(ctx, now)
			a.checkPhone(ctx, now)
			connected, _, _ := a.mon.snapshot()
			switch {
			case connected && !now.Before(nextCompanionCheck):
				nextCompanionCheck = now.Add(a.settings().CompanionCheckEvery)
				a.refreshDirectory(ctx)
				a.checkCompanions(ctx)
				a.requestSweep()
			case connected && (a.listStale || !a.Directory.IsLoaded()):
				// The last group list read failed: until one loads nothing is
				// decided, and after that the list may lack what WhatsApp
				// sent no event for (a group linked to a community), so try
				// again every tick, not every interval.
				if a.refreshDirectory(ctx) {
					a.requestSweep()
				}
			}
			a.writeStatus(ctx)
			a.checkDaily(ctx, now) // after the status write: it reports the state just recorded
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
					if a.Executor != nil {
						a.Executor.Wake() // the actions that waited for the connection
					}
					nextCompanionCheck = now.Add(a.settings().CompanionCheckEvery)
					a.Directory.SetSelf(a.Adapter.Self())
					a.refreshDirectory(ctx)
					a.checkCompanions(ctx)
					a.requestSweep()
					a.writeStatus(ctx)
				case l.Kind == client.CaughtUp:
					a.backlogDelivered(ctx, l.Detail)
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
						l.Detail, bannedUntil.UTC().Format("2006-01-02 15:04 MST"), wait.Round(time.Minute)),
						Buttons: []string{ledger.ButtonResume}})
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

// alert delivers al with a deadline, so a stuck channel never blocks shutdown;
// a failure is logged.
func (a *App) alert(ctx context.Context, al alert.Alert) {
	_ = a.tryAlert(ctx, al)
}

// tryAlert is alert that also returns the failure.
func (a *App) tryAlert(ctx context.Context, al alert.Alert) error {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), alertTimeout)
	defer cancel()
	err := a.Alerter.Alert(actx, al)
	if err != nil {
		a.Log.Error("alert delivery failed", "kind", string(al.Kind), "err", err)
	}
	return err
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
	for _, d := range strings.Split(a.marked(status, store.StatusCompanionsSeen), ",") {
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
	if len(fresh) == 0 {
		a.mark(ctx, store.StatusCompanionsSeen, strings.Join(current, ","))
		return
	}
	now := a.Clock.Now()
	reason := fmt.Sprintf("%d other linked device(s) on the bot number", len(devices))
	a.pause(ctx, store.Pause{Source: store.SourceExtraCompanion, Scope: store.ScopeRemoveBan, Reason: reason, Since: now})
	a.alert(ctx, alert.Alert{Kind: alert.ExtraCompanion, Priority: true, Text: reason +
		" (new: " + mask.IDs(strings.Join(fresh, ", ")) + "). Removals and bans are PAUSED; deletes continue. " +
		"If you linked it yourself press [Resume]; if not, unlink it on the bot phone now.",
		Buttons: []string{ledger.ButtonResume}})
	// Marked last: a stop before this repeats the pause and the alert at the
	// next check instead of losing them.
	a.mark(ctx, store.StatusCompanionsSeen, strings.Join(current, ","))
}

// reload swaps in the config file again (whole, or not at all) and applies
// its supervisor settings. It returns "config v<hash> loaded" or why it was
// REJECTED; announce also tells the admins (a /reload gets it as its reply).
// Safe from any goroutine.
func (a *App) reload(ctx context.Context, announce bool) string {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	l, err := a.Config.Reload()
	if err != nil {
		a.Log.Error("config reload rejected", "err", err)
		if announce {
			a.alert(ctx, alert.Alert{Kind: alert.ConfigRejected, Priority: true, Text: err.Error()})
		}
		return err.Error()
	}
	s := SettingsFrom(l.Config)
	a.settingsMu.Lock()
	a.Settings = s
	a.settingsMu.Unlock()
	if a.mon != nil { // nil until Run starts
		a.mon.setThresholds(s.DeafAfter, s.DisconnectAlert)
	}
	a.Log.Info("config reloaded", "config", "v"+l.Hash)
	text := "config v" + l.Hash + " loaded"
	if announce {
		a.alert(ctx, alert.Alert{Kind: alert.ConfigLoaded, Text: text})
	}
	a.writeStatus(ctx)
	return text
}

func (a *App) settings() Settings {
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	return a.Settings
}

// outsideTimer is a timer outside the app that writes its last run to the
// store.
type outsideTimer struct {
	key, name string
	every     func(a *App) time.Duration
}

var outsideTimers = []outsideTimer{
	{store.StatusSyncLastRun, "config sync", func(a *App) time.Duration {
		return time.Duration(a.Config.Current().Config.ConfigSyncMinutes) * time.Minute
	}},
	{store.StatusBackupLastRun, "backup", func(*App) time.Duration { return 24 * time.Hour }},
}

// watchedTimers are the outside timers this deployment has: all of them, less
// the config sync under NoSyncTimer.
func (a *App) watchedTimers() []outsideTimer {
	var out []outsideTimer
	for _, t := range outsideTimers {
		if a.NoSyncTimer && t.key == store.StatusSyncLastRun {
			continue
		}
		out = append(out, t)
	}
	return out
}

// checkOverdue raises one priority alert per episode when a watched timer
// (the config sync, the nightly backup) has not run for twice its interval
// (counted from this start when it never ran).
func (a *App) checkOverdue(ctx context.Context, now time.Time) {
	st, err := a.Store.Status(ctx)
	if err != nil {
		a.Log.Error("read status", "err", err)
		return
	}
	for _, t := range a.watchedTimers() {
		last := st[t.key].Time()
		since := last
		if since.IsZero() {
			since = a.started
		}
		every := t.every(a)
		if now.Sub(since) <= 2*every {
			delete(a.overdue, t.key)
			continue
		}
		if seen, ok := a.overdue[t.key]; ok && seen.Equal(last) {
			continue
		}
		a.overdue[t.key] = last
		a.Log.Error("outside timer overdue", "timer", t.name, "last_run", last, "every", every)
		a.alert(ctx, alert.Alert{Kind: alert.Overdue, Priority: true, Text: fmt.Sprintf(
			"The %s timer has not run for %s (it runs every %s; last run %s). Check the timer that runs it on the "+
				"node (docs/runbook.md).", t.name, now.Sub(since).Round(time.Minute), every, ago(now, last))})
	}
}

// Phone reminder timings (WhatsApp unlinks a linked device after 14 days
// without the phone; fixed, not tunables).
const (
	phoneRemindAfter   = 7 * 24 * time.Hour
	phoneEscalateAfter = 10 * 24 * time.Hour
)

// checkPhone sends the weekly "open WhatsApp on the bot phone" reminder with
// [Done], and a priority escalation on day 10 without [Done]; each once per
// [Done] episode.
func (a *App) checkPhone(ctx context.Context, now time.Time) {
	st, err := a.Store.Status(ctx)
	if err != nil {
		a.Log.Error("read status", "err", err)
		return
	}
	key := st[store.StatusPhoneDone].Value
	done := st[store.StatusPhoneDone].Time()
	if done.IsZero() {
		// No [Done] yet: count from this first start.
		if err := a.Store.SetStatus(ctx, map[string]string{
			store.StatusPhoneDone: store.StatusTime(now)}); err != nil {
			a.Log.Error("write status", "err", err)
		}
		return
	}
	age := now.Sub(done)
	days := int(age / (24 * time.Hour))
	// Each is marked sent only once the admin chat took it: the next tick
	// tries again otherwise.
	switch {
	case age >= phoneEscalateAfter && a.marked(st, store.StatusPhoneEscalated) != key:
		a.Log.Error("bot phone not confirmed opened", "days", days)
		if a.tryAlert(ctx, alert.Alert{Kind: alert.PhoneEscalation, Priority: true, Buttons: []string{ledger.ButtonDone},
			Text: fmt.Sprintf("Day %d without [Done]: open WhatsApp on the bot phone NOW. WhatsApp unlinks the bot's "+
				"linked device after 14 days without the phone, and groupwarden stops until it is paired again. "+
				"Press [Done] once you have.", days)}) == nil {
			a.mark(ctx, store.StatusPhoneEscalated, key)
		}
	case age >= phoneRemindAfter && age < phoneEscalateAfter && a.marked(st, store.StatusPhoneReminded) != key:
		if a.tryAlert(ctx, alert.Alert{Kind: alert.PhoneReminder, Buttons: []string{ledger.ButtonDone},
			Text: "Weekly check: open WhatsApp on the bot phone (WhatsApp unlinks the bot's linked device after 14 " +
				"days without the phone). Press [Done] once you have."}) == nil {
			a.mark(ctx, store.StatusPhoneReminded, key)
		}
	}
}

// mark sets a once-marker (the daily check's day, the phone reminder's
// episode, the linked devices reported): for this run in memory, and in the
// store for the next start. The memory copy is what stops a repeat when the
// store write fails: a database that cannot be written must not turn a
// once-a-day post into one every tick.
func (a *App) mark(ctx context.Context, key, value string) {
	a.marksMu.Lock()
	if a.marks == nil {
		a.marks = map[string]string{}
	}
	a.marks[key] = value
	a.marksMu.Unlock()
	if err := a.Store.SetStatus(ctx, map[string]string{key: value}); err != nil {
		a.Log.Error("write status (kept in memory for this run)", "key", key, "err", err)
	}
}

// marked is a once-marker's value: what this run marked, else the store's.
func (a *App) marked(st map[string]store.StatusValue, key string) string {
	a.marksMu.Lock()
	defer a.marksMu.Unlock()
	if v, ok := a.marks[key]; ok {
		return v
	}
	return st[key].Value
}

// awaitingBacklog starts holding sweeps as a connection starts, until
// WhatsApp says it has delivered what it held meanwhile (a connect before it
// said so keeps waiting for the same).
func (a *App) awaitingBacklog() {
	a.backlogMu.Lock()
	defer a.backlogMu.Unlock()
	select {
	case <-a.caughtUp:
		a.caughtUp = make(chan struct{})
	default:
	}
}

// backlogDelivered records that WhatsApp has delivered its offline backlog:
// sweeps may run once the worker has decided every inbox row there is now.
func (a *App) backlogDelivered(ctx context.Context, detail string) {
	mark, err := a.Store.InboxLast(ctx)
	if err != nil {
		mark = math.MaxInt64 // not knowing the newest row, wait for every row
		a.Log.Error("could not read the inbox; sweeps wait until it is empty", "err", err)
	}
	a.Log.Info("WhatsApp delivered the offline backlog", "detail", detail, "inbox_through", mark)
	a.backlogMu.Lock()
	defer a.backlogMu.Unlock()
	a.backlogMark = mark
	select {
	case <-a.caughtUp:
	default:
		close(a.caughtUp)
	}
}

// refreshDirectory relearns every group's community and admins and reports
// whether it could. While the list has never loaded nothing is decided, so
// that failure is a priority alert (once until a list loads).
func (a *App) refreshDirectory(ctx context.Context) bool {
	err := a.Directory.Refresh(func() ([]client.Group, error) { return a.Adapter.JoinedGroups(ctx) })
	a.listStale = err != nil
	if err != nil {
		a.Log.Warn("could not list groups", "retry_in", monitorEvery, "err", mask.IDs(err.Error()))
		if !a.Directory.IsLoaded() && !a.dirAlerted {
			a.dirAlerted = true
			a.alert(ctx, alert.Alert{Kind: alert.CoverageLost, Priority: true, Text: "The bot could not list its " +
				"WhatsApp groups (" + mask.IDs(err.Error()) + "): no message is checked until it can. It tries again " +
				"every 30 seconds; messages wait in its inbox meanwhile."})
		}
		return false
	}
	a.dirAlerted = false
	return true
}

// writeStatus records the run state for `healthcheck` and other commands.
func (a *App) writeStatus(ctx context.Context) {
	if a.mon == nil { // nil until Run starts
		return
	}
	connected, deaf, last := a.mon.snapshot()
	kv := map[string]string{
		store.StatusHeartbeat:  "",
		store.StatusConnected:  boolStr(connected),
		store.StatusDeaf:       boolStr(deaf),
		store.StatusConfigHash: a.Config.Current().Hash,
	}
	if !last.IsZero() {
		kv[store.StatusLastEvent] = store.StatusTime(last)
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

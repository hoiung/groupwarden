// Package action fires queued WhatsApp actions (delete, remove, reject) one
// at a time: through the token bucket, past the circuit breaker, re-checked
// immediately before the send, and recorded in the ledger.
package action

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"golang.org/x/time/rate"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/rules"
	"github.com/hoiung/groupwarden/internal/store"
)

// Fixed timings (not tunables).
const (
	idlePoll         = 5 * time.Second
	retryBase        = 30 * time.Second
	retryMax         = 30 * time.Minute
	maxAttempts      = 8
	rateLimitBackoff = 2 * time.Minute
)

// Executor fires the outbox.
type Executor struct {
	Store     *store.Store
	Adapter   client.Adapter
	Config    *config.Holder
	Directory *pipeline.Directory
	// Moderator re-decides a message with the current config at fire time.
	Moderator *pipeline.Moderator
	Alerter   alert.Alerter
	// Reported is called after a report was stored (wakes the reporter).
	Reported func()
	// NotAdmin is called when WhatsApp refused an action because the bot is
	// not an admin of chat (cause names the action). The app sets it to the
	// sweep's LostAdmin, which marks the group not covered and alerts (nil:
	// the row is only failed).
	NotAdmin func(ctx context.Context, chat client.JID, cause string)
	Log      *slog.Logger
	Now      func() time.Time
	// Sleep waits for d or until ctx ends (tests replace it).
	Sleep func(ctx context.Context, d time.Duration) error

	wake    chan struct{}
	limiter *rate.Limiter
	rate    config.Rate
}

// Sleep is the real Executor.Sleep.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (x *Executor) init() {
	if x.wake == nil {
		x.wake = make(chan struct{}, 1)
	}
	if x.Now == nil {
		x.Now = time.Now
	}
	if x.Sleep == nil {
		x.Sleep = Sleep
	}
}

// Wake asks the executor to look at the outbox now.
func (x *Executor) Wake() {
	x.init()
	select {
	case x.wake <- struct{}{}:
	default:
	}
}

// Run fires actions as they become due until ctx ends.
func (x *Executor) Run(ctx context.Context) {
	x.init()
	for {
		fired, err := x.Step(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			x.Log.Error("action step failed", "err", mask.IDs(err.Error()))
		}
		if fired && err == nil {
			continue
		}
		wait := idlePoll
		if at, ok, err := x.Store.NextWake(ctx); err == nil && ok {
			if d := at.Sub(x.Now()); d < wait {
				wait = max(d, 10*time.Millisecond)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-x.wake:
		case <-time.After(wait):
		}
	}
}

// Step fires (or settles) at most one due action and reports whether it did.
// A pause holds the actions it covers in the outbox: deletes stop only for a
// full pause, removals and rejections for either kind.
func (x *Executor) Step(ctx context.Context) (bool, error) {
	x.init()
	pausedAll, _ := x.Store.PausedFor(ctx, store.ScopeAll)
	pausedRemove, _ := x.Store.PausedFor(ctx, store.ScopeRemoveBan)
	row, ok, err := x.Store.NextDue(ctx, x.Now(), !pausedAll, !pausedRemove)
	if err != nil || !ok {
		return false, err
	}
	if err := x.takeToken(ctx); err != nil {
		return false, err
	}
	if row.Action != store.ActRevoke {
		tripped, err := x.breaker(ctx)
		if err != nil || tripped {
			return false, err
		}
	}
	// Re-read: the row may have been settled while this step waited.
	row, err = x.Store.Ledger(ctx, row.ID)
	if err != nil {
		return false, err
	}
	if row.Status != store.Intended || row.Mode != store.ModeEnforce {
		return true, nil
	}
	v, err := x.recheck(ctx, row)
	if err != nil {
		return false, err
	}
	switch v.outcome {
	case hold:
		return false, nil
	case shadow:
		if err := x.Store.ToShadow(ctx, row.ID, v.reason); err != nil {
			return false, err
		}
		x.report(ctx, store.Report{Kind: ledger.KindWouldRemove, Community: row.Community, Subject: row.Target,
			Text: fmt.Sprintf("Would %s in %s, which is in shadow mode: not done.", what(row.Action), row.Community)}, row.ID)
		return true, nil
	case fail:
		if v.report != nil {
			x.report(ctx, *v.report, row.ID)
		}
		x.Log.Info("action not sent", "action", string(row.Action), "chat", mask.IDs(row.Chat), "reason", mask.IDs(v.reason))
		return true, x.Store.Finish(ctx, row.ID, store.Failed, v.reason, 0, time.Time{})
	}
	return true, x.send(ctx, row)
}

// takeToken waits for the token bucket (rate.per_minute, rate.burst).
func (x *Executor) takeToken(ctx context.Context) error {
	r := x.Config.Current().Config.Rate
	if x.limiter == nil || r != x.rate {
		x.limiter = rate.NewLimiter(rate.Limit(float64(r.PerMinute)/60), r.Burst)
		x.rate = r
	}
	now := x.Now()
	res := x.limiter.ReserveN(now, 1)
	if !res.OK() {
		return fmt.Errorf("rate limiter refused a token (burst %d)", r.Burst)
	}
	if d := res.DelayFrom(now); d > 0 {
		return x.Sleep(ctx, d)
	}
	return nil
}

// breaker trips when breaker.max_actions removals and rejections were sent in
// the last breaker.window_minutes (counted from the last [Resume]): removals
// and bans pause, deletes continue, and the admins get [Resume].
func (x *Executor) breaker(ctx context.Context) (bool, error) {
	cfg := x.Config.Current().Config.Breaker
	now := x.Now()
	window := time.Duration(cfg.WindowMinutes) * time.Minute
	since := now.Add(-window)
	status, err := x.Store.Status(ctx)
	if err != nil {
		return false, err
	}
	if ms, err := strconv.ParseInt(status[store.StatusBreakerReset].Value, 10, 64); err == nil {
		if reset := time.UnixMilli(ms); reset.After(since) {
			since = reset
		}
	}
	n, err := x.Store.SentSince(ctx, since)
	if err != nil {
		return false, err
	}
	if n < cfg.MaxActions {
		return false, nil
	}
	text := fmt.Sprintf("Circuit breaker: %d removals in %d minutes. Removals and bans are PAUSED; deletes continue. "+
		"Check /status, then press [Resume].", n, cfg.WindowMinutes)
	if err := x.Store.SetPause(ctx, store.Pause{Source: store.SourceBreaker, Scope: store.ScopeRemoveBan,
		Reason: text, Since: now}); err != nil {
		return false, err
	}
	if err := x.Alerter.Alert(ctx, alert.Alert{Kind: alert.Breaker, Priority: true, Text: text,
		Buttons: []string{ledger.ButtonResume}}); err != nil {
		x.Log.Error("breaker alert failed", "err", err)
	}
	return true, nil
}

// Resume is [Resume]: every pause is lifted, the breaker counts again from
// now, and queued actions fire, each re-checked first.
func (x *Executor) Resume(ctx context.Context) error {
	pauses, err := x.Store.Pauses(ctx)
	if err != nil {
		return err
	}
	for _, p := range pauses {
		if p.Source == store.SourceStorage {
			x.Store.ClearStorageFailure()
			continue
		}
		if err := x.Store.ClearPause(ctx, p.Source); err != nil {
			return err
		}
	}
	if err := x.Store.SetStatus(ctx, map[string]string{
		store.StatusBreakerReset: strconv.FormatInt(x.Now().UnixMilli(), 10)}); err != nil {
		return err
	}
	x.Wake()
	return nil
}

type outcome int

const (
	proceed outcome = iota
	hold            // a pause covers it: stays queued
	shadow          // the target scope is in shadow mode
	fail            // a check failed: the row is failed with the reason
)

type verdict struct {
	outcome outcome
	reason  string
	report  *store.Report
}

// recheck runs immediately before the send (after the token bucket and the
// breaker): the target scope must be in enforce mode, no pause may cover the
// action, the current config must still decide it, the target must still be
// banned, the message must still be young enough, and the target must not be
// a current admin.
func (x *Executor) recheck(ctx context.Context, row store.LedgerRow) (verdict, error) {
	if paused, why := x.Store.PausedFor(ctx, store.ScopeOf(row.Action)); paused {
		return verdict{outcome: hold, reason: why}, nil
	}
	cur := x.Config.Current()
	mode, configured := cur.Rules.ModeFor(row.Community)
	if !configured {
		return verdict{outcome: fail, reason: row.Community + " is no longer a configured community"}, nil
	}
	if mode != rules.Enforce {
		return verdict{outcome: shadow, reason: row.Community + " is in shadow mode"}, nil
	}
	if row.EvidenceID != 0 {
		ev, ok, err := x.Store.Evidence(ctx, row.EvidenceID)
		if err != nil {
			return verdict{}, err
		}
		if !ok {
			return verdict{outcome: fail, reason: "the evidence copy is gone (purged or forgotten)"}, nil
		}
		msg, err := evidenceMessage(ev)
		if err != nil {
			return verdict{outcome: fail, reason: err.Error()}, nil
		}
		d, _, hash := x.Moderator.Evaluate(msg)
		if d.Action != rules.DeleteRemoveBan {
			return verdict{outcome: fail, reason: "the current config (v" + hash + ") no longer acts on this message"}, nil
		}
		if row.Action != store.ActRevoke && !slices.Contains(d.BanIn, row.Community) {
			return verdict{outcome: fail, reason: "the current ban scope no longer covers " + row.Community}, nil
		}
	}
	member := x.Directory.Complete(client.MemberOf(client.JID(row.Target), client.JID(row.Address)))
	if _, banned, err := x.Store.FindBan(ctx, member.IDs(), row.Community); err != nil {
		return verdict{}, err
	} else if !banned {
		return verdict{outcome: fail, reason: "the member was unbanned"}, nil
	}
	maxAge := time.Duration(cur.Config.ActOnReplayMaxAge)
	if !row.MsgTime.IsZero() && x.Now().Sub(row.MsgTime) > maxAge {
		return verdict{outcome: fail, reason: "the message is older than act_on_replay_max_age (" + maxAge.String() + ")"}, nil
	}
	if x.Directory.IsAdmin(member.LID, member.Phone, cur.Rules) {
		return verdict{outcome: fail, reason: "the member is a current admin", report: &store.Report{
			Kind: ledger.KindAdminSpared, Priority: true, Community: row.Community, Subject: row.Target,
			Text: fmt.Sprintf("The bot did not %s: the member is a current admin in %s. An admin must decide.",
				what(row.Action), row.Community)}}, nil
	}
	return verdict{outcome: proceed}, nil
}

// send makes the WhatsApp call and records its result.
func (x *Executor) send(ctx context.Context, row store.LedgerRow) error {
	chat := client.JID(row.Chat)
	member := x.Directory.Complete(client.MemberOf(client.JID(row.Target), client.JID(row.Address)))
	addr := client.JID(row.Address)
	if row.Action == store.ActRemove && x.Directory.Present(chat, member) {
		addr = x.Directory.AddressIn(chat, member)
	}
	if addr == "" {
		addr = client.JID(row.Target)
	}
	now := x.Now()
	var err error
	status, code := store.Requested, 0
	switch row.Action {
	case store.ActRevoke:
		err = x.Adapter.Revoke(ctx, chat, addr, row.MsgID)
	case store.ActRemove, store.ActReject:
		call := x.Adapter.Remove
		if row.Action == store.ActReject {
			call = x.Adapter.RejectJoinRequests
		}
		var results []client.MemberResult
		results, err = call(ctx, chat, []client.JID{addr})
		if err == nil {
			status, code = memberStatus(results)
		}
	default:
		return x.Store.Finish(ctx, row.ID, store.Failed, "not a WhatsApp action", 0, time.Time{})
	}
	if err != nil {
		return x.failed(ctx, row, err)
	}
	reason := ""
	if status == store.Failed {
		reason = fmt.Sprintf("WhatsApp refused it (code %d)", code)
	}
	x.Log.Info("action sent", "action", string(row.Action), "chat", mask.IDs(row.Chat), "target", mask.IDs(row.Target),
		"status", string(status), "code", code, "config", "v"+row.ConfigHash)
	return x.Store.Finish(ctx, row.ID, status, reason, code, now)
}

func memberStatus(results []client.MemberResult) (store.Status, int) {
	if len(results) == 0 {
		return store.Failed, -1
	}
	switch r := results[0]; r.Status {
	case client.MemberDone:
		return store.Requested, 0
	case client.MemberAlreadyGone:
		return store.AlreadyGone, r.Code
	default:
		return store.Failed, r.Code
	}
}

// failed handles a call that returned an error: a rate limit waits and
// retries, a permission refusal fails the row, anything else is retried
// with backoff up to maxAttempts.
func (x *Executor) failed(ctx context.Context, row store.LedgerRow, err error) error {
	now := x.Now()
	msg := mask.IDs(err.Error())
	switch {
	case errors.Is(err, client.ErrRateLimited):
		x.Log.Warn("WhatsApp rate limit; backing off", "action", string(row.Action), "for", rateLimitBackoff)
		return x.Store.Retry(ctx, row.ID, now.Add(rateLimitBackoff), msg)
	case errors.Is(err, client.ErrNotAdmin):
		x.Log.Warn("the bot is not an admin there", "action", string(row.Action), "chat", mask.IDs(row.Chat))
		if err := x.Store.Finish(ctx, row.ID, store.Failed, "the bot is not an admin in this group: "+msg, 0, now); err != nil {
			return err
		}
		if x.NotAdmin != nil {
			x.NotAdmin(ctx, client.JID(row.Chat), "WhatsApp refused to "+what(row.Action))
		}
		return nil
	case row.Attempts+1 >= maxAttempts:
		x.Log.Error("action failed", "action", string(row.Action), "chat", mask.IDs(row.Chat), "err", msg)
		return x.Store.Finish(ctx, row.ID, store.Failed, msg, 0, now)
	}
	d := min(retryBase<<row.Attempts, retryMax)
	x.Log.Warn("action failed; retrying", "action", string(row.Action), "in", d, "err", msg)
	return x.Store.Retry(ctx, row.ID, now.Add(d), msg)
}

func (x *Executor) report(ctx context.Context, r store.Report, ledgerID int64) {
	if _, err := x.Store.AddReport(ctx, r, []int64{ledgerID}); err != nil {
		x.Log.Error("could not store a report", "kind", r.Kind, "err", err)
		return
	}
	if x.Reported != nil {
		x.Reported()
	}
}

func what(a store.Action) string {
	switch a {
	case store.ActRevoke:
		return "delete a message"
	case store.ActReject:
		return "reject a join request"
	}
	return "remove a member"
}

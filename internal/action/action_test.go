package action_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
)

func pause(t *testing.T, k *modtest.Kit, source string, scope store.Scope) {
	t.Helper()
	if err := k.Store.SetPause(k.Ctx, store.Pause{Source: source, Scope: scope, Reason: "test", Since: k.Clock.Now()}); err != nil {
		t.Fatal(err)
	}
}

func status(t *testing.T, k *modtest.Kit, m client.Member, a store.Action, chat client.JID) store.LedgerRow {
	t.Helper()
	rows := k.Find(m, a, chat)
	if len(rows) != 1 {
		t.Fatalf("%s %s rows: %+v", a, chat, rows)
	}
	return rows[0]
}

// TestRecheckPaused: a pause holds the actions it covers in the outbox
// (deletes only stop for a full pause) and they fire after [Resume].
func TestRecheckPaused(t *testing.T) {
	k := modtest.New(t, "")
	pause(t, k, store.SourceExtraCompanion, store.ScopeRemoveBan)
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Fire()
	if k.Fake.Count("Revoke") != 1 || k.Fake.Count("Remove") != 0 {
		t.Fatalf("with removals paused: %v", k.Fake.Calls())
	}
	if r := status(t, k, modtest.SpammerM, store.ActRemove, modtest.G1); r.Status != store.Intended {
		t.Fatalf("paused removal left the queue: %s", r.Status)
	}
	pause(t, k, store.SourceRestore, store.ScopeAll)
	k.Clock.Advance(time.Minute)
	k.Deliver(k.Msg("M2", modtest.GB, modtest.Member, modtest.SpamText))
	k.Fire()
	if k.Fake.Count("Revoke") != 1 {
		t.Fatal("a delete fired under a full pause")
	}
	if err := k.Exec.Resume(k.Ctx); err != nil {
		t.Fatal(err)
	}
	k.Fire()
	if k.Fake.Count("Revoke") != 2 || k.Fake.Count("Remove "+string(modtest.G1)+" "+string(modtest.Spammer)) != 1 {
		t.Fatalf("after [Resume]: %v", k.Fake.Calls())
	}
}

// TestRecheckConfigChanged: an action the current config no longer decides
// fails with the reason instead of firing.
func TestRecheckConfigChanged(t *testing.T) {
	k := modtest.New(t, "")
	k.Deliver(k.Spam("M1", modtest.G1))
	k.ReloadConfig(strings.Replace(modtest.Config, "confirmed: true", "confirmed: false", 1))
	k.Fire()
	if k.Fake.Count("Revoke")+k.Fake.Count("Remove") != 0 {
		t.Fatalf("fired under a config that no longer acts: %v", k.Fake.Calls())
	}
	for _, r := range k.Rows(modtest.SpammerM) {
		if r.Action == store.ActBan {
			continue
		}
		if r.Status != store.Failed || !strings.Contains(r.Reason, "no longer acts on this message") {
			t.Fatalf("%s %s: %s %q", r.Action, r.Chat, r.Status, r.Reason)
		}
	}
}

// TestRecheckUnbanned: an action on someone unbanned before it fired fails.
func TestRecheckUnbanned(t *testing.T) {
	k := modtest.New(t, "")
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Unban(modtest.SpammerM)
	k.Fire()
	if k.Fake.Count("Revoke")+k.Fake.Count("Remove") != 0 {
		t.Fatalf("fired on an unbanned member: %v", k.Fake.Calls())
	}
	if r := status(t, k, modtest.SpammerM, store.ActRevoke, modtest.G1); r.Status != store.Failed || !strings.Contains(r.Reason, "not banned in") {
		t.Fatalf("revoke: %s %q", r.Status, r.Reason)
	}
}

// TestRecheckTooOld: an action whose message got older than
// act_on_replay_max_age while it waited fails.
func TestRecheckTooOld(t *testing.T) {
	k := modtest.New(t, "")
	m := k.Spam("M1", modtest.G1)
	m.Time = k.Clock.Now().Add(-40 * time.Hour)
	k.Deliver(m)
	if r := status(t, k, modtest.SpammerM, store.ActRevoke, modtest.G1); r.Mode != store.ModeEnforce {
		t.Fatal("a 40-hour-old message was not acted on")
	}
	k.Clock.Advance(8 * time.Hour) // 48 hours old now
	k.Fire()
	if k.Fake.Count("Revoke") != 0 {
		t.Fatal("deleted a message older than act_on_replay_max_age")
	}
	if r := status(t, k, modtest.SpammerM, store.ActRevoke, modtest.G1); r.Status != store.Failed || !strings.Contains(r.Reason, "older than") {
		t.Fatalf("revoke: %s %q", r.Status, r.Reason)
	}
}

// TestRecheckAfterLimiter: the re-check runs after the token bucket, so a
// change while an action waited for its token is seen.
func TestRecheckAfterLimiter(t *testing.T) {
	k := modtest.New(t, "rate:\n  per_minute: 1\n  burst: 1\n")
	pause(t, k, store.SourceExtraCompanion, store.ScopeRemoveBan) // deletes only
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Deliver(k.Msg("M2", modtest.GB, modtest.Member, modtest.SpamText))
	unbanned := false
	k.Clock.OnSleep = func(time.Duration) {
		if !unbanned {
			unbanned = true
			k.Unban(modtest.MemberM) // while the second delete waits for its token
		}
	}
	k.Fire()
	if slept := k.Clock.Slept(); len(slept) != 1 || slept[0] != time.Minute {
		t.Fatalf("slept %v, want one wait of 1m", slept)
	}
	if k.Fake.Count("Revoke") != 1 {
		t.Fatalf("calls %v, want only the first delete", k.Fake.Calls())
	}
	if r := status(t, k, modtest.MemberM, store.ActRevoke, modtest.GB); r.Status != store.Failed {
		t.Fatalf("second delete: %s", r.Status)
	}
}

// TestPostJudgedInItsOwnCommunity: under all_communities a spam post in one
// community removes the sender from the other community's groups too, even
// where that community disables the rule: the fire-time re-check judges the
// post in the community it was posted in.
func TestPostJudgedInItsOwnCommunity(t *testing.T) {
	k := modtest.NewConfig(t, strings.Replace(modtest.Config, "    name: set b\n", "    name: set b\n    disable_rules: [pitch]\n", 1))
	k.Dir.Apply(&client.GroupChange{Group: modtest.GB, Joined: []client.JID{modtest.Spammer}})
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Fire()
	if k.Fake.Count("Remove "+string(modtest.GB)+" "+string(modtest.Spammer)) != 1 {
		t.Fatalf("not removed from the other community's group: %v", k.Fake.Calls())
	}
}

// TestFanOutHonoursTargetScopeMode: a removal in a community in shadow mode
// is only a "would remove" record, at decision time and at fire time.
func TestFanOutHonoursTargetScopeMode(t *testing.T) {
	cfg := strings.Replace(modtest.Config, "    name: set b\n", "    name: set b\n    mode: shadow\n", 1)
	k := modtest.NewConfig(t, cfg)
	k.Dir.Apply(&client.GroupChange{Group: modtest.GB, Joined: []client.JID{modtest.Spammer}})
	k.Deliver(k.Spam("M1", modtest.G1))
	if r := status(t, k, modtest.SpammerM, store.ActRemove, modtest.GB); r.Mode != store.ModeShadow {
		t.Fatalf("removal in a shadow community recorded as %s", r.Mode)
	}
	if r := status(t, k, modtest.SpammerM, store.ActRemove, modtest.G1); r.Mode != store.ModeEnforce {
		t.Fatalf("removal in an enforce community recorded as %s", r.Mode)
	}
	// The community goes to shadow mode before the queued removals fire.
	k.ReloadConfig(strings.Replace(cfg, "    name: community a\n", "    name: community a\n    mode: shadow\n", 1))
	k.Fire()
	if k.Fake.Count("Remove")+k.Fake.Count("Revoke") != 0 {
		t.Fatalf("fired in a shadow community: %v", k.Fake.Calls())
	}
	if r := status(t, k, modtest.SpammerM, store.ActRemove, modtest.G1); r.Mode != store.ModeShadow || r.Status != store.Intended {
		t.Fatalf("queued removal became %s/%s, want a shadow record", r.Mode, r.Status)
	}
	if reps := k.Reports(ledger.KindWouldRemove); len(reps) == 0 {
		t.Fatal("no would-remove report")
	}
	if k.Logged("action moved to shadow") == 0 {
		t.Fatal("no log line for the actions moved to shadow")
	}
	if n, _ := k.Store.OutboxLen(k.Ctx); n != 0 {
		t.Fatalf("outbox still holds %d rows", n)
	}
}

// revokes queues n deletes of messages by banned members (no evidence copy,
// so only the ban and the age are re-checked).
func revokes(t *testing.T, k *modtest.Kit, n int) {
	t.Helper()
	k.Ban(modtest.MemberM)
	for i := 0; i < n; i++ {
		id := string(rune('A' + i))
		writePlan(t, k, ledger.Plan{Trigger: "T" + id, Target: modtest.MemberM, ConfigHash: "h",
			Intents: []ledger.Intent{{Action: store.ActRevoke, Chat: modtest.G1, Community: string(modtest.Community),
				Enforce: true, Address: modtest.Member, MsgID: "MSG" + id, MsgTime: k.Clock.Now()}}})
	}
}

// removals queues one removal per group for a banned member.
func removals(t *testing.T, k *modtest.Kit, m client.Member, groups ...client.JID) {
	t.Helper()
	k.Ban(m)
	for _, g := range groups {
		writePlan(t, k, ledger.Plan{Trigger: "R" + string(g), Target: m, ConfigHash: "h",
			Intents: []ledger.Intent{{Action: store.ActRemove, Chat: g, Community: string(modtest.Community), Enforce: true}}})
	}
}

func writePlan(t *testing.T, k *modtest.Kit, p ledger.Plan) {
	t.Helper()
	if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
		_, err := ledger.Write(k.Ctx, tx, p, k.Clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestTokenBucketQueues: outbound actions take tokens (rate.per_minute,
// rate.burst); past the burst each waits its turn, in order.
func TestTokenBucketQueues(t *testing.T) {
	k := modtest.New(t, "rate:\n  per_minute: 6\n  burst: 2\n")
	revokes(t, k, 4)
	if n := k.Fire(); n != 4 {
		t.Fatalf("fired %d, want 4", n)
	}
	if slept := k.Clock.Slept(); len(slept) != 2 || slept[0] != 10*time.Second || slept[1] != 10*time.Second {
		t.Fatalf("waits %v, want two of 10s (6 a minute after a burst of 2)", slept)
	}
	var order []string
	for _, c := range k.Fake.Calls() {
		if strings.HasPrefix(c, "Revoke") {
			order = append(order, c[strings.LastIndex(c, " ")+1:])
		}
	}
	if strings.Join(order, ",") != "MSGA,MSGB,MSGC,MSGD" {
		t.Fatalf("order %v", order)
	}
}

// TestBreakerPausesRemovalsNotDeletes: after breaker.max_actions removals in
// the window, removals and bans pause with a [Resume] alert; deletes go on.
func TestBreakerPausesRemovalsNotDeletes(t *testing.T) {
	k := modtest.New(t, "breaker:\n  max_actions: 2\n  window_minutes: 10\n")
	removals(t, k, modtest.Other1M, modtest.G1, modtest.G2, modtest.GB)
	revokes(t, k, 1)
	k.Fire()
	if n := k.Fake.Count("Remove"); n != 2 {
		t.Fatalf("removed %d times, want 2 then the breaker", n)
	}
	if k.Fake.Count("Revoke") != 1 {
		t.Fatal("the breaker stopped a delete")
	}
	paused, why := k.Store.PausedFor(k.Ctx, store.ScopeRemoveBan)
	if !paused || !strings.Contains(why, store.SourceBreaker) {
		t.Fatalf("not paused: %q", why)
	}
	if p, _ := k.Store.PausedFor(k.Ctx, store.ScopeAll); p {
		t.Fatal("the breaker paused deletes")
	}
	al := k.Alerts.OfKind(alert.Breaker)
	if len(al) != 1 || !al[0].Priority || len(al[0].Buttons) != 1 || al[0].Buttons[0] != ledger.ButtonResume {
		t.Fatalf("breaker alerts %+v", al)
	}
	if err := k.Exec.Resume(k.Ctx); err != nil {
		t.Fatal(err)
	}
	k.Fire()
	if n := k.Fake.Count("Remove"); n != 3 {
		t.Fatalf("after [Resume] removed %d times, want 3", n)
	}
}

// TestPauseSurvivesRestart: a pause is in the store, so a restarted bot stays
// paused.
func TestPauseSurvivesRestart(t *testing.T) {
	k := modtest.New(t, "")
	pause(t, k, store.SourceBreaker, store.ScopeRemoveBan)
	removals(t, k, modtest.Other1M, modtest.G1)
	k.Reopen()
	if paused, why := k.Store.PausedFor(k.Ctx, store.ScopeRemoveBan); !paused || !strings.Contains(why, store.SourceBreaker) {
		t.Fatalf("after a restart: paused=%v %q", paused, why)
	}
	k.Fire()
	if k.Fake.Count("Remove") != 0 {
		t.Fatal("a restarted bot removed while paused")
	}
}

// TestUnreadablePauseFailsClosed: pause state that cannot be read counts as
// paused, for deletes too.
func TestUnreadablePauseFailsClosed(t *testing.T) {
	k := modtest.New(t, "")
	revokes(t, k, 1)
	db, err := sql.Open("sqlite", "file:"+k.DBPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `ALTER TABLE pause RENAME TO pause_unreadable`); err != nil {
		t.Fatal(err)
	}
	paused, why := k.Store.PausedFor(k.Ctx, store.ScopeAll)
	if !paused || !strings.Contains(why, "unreadable") {
		t.Fatalf("unreadable pause state: paused=%v %q", paused, why)
	}
	k.Fire()
	if k.Fake.Count("Revoke") != 0 {
		t.Fatal("an action fired with unreadable pause state")
	}
}

// TestQueuedActionsRecheckedOnResume: what waited out a pause is re-checked
// when [Resume] releases it.
func TestQueuedActionsRecheckedOnResume(t *testing.T) {
	k := modtest.New(t, "")
	pause(t, k, store.SourceBreaker, store.ScopeRemoveBan)
	removals(t, k, modtest.Other1M, modtest.G1)
	removals(t, k, modtest.Other2M, modtest.G1)
	k.Fire()
	k.Unban(modtest.Other1M) // an admin unbans one while removals are paused
	if err := k.Exec.Resume(k.Ctx); err != nil {
		t.Fatal(err)
	}
	k.Fire()
	if k.Fake.Count("Remove "+string(modtest.G1)+" "+string(modtest.Other1)) != 0 {
		t.Fatal("an unbanned member was removed after [Resume]")
	}
	if k.Fake.Count("Remove "+string(modtest.G1)+" "+string(modtest.Other2)) != 1 {
		t.Fatalf("calls %v", k.Fake.Calls())
	}
	if r := status(t, k, modtest.Other1M, store.ActRemove, modtest.G1); r.Status != store.Failed {
		t.Fatalf("unbanned member's row: %s", r.Status)
	}
}

// TestRestoreStartsPaused: a database restored from a backup starts with
// every action paused until [Resume].
func TestRestoreStartsPaused(t *testing.T) {
	k := modtest.New(t, "")
	revokes(t, k, 1)
	snap := filepath.Join(t.TempDir(), "restored.db")
	if err := k.Store.Snapshot(k.Ctx, snap); err != nil {
		t.Fatal(err)
	}
	if p, _ := k.Store.PausedFor(k.Ctx, store.ScopeAll); p {
		t.Fatal("taking a snapshot paused the running bot")
	}
	k.DBPath = snap // restore: the bot starts on the copy
	k.Reopen()
	paused, why := k.Store.PausedFor(k.Ctx, store.ScopeAll)
	if !paused || !strings.Contains(why, store.SourceRestore) {
		t.Fatalf("restored copy: paused=%v %q", paused, why)
	}
	k.Fire()
	if k.Fake.Count("Revoke") != 0 {
		t.Fatal("a restored bot acted before [Resume]")
	}
	if err := k.Exec.Resume(k.Ctx); err != nil {
		t.Fatal(err)
	}
	k.Fire()
	if k.Fake.Count("Revoke") != 1 {
		t.Fatalf("after [Resume]: %v", k.Fake.Calls())
	}
}

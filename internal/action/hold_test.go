package action_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
)

// TestPauseHoldsBanUntilResume: a pause covering removals and bans holds a
// new ban too (deletes continue): the spammer is not on the ban list while
// paused, the held ban survives a restart's recovery without being queued,
// and after [Resume] the first removal applies it and fires.
func TestPauseHoldsBanUntilResume(t *testing.T) {
	k := modtest.New(t, "")
	pause(t, k, store.SourceBreaker, store.ScopeRemoveBan)
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Fire()
	if k.Fake.Count("Revoke") != 1 || k.Fake.Count("Remove") != 0 {
		t.Fatalf("while paused: %v", k.Fake.Calls())
	}
	if k.Banned(modtest.SpammerM, "") {
		t.Fatal("a pause covering bans let a ban through")
	}
	ban := status(t, k, modtest.SpammerM, store.ActBan, store.BanEverywhere)
	if ban.Status != store.Intended {
		t.Fatalf("held ban row %s", ban.Status)
	}
	reps := k.Reports(ledger.KindAction)
	if len(reps) != 1 || !strings.Contains(reps[0].Text, "paused") {
		t.Fatalf("action reports %+v, want one saying removals and bans are paused", reps)
	}
	k.Reopen()
	if _, err := ledger.Recover(k.Ctx, k.Store, k.Log); err != nil {
		t.Fatalf("recovery with a held ban: %v", err)
	}
	if err := k.Exec.Resume(k.Ctx); err != nil {
		t.Fatal(err)
	}
	k.Fire()
	if !k.Banned(modtest.SpammerM, "") || removed(k, modtest.G1) != 1 {
		t.Fatalf("after [Resume]: banned %v, calls %v", k.Banned(modtest.SpammerM, ""), k.Fake.Calls())
	}
	if r := status(t, k, modtest.SpammerM, store.ActBan, store.BanEverywhere); r.Status != store.Requested {
		t.Fatalf("held ban row after [Resume]: %s", r.Status)
	}
	if n := k.Logged("held ban applied"); n != 1 {
		t.Fatalf("%d log lines for the held ban, want 1", n)
	}
}

// TestFixedRuleBeforeResumeBansNobody: a rule fixed while a pause held its
// actions bans nobody after [Resume]: the removals fail the re-check, the
// held ban fails with them, and a later sweep finds no ban to act on.
func TestFixedRuleBeforeResumeBansNobody(t *testing.T) {
	k := modtest.New(t, "")
	pause(t, k, store.SourceBreaker, store.ScopeRemoveBan)
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Fire()
	k.ReloadConfig(strings.Replace(modtest.Config, "confirmed: true", "confirmed: false", 1))
	if err := k.Exec.Resume(k.Ctx); err != nil {
		t.Fatal(err)
	}
	k.Fire()
	if k.Banned(modtest.SpammerM, "") || k.Fake.Count("Remove") != 0 {
		t.Fatalf("banned %v, calls %v", k.Banned(modtest.SpammerM, ""), k.Fake.Calls())
	}
	if r := status(t, k, modtest.SpammerM, store.ActBan, store.BanEverywhere); r.Status != store.Failed ||
		!strings.Contains(r.Reason, "no longer acts") {
		t.Fatalf("held ban row %s %q", r.Status, r.Reason)
	}
	if err := k.Enforcer.CheckPresent(k.Ctx, modtest.G2, "run1"); err != nil {
		t.Fatal(err)
	}
	k.Fire()
	if k.Fake.Count("Remove") != 0 {
		t.Fatalf("a sweep removed an innocent member: %v", k.Fake.Calls())
	}
}

// TestFullPauseReportSaysEverythingWaits: under /pause (every action) the
// action report does not claim the post is deleted.
func TestFullPauseReportSaysEverythingWaits(t *testing.T) {
	k := modtest.New(t, "")
	pause(t, k, store.SourceAdmin, store.ScopeAll)
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Fire()
	reps := k.Reports(ledger.KindAction)
	if len(reps) != 1 || !strings.Contains(reps[0].Text, "Every action is paused") || k.Fake.Count("Revoke") != 0 {
		t.Fatalf("reports %+v, calls %v", reps, k.Fake.Calls())
	}
}

// TestRemovalNotAgedOut: only a delete is bound by act_on_replay_max_age; a
// removal a pause held past it still fires after [Resume].
func TestRemovalNotAgedOut(t *testing.T) {
	k := modtest.New(t, "")
	pause(t, k, store.SourceBreaker, store.ScopeRemoveBan)
	m := k.Spam("M1", modtest.G1)
	m.Time = k.Clock.Now().Add(-40 * time.Hour)
	k.Deliver(m)
	k.Fire()
	k.Clock.Advance(10 * time.Hour) // the message is 50 hours old
	if err := k.Exec.Resume(k.Ctx); err != nil {
		t.Fatal(err)
	}
	k.Fire()
	if removed(k, modtest.G1) != 1 {
		t.Fatalf("a held removal aged out: %v", k.Fake.Calls())
	}
}

// TestHeldActionsDoNotSpin: an action a pause holds is not due, so the
// executor sleeps its idle poll instead of waking every few milliseconds.
func TestHeldActionsDoNotSpin(t *testing.T) {
	k := modtest.New(t, "")
	pause(t, k, store.SourceBreaker, store.ScopeRemoveBan)
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Fire()
	if _, ok, err := k.Store.NextWake(k.Ctx, true, false); err != nil || ok {
		t.Fatalf("held removals counted as due: ok %v, %v", ok, err)
	}
	if _, ok, err := k.Store.NextWake(k.Ctx, true, true); err != nil || !ok {
		t.Fatalf("queued removals not found when allowed: ok %v, %v", ok, err)
	}
	var calls atomic.Int64
	k.Exec.Now = func() time.Time { calls.Add(1); return k.Clock.Now() }
	ctx, cancel := context.WithTimeout(k.Ctx, 300*time.Millisecond)
	defer cancel()
	k.Exec.Run(ctx)
	if n := calls.Load(); n > 10 {
		t.Fatalf("the executor looped %d times in 300ms while paused", n)
	}
}

// TestUndoWhileRemovalInFlight: [Undo] committed while a removal's WhatsApp
// call is in flight keeps the row overturned (the call is still recorded on
// it) and raises a priority report naming the group to re-invite them to.
func TestUndoWhileRemovalInFlight(t *testing.T) {
	k := modtest.New(t, "")
	k.Deliver(k.Spam("M1", modtest.G1))
	reps := k.Reports(ledger.KindAction)
	if len(reps) != 1 {
		t.Fatalf("action reports %+v", reps)
	}
	admin := &pipeline.Admin{Store: k.Store, Config: k.Holder, Directory: k.Dir, Enforcer: k.Enforcer, Log: k.Log}
	var undoErr error
	undone := false
	k.Fake.OnCall = func(call string) {
		if !undone && strings.HasPrefix(call, "Remove "+string(modtest.G2)+" ") {
			undone = true
			_, undoErr = admin.Undo(k.Ctx, reps[0].ID, "admin")
		}
	}
	k.Fire()
	if !undone || undoErr != nil {
		t.Fatalf("undo ran %v: %v", undone, undoErr)
	}
	r := status(t, k, modtest.SpammerM, store.ActRemove, modtest.G2)
	if r.Status != store.Overturned || r.SentAt.IsZero() {
		t.Fatalf("in-flight removal row %s, sent %v", r.Status, r.SentAt)
	}
	race := k.Reports(ledger.KindUndoRace)
	if len(race) != 1 || !race[0].Priority || !strings.Contains(race[0].Text, "jobs") {
		t.Fatalf("undo race reports %+v", race)
	}
	if n := k.Logged("action went out while [Undo] ran"); n != 1 {
		t.Fatalf("%d log lines for the race, want 1", n)
	}
	if k.Banned(modtest.SpammerM, "") {
		t.Fatal("still banned after [Undo]")
	}
}

func removed(k *modtest.Kit, group client.JID) int {
	return k.Fake.Count("Remove " + string(group) + " " + string(modtest.Spammer))
}

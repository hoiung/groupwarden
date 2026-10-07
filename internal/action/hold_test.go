package action_test

import (
	"context"
	"strconv"
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
	// Under all_communities the ban covers the other community too.
	if !k.Banned(modtest.SpammerM, "set-b") {
		t.Fatal("the held ban was applied in its own community only")
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

// resumeAndFire presses [Resume] and fires the outbox.
func resumeAndFire(t *testing.T, k *modtest.Kit) {
	t.Helper()
	if err := k.Exec.Resume(k.Ctx); err != nil {
		t.Fatal(err)
	}
	k.Fire()
}

// heldSpam holds a ban: a breaker pause, then M1's spam post in G1.
func heldSpam(t *testing.T, k *modtest.Kit) {
	t.Helper()
	pause(t, k, store.SourceBreaker, store.ScopeRemoveBan)
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Fire()
	if k.Banned(modtest.SpammerM, "") {
		t.Fatal("a pause covering bans let a ban through")
	}
}

// TestAdminBanDuringPauseAppliesAtOnce: an admin's ban is never folded into a
// ban a pause holds: the member is banned at once and the held ban is settled
// by it, while the removals still wait for [Resume].
func TestAdminBanDuringPauseAppliesAtOnce(t *testing.T) {
	k := modtest.New(t, "")
	heldSpam(t, k)
	k.Ban(modtest.SpammerM)
	if !k.Banned(modtest.SpammerM, "") {
		t.Fatal("an admin's ban during a pause was swallowed by the held ban")
	}
	rows := k.Find(modtest.SpammerM, store.ActBan, store.BanEverywhere)
	if len(rows) != 2 || rows[0].Status != store.Requested || !strings.Contains(rows[0].Reason, "in force through ledger row") ||
		rows[1].Status != store.Requested {
		t.Fatalf("ban rows %+v, want the held one settled by the admin's", rows)
	}
	if removed(k, modtest.G1) != 0 {
		t.Fatalf("a removal went out while paused: %v", k.Fake.Calls())
	}
	resumeAndFire(t, k)
	if removed(k, modtest.G1) != 1 {
		t.Fatalf("after [Resume]: %v", k.Fake.Calls())
	}
}

// TestLaterPostKeepsHeldBanWhenFirstRefuted: a second spam post during the
// pause reuses the first post's removal and held ban. A config change that
// refutes only the first post leaves the second counting, so after [Resume]
// the member is banned and removed; with both refuted, neither happens.
func TestLaterPostKeepsHeldBanWhenFirstRefuted(t *testing.T) {
	both := strings.Replace(modtest.Config, "    - name: lure\n      action: delete_remove_ban\n",
		"    - name: lure\n      action: delete_remove_ban\n      confirmed: true\n", 1)
	pitchOff := strings.Replace(both, "confirmed: true", "confirmed: false", 1)
	for _, tc := range []struct {
		name   string
		reload string
		acts   bool
	}{
		{"first refuted", pitchOff, true},
		{"both refuted", strings.Replace(pitchOff, "confirmed: true", "confirmed: false", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := modtest.NewConfig(t, both)
			pause(t, k, store.SourceBreaker, store.ScopeRemoveBan)
			k.Deliver(k.Spam("M1", modtest.G1))
			k.Deliver(k.Msg("M2", modtest.G1, modtest.Spammer, "crypto, inbox me"))
			k.Fire()
			if n := len(k.Find(modtest.SpammerM, store.ActBan, store.BanEverywhere)); n != 1 || k.Fake.Count("Revoke") != 2 {
				t.Fatalf("%d held ban rows, calls %v: want both posts deleted and one held ban", n, k.Fake.Calls())
			}
			k.ReloadConfig(tc.reload)
			resumeAndFire(t, k)
			if k.Banned(modtest.SpammerM, "") != tc.acts || (removed(k, modtest.G1) == 1) != tc.acts {
				t.Fatalf("banned %v, calls %v; want acted %v", k.Banned(modtest.SpammerM, ""), k.Fake.Calls(), tc.acts)
			}
		})
	}
}

// TestHeldBanAppliesUnderTheCurrentScope: a ban held under one bans.scope is
// applied under the scope in force at [Resume].
func TestHeldBanAppliesUnderTheCurrentScope(t *testing.T) {
	k := modtest.New(t, "")
	heldSpam(t, k)
	k.Reload("bans:\n  scope: per_community\n")
	resumeAndFire(t, k)
	if !k.Banned(modtest.SpammerM, string(modtest.Community)) || removed(k, modtest.G1) != 1 {
		t.Fatalf("banned in the community %v, calls %v", k.Banned(modtest.SpammerM, string(modtest.Community)), k.Fake.Calls())
	}
	if k.Banned(modtest.SpammerM, "set-b") {
		t.Fatal("the held ban was applied in the scope it was decided under (every community)")
	}
	if r := status(t, k, modtest.SpammerM, store.ActBan, store.BanEverywhere); r.Status != store.Requested {
		t.Fatalf("held ban row %s %q", r.Status, r.Reason)
	}
}

// TestHeldBanSurvivesTheBotLeavingTheGroup: a post is judged in the community
// it was decided in, so the bot leaving its group during the pause does not
// refute it: the member is banned and removed from the groups the bot is
// still in.
func TestHeldBanSurvivesTheBotLeavingTheGroup(t *testing.T) {
	k := modtest.New(t, "")
	heldSpam(t, k)
	k.Deliver(&client.GroupChange{Group: modtest.G1, Time: k.Clock.Now(), Left: []client.JID{modtest.Bot}})
	resumeAndFire(t, k)
	if !k.Banned(modtest.SpammerM, "") || removed(k, modtest.G2) != 1 {
		t.Fatalf("banned %v, calls %v", k.Banned(modtest.SpammerM, ""), k.Fake.Calls())
	}
}

// TestHeldBanFailsWhenItsCommunityStopsEnforcing: a ban held in a community
// that is in shadow mode, or no longer configured, by [Resume] fails, so
// nobody is banned or removed.
func TestHeldBanFailsWhenItsCommunityStopsEnforcing(t *testing.T) {
	const entry = "    name: community a\n"
	for _, tc := range []struct{ name, reload, reason string }{
		{"shadow", strings.Replace(modtest.Config, entry, entry+"    mode: shadow\n", 1), "is in shadow mode"},
		{"not configured", strings.Replace(modtest.Config, "  \""+string(modtest.Community)+"\":\n"+entry, "", 1),
			"is no longer a configured community"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := modtest.New(t, "")
			heldSpam(t, k)
			k.ReloadConfig(tc.reload)
			resumeAndFire(t, k)
			if k.Banned(modtest.SpammerM, "") || k.Fake.Count("Remove") != 0 {
				t.Fatalf("banned %v, calls %v", k.Banned(modtest.SpammerM, ""), k.Fake.Calls())
			}
			if r := status(t, k, modtest.SpammerM, store.ActBan, store.BanEverywhere); r.Status != store.Failed ||
				!strings.Contains(r.Reason, tc.reason) {
				t.Fatalf("held ban row %s %q", r.Status, r.Reason)
			}
			if n := k.Logged("held ban failed"); n != 1 {
				t.Fatalf("%d log lines for the failed held ban, want 1", n)
			}
		})
	}
}

// TestHeldBanOutlivesRetention: a pause longer than the evidence window keeps
// the copies its open actions stand for, a later post's and attachments
// included (purges spare them and the held ban), and `member forget` keeps a
// held ban as it keeps an active one; after [Resume] the member is banned and
// removed either way.
func TestHeldBanOutlivesRetention(t *testing.T) {
	for _, forget := range []bool{false, true} {
		t.Run("forget "+strconv.FormatBool(forget), func(t *testing.T) {
			k := modtest.New(t, "")
			k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
				return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
			}
			pause(t, k, store.SourceBreaker, store.ScopeRemoveBan)
			m := k.Spam("M1", modtest.G1)
			m.Media = &client.Media{Kind: "image", MimeType: "image/jpeg", FileName: "offer.jpg", Size: 8, Raw: []byte("raw")}
			k.Deliver(m)
			k.Deliver(k.Spam("M2", modtest.G1)) // reuses M1's held ban
			if n, err := k.Media.Fetch(k.Ctx); err != nil || n != 1 {
				t.Fatalf("fetched %d (%v)", n, err)
			}
			k.Fire()
			k.Clock.Advance(400 * 24 * time.Hour) // past the evidence window and the action log's
			cutoff := k.Clock.Now().Add(-24 * time.Hour)
			if n, files, err := k.Store.PurgeEvidence(k.Ctx, cutoff); err != nil || n != 0 || len(files) != 0 {
				t.Fatalf("purged %d copies and %d files an open action needs (%v)", n, len(files), err)
			}
			// The delete already went out (deletes continue), so its row goes;
			// the held ban and the queued removals stay.
			if _, err := k.Store.PurgeLedger(k.Ctx, cutoff); err != nil {
				t.Fatal(err)
			}
			if n := len(k.Find(modtest.SpammerM, store.ActBan, store.BanEverywhere)); n != 1 {
				t.Fatalf("%d held ban rows after the ledger purge, want 1", n)
			}
			if forget {
				f, err := k.Store.ForgetMember(k.Ctx, modtest.SpammerM.IDs())
				if err != nil || f.HeldBans != 1 || f.Evidence == 0 {
					t.Fatalf("forget: %+v, %v", f, err)
				}
			}
			resumeAndFire(t, k)
			if !k.Banned(modtest.SpammerM, "") || removed(k, modtest.G1) != 1 {
				t.Fatalf("banned %v, calls %v", k.Banned(modtest.SpammerM, ""), k.Fake.Calls())
			}
		})
	}
}

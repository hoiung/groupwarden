package pipeline_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
)

// TestCrossPostRemovesOnce: a spammer posting in two groups before anything
// fires is removed once per group (the second post reuses the first's open
// removals and ban), while each post is deleted and reported.
func TestCrossPostRemovesOnce(t *testing.T) {
	k := modtest.New(t, "")
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Clock.Advance(time.Second)
	k.Deliver(k.Spam("M2", modtest.G2))
	k.Fire()
	for _, g := range []client.JID{modtest.Community, modtest.G1, modtest.G2} {
		if n := removed(k, g, modtest.Spammer); n != 1 {
			t.Fatalf("removed from %s %d times: %v", g, n, k.Fake.Calls())
		}
	}
	if n := k.Fake.Count("Revoke"); n != 2 {
		t.Fatalf("%d deletes, want one per post", n)
	}
	if n := len(k.Reports(ledger.KindAction)); n != 2 {
		t.Fatalf("%d action reports, want one per post", n)
	}
	if n := len(k.Find(modtest.SpammerM, store.ActBan, store.BanEverywhere)); n != 1 {
		t.Fatalf("%d ban rows, want 1", n)
	}
}

// TestEditOfSpamActionedOnce: a spam post and its edit, both decided before
// anything fires, delete the message once and report once.
func TestEditOfSpamActionedOnce(t *testing.T) {
	k := modtest.New(t, "")
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Clock.Advance(30 * time.Second)
	edit := k.Spam("E1", modtest.G1)
	edit.TargetID, edit.IsEdit = "M1", true
	k.Deliver(edit)
	k.Fire()
	if n := k.Fake.Count("Revoke " + string(modtest.G1) + " " + string(modtest.Spammer) + " M1"); n != 1 {
		t.Fatalf("deleted M1 %d times: %v", n, k.Fake.Calls())
	}
	if n := len(k.Reports(ledger.KindAction)); n != 1 {
		t.Fatalf("%d action reports, want 1", n)
	}
}

// TestShadowSweepThenEnforce: in a shadow-mode set, each sweep that finds a
// banned member is one shadow row and one "would be removed" report (no
// [Undo]: nothing was done); once the set enforces, the next sweep removes
// them.
func TestShadowSweepThenEnforce(t *testing.T) {
	shadow := strings.Replace(modtest.Config, "    name: set b\n", "    name: set b\n    mode: shadow\n", 1)
	k := modtest.NewConfig(t, shadow)
	k.Ban(modtest.MemberM)
	for _, run := range []string{"run1", "run2", "run3"} {
		if err := k.Enforcer.CheckPresent(k.Ctx, modtest.GB, run); err != nil {
			t.Fatal(err)
		}
	}
	k.Fire()
	if rows := k.Find(modtest.MemberM, store.ActRemove, modtest.GB); len(rows) != 1 || rows[0].Mode != store.ModeShadow {
		t.Fatalf("shadow rows %+v, want one", rows)
	}
	reps := k.Reports(ledger.KindWouldRemove)
	if len(reps) != 1 || len(reps[0].Buttons) != 0 || !strings.Contains(reps[0].Text, "shadow mode") ||
		len(k.Reports(ledger.KindBannedRejoin)) != 0 {
		t.Fatalf("would-remove reports %+v, banned-rejoin %d", reps, len(k.Reports(ledger.KindBannedRejoin)))
	}
	k.ReloadConfig(modtest.Config)
	if err := k.Enforcer.CheckPresent(k.Ctx, modtest.GB, "run4"); err != nil {
		t.Fatal(err)
	}
	k.Fire()
	if n := removed(k, modtest.GB, modtest.Member); n != 1 {
		t.Fatalf("removed %d times after going to enforce: %v", n, k.Fake.Calls())
	}
}

// TestShadowJoinNotReportedRemoved: a banned member joining a group in a
// shadow-mode set is reported as "would be removed", without [Undo].
func TestShadowJoinNotReportedRemoved(t *testing.T) {
	shadow := strings.Replace(modtest.Config, "    name: set b\n", "    name: set b\n    mode: shadow\n", 1)
	k := modtest.NewConfig(t, shadow)
	k.Ban(modtest.SpammerM)
	k.Deliver(change(k, modtest.GB, "", []client.JID{modtest.Spammer}, nil))
	k.Fire()
	reps := k.Reports(ledger.KindWouldRemove)
	if len(reps) != 1 || len(reps[0].Buttons) != 0 || k.Fake.Count("Remove") != 0 ||
		len(k.Reports(ledger.KindBannedRejoin))+len(k.Reports(ledger.KindUnknownActor)) != 0 {
		t.Fatalf("reports %+v, calls %v", reps, k.Fake.Calls())
	}
}

// TestBannedAdminReportedOncePerBan: a banned member who is a current admin
// is reported once, not on every sweep.
func TestBannedAdminReportedOncePerBan(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.AdminM)
	for _, run := range []string{"run1", "run2", "run3"} {
		if err := k.Enforcer.CheckPresent(k.Ctx, modtest.G1, run); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(k.Reports(ledger.KindAdminSpared)); n != 1 {
		t.Fatalf("%d admin_spared reports after 3 sweeps, want 1", n)
	}
	if k.Fake.Count("Remove") != 0 {
		t.Fatalf("removed a current admin: %v", k.Fake.Calls())
	}
}

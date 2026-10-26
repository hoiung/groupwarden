package app

import (
	"context"
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/telegram"
)

// banApp is an App over the moderation kit with the admin chat's ban
// controls and a sweep request channel of its own to read.
func banApp(t *testing.T) (*App, *modtest.Kit) {
	t.Helper()
	k := modtest.New(t, "")
	return &App{Adapter: k.Fake, Store: k.Store, Config: k.Holder, Directory: k.Dir, Log: k.Log, Clock: SystemClock{},
		Admin:    &pipeline.Admin{Store: k.Store, Config: k.Holder, Directory: k.Dir, Enforcer: k.Enforcer, Log: k.Log},
		sweepNow: make(chan struct{}, 1)}, k
}

// TestBanSweepsNow (AC 5.3): every /ban from the admin chat asks for a sweep
// at once (the request the bot makes at connect), so a banned member who is
// present is removed without waiting for reconcile.interval_minutes: by
// number, and as a reply to a report, which is the [Ban] button's code. A
// number already banned gets no second ban but asks for a sweep all the
// same: every /ban sweeps and cleans up.
func TestBanSweepsNow(t *testing.T) {
	ctx := context.Background()
	by := telegram.Actor{ID: 501, Name: "Ann"}
	t.Run("by-number", func(t *testing.T) {
		a, k := banApp(t)
		reply, err := controls{a}.BanNumber(ctx, modtest.SpammerPhone, by)
		if err != nil || !strings.HasPrefix(reply, "Banned by Ann:") || !k.Banned(modtest.SpammerM, "") {
			t.Fatalf("reply %q %v", reply, err)
		}
		if len(a.sweepNow) != 1 {
			t.Fatalf("%d sweeps pending after /ban <number>, want 1", len(a.sweepNow))
		}
	})
	t.Run("by-reply", func(t *testing.T) {
		a, k := banApp(t)
		m := k.Msg("W1", modtest.G1, modtest.Spammer, "crypto: inbox me")
		m.SenderAlt = modtest.SpammerPhone
		k.Deliver(m)
		reps := k.Reports(ledger.KindWouldHaveActed)
		if len(reps) != 1 || len(a.sweepNow) != 0 {
			t.Fatalf("reports %+v, %d sweeps pending, want one watch-only report and none", reps, len(a.sweepNow))
		}
		reply, err := controls{a}.Ban(ctx, reps[0].ID, by)
		if err != nil || !strings.HasPrefix(reply, "Banned by Ann (#") || !k.Banned(modtest.SpammerM, "") {
			t.Fatalf("reply %q %v", reply, err)
		}
		if len(a.sweepNow) != 1 {
			t.Fatalf("%d sweeps pending after /ban as a reply ([Ban]), want 1", len(a.sweepNow))
		}
	})
	t.Run("already-banned-still-sweeps", func(t *testing.T) {
		a, k := banApp(t)
		k.Ban(modtest.SpammerM)
		reply, err := controls{a}.BanNumber(ctx, modtest.SpammerPhone, by)
		if err != nil || !strings.Contains(reply, "already banned everywhere, so no ban was added") {
			t.Fatalf("reply %q %v", reply, err)
		}
		if len(a.sweepNow) != 1 {
			t.Fatalf("%d sweeps pending after /ban <number> of a number already banned, want 1", len(a.sweepNow))
		}
	})
}

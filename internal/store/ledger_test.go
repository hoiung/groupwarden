package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestHeldBanSettlesOnce: HeldBan matches a held ban only in the communities
// it covers; an admin's ban settles the held bans its scope covers and no
// other; applying or failing a held ban changes only a row still held.
func TestHeldBanSettlesOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := openAt(t, &now)
	const x = "99999000000444@lid"
	held := func(chat string) LedgerRow {
		t.Helper()
		var id int64
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			var err error
			id, _, err = InsertLedger(ctx, tx, LedgerRow{Action: ActBan, Chat: chat, Target: x, TriggerID: "T-" + chat,
				Community: chat, Mode: ModeEnforce, ConfigHash: "h"}, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		r, err := s.Ledger(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	statusOf := func(r LedgerRow) Status {
		t.Helper()
		got, err := s.Ledger(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	c1, c2 := held("c1"), held("c2")
	for community, want := range map[string]bool{"c1": true, "c2": true, "c3": false, "": true} {
		if got, err := s.HeldBan(ctx, []string{x}, community); err != nil || got != want {
			t.Errorf("HeldBan in %q = %v (%v), want %v", community, got, err, want)
		}
	}
	// An admin's ban in c1 settles the held ban there, not the one in c2.
	if err := s.Write(ctx, func(tx *sql.Tx) error { return SettleHeldBansIn(ctx, tx, x, "c1", 99, now) }); err != nil {
		t.Fatal(err)
	}
	if statusOf(c1) != Requested || statusOf(c2) != Intended {
		t.Fatalf("after an admin's ban in c1: c1 %s, c2 %s; want requested, intended", statusOf(c1), statusOf(c2))
	}
	// The settled row is neither failed nor applied again.
	if err := s.FailHeldBan(ctx, c1.ID, "refuted"); err != nil || statusOf(c1) != Requested {
		t.Fatalf("failing a settled held ban: %s (%v)", statusOf(c1), err)
	}
	if applied, err := s.ApplyHeldBan(ctx, c1, []string{"c1"}); err != nil || applied {
		t.Fatalf("applying a settled held ban: applied %v (%v)", applied, err)
	}
	if _, banned, err := s.FindBan(ctx, []string{x}, "c1"); err != nil || banned {
		t.Fatalf("a settled held ban wrote a ban entry: %v (%v)", banned, err)
	}
	// The held ban in c2 applies once, and then cannot be failed.
	for i, want := range []bool{true, false} {
		if applied, err := s.ApplyHeldBan(ctx, c2, []string{"c2"}); err != nil || applied != want {
			t.Fatalf("apply %d: applied %v (%v), want %v", i+1, applied, err, want)
		}
	}
	if _, banned, err := s.FindBan(ctx, []string{x}, "c2"); err != nil || !banned {
		t.Fatalf("the applied held ban is not on the ban list: %v (%v)", banned, err)
	}
	if err := s.FailHeldBan(ctx, c2.ID, "refuted"); err != nil || statusOf(c2) != Requested {
		t.Fatalf("failing an applied held ban: %s (%v)", statusOf(c2), err)
	}
}

// TestRowChangesOnlyFromIntended: moving a queued row to shadow, or
// finishing it, changes only a row still at intended. A row [Undo] overturned
// meanwhile keeps its status and mode, and the caller is told nothing changed
// (so no report about it is written).
func TestRowChangesOnlyFromIntended(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := openAt(t, &now)
	for _, c := range []struct {
		name   string
		change func(tx *sql.Tx, id int64) (bool, error)
	}{
		{"to shadow", func(tx *sql.Tx, id int64) (bool, error) { return ToShadowIn(ctx, tx, id, "shadow mode", now) }},
		{"finish", func(tx *sql.Tx, id int64) (bool, error) {
			return FinishIn(ctx, tx, id, Failed, "spared", 0, time.Time{}, now)
		}},
	} {
		var live, undone int64
		var liveChanged, undoneChanged bool
		err := s.Write(ctx, func(tx *sql.Tx) error {
			var err error
			for i, id := range []*int64{&live, &undone} {
				if *id, _, err = InsertLedger(ctx, tx, LedgerRow{Action: ActRemove, Chat: "99999000000111@g.us",
					Target: "99999000000444@lid", TriggerID: c.name + string(rune('A'+i)), Community: "c",
					Mode: ModeEnforce, ConfigHash: "h"}, now); err != nil {
					return err
				}
			}
			if _, err := Overturn(ctx, tx, []int64{undone}, "undo", now); err != nil {
				return err
			}
			if liveChanged, err = c.change(tx, live); err != nil {
				return err
			}
			undoneChanged, err = c.change(tx, undone)
			return err
		})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !liveChanged || undoneChanged {
			t.Errorf("%s: changed queued row %v, overturned row %v; want true, false", c.name, liveChanged, undoneChanged)
		}
		r, err := s.Ledger(ctx, undone)
		if err != nil || r.Status != Overturned || r.Mode != ModeEnforce {
			t.Errorf("%s: overturned row became %s/%s (%v)", c.name, r.Status, r.Mode, err)
		}
	}
}

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
	if err := s.Write(ctx, func(tx *sql.Tx) error { return SettleHeldBansIn(ctx, tx, []string{x}, "c1", 99, now) }); err != nil {
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

// TestHeldBanFoundByPhone: a held ban that names the member by LID is found,
// settled and dropped by the phone it keeps in address, as for someone who
// joined since the group list was last read (the directory has no pair for
// them, so a lookup by number has the phone alone).
func TestHeldBanFoundByPhone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := openAt(t, &now)
	const lid, phone, other = "99999000000444@lid", "447700900123@s.whatsapp.net", "447700900456@s.whatsapp.net"
	held := map[string]int64{}
	for _, chat := range []string{"c1", "c2", "c3"} {
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			var err error
			held[chat], _, err = InsertLedger(ctx, tx, LedgerRow{Action: ActBan, Chat: chat, Target: lid, Address: phone,
				TriggerID: "T-" + chat, Community: chat, Mode: ModeEnforce, ConfigHash: "h"}, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	statusOf := func(chat string) Status {
		t.Helper()
		got, err := s.Ledger(ctx, held[chat])
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	for ids, want := range map[string]bool{phone: true, other: false} {
		if got, err := s.HeldBan(ctx, []string{ids}, ""); err != nil || got != want {
			t.Errorf("HeldBan(%s) = %v (%v), want %v", ids, got, err, want)
		}
	}
	if err := s.Write(ctx, func(tx *sql.Tx) error { return SettleHeldBansIn(ctx, tx, []string{phone}, "c1", 99, now) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *sql.Tx) error { return DropHeldBans(ctx, tx, []string{phone}, "c2", "unbanned", now) }); err != nil {
		t.Fatal(err)
	}
	if statusOf("c1") != Requested || statusOf("c2") != Overturned || statusOf("c3") != Intended {
		t.Fatalf("settled by phone in c1, dropped by phone in c2: c1 %s, c2 %s, c3 %s; want requested, overturned, intended",
			statusOf("c1"), statusOf("c2"), statusOf("c3"))
	}
}

// TestRetryKeepsAnOverturnedRow: a call that fails after [Undo] overturned its
// row (mid-call) does not overwrite the row: its status, the reason saying who
// overturned it, and its attempt count stay.
func TestRetryKeepsAnOverturnedRow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := openAt(t, &now)
	var id int64
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if id, _, err = InsertLedger(ctx, tx, LedgerRow{Action: ActRemove, Chat: "99999000000111@g.us",
			Target: "99999000000444@lid", TriggerID: "R1", Community: "c", Mode: ModeEnforce, ConfigHash: "h"}, now); err != nil {
			return err
		}
		_, err = Overturn(ctx, tx, []int64{id}, "undone in the admin chat by Ann", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Retry(ctx, id, now.Add(time.Minute), "network timeout"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Ledger(ctx, id)
	if err != nil || r.Status != Overturned || r.Reason != "undone in the admin chat by Ann" || r.Attempts != 0 {
		t.Fatalf("row after a failed call: %s %q attempts=%d (%v); want overturned, the undo reason, 0", r.Status,
			r.Reason, r.Attempts, err)
	}
}

// TestActionReportedOnlyForTheSameAction: a not_done report silences another
// only for the same action, group, member and message, and only one written
// since the given time; a report of another kind on the same action silences
// nothing.
func TestActionReportedOnlyForTheSameAction(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s := openAt(t, &now)
	const g1, g2 = "99999000000111@g.us", "99999000000222@g.us"
	const x, y = "99999000000444@lid", "99999000000777@lid"
	told := LedgerRow{Action: ActRemove, Chat: g1, Target: x, TriggerID: "T1", Community: "c", Mode: ModeEnforce,
		ConfigHash: "h"}
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		id, _, err := InsertLedger(ctx, tx, told, now)
		if err != nil {
			return err
		}
		_, err = InsertReport(ctx, tx, Report{Kind: "not_done", Community: "c", Subject: x, Text: "do it by hand"},
			[]int64{id}, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	with := func(f func(*LedgerRow)) LedgerRow {
		r := told
		f(&r)
		return r
	}
	for _, c := range []struct {
		name  string
		kind  string
		row   LedgerRow
		since time.Time
		want  bool
	}{
		{"the same action", "not_done", told, now.Add(-time.Hour), true},
		{"written at since", "not_done", told, now, true},
		{"another kind", "action", told, now.Add(-time.Hour), false},
		{"another action", "not_done", with(func(r *LedgerRow) { r.Action = ActReject }), now.Add(-time.Hour), false},
		{"another group", "not_done", with(func(r *LedgerRow) { r.Chat = g2 }), now.Add(-time.Hour), false},
		{"another member", "not_done", with(func(r *LedgerRow) { r.Target = y }), now.Add(-time.Hour), false},
		{"another message", "not_done", with(func(r *LedgerRow) { r.MsgID = "M2" }), now.Add(-time.Hour), false},
		{"written before since", "not_done", told, now.Add(time.Millisecond), false},
	} {
		var got bool
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			var err error
			got, err = ActionReported(ctx, tx, c.kind, c.row, c.since)
			return err
		}); err != nil || got != c.want {
			t.Errorf("%s: reported %v (%v), want %v", c.name, got, err, c.want)
		}
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

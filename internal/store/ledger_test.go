package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

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

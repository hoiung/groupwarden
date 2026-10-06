package action_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
)

// TestStateChangeAndItsReportTogether: each step that changes a row's or a
// ban's state and reports it writes both in one transaction. While either
// half cannot be written the step fails and writes neither (written apart, a
// report could be lost, since nothing decides the row or the ban again, or
// repeated, since the next step decides it again); once both can, the state
// changes and the admins get exactly one report.
func TestStateChangeAndItsReportTogether(t *testing.T) {
	unknown := client.Member{Phone: "447700900456@s.whatsapp.net"}
	removal := func(k *modtest.Kit, m client.Member) store.LedgerRow {
		t.Helper()
		rows := k.Find(m, store.ActRemove, modtest.G1)
		if len(rows) != 1 {
			t.Fatalf("removal rows %+v", rows)
		}
		return rows[0]
	}
	for _, c := range []struct {
		name    string
		kind    string
		state   string // the table the state change writes
		setup   func(k *modtest.Kit)
		step    func(k *modtest.Kit) error
		changed func(k *modtest.Kit) bool
	}{
		{
			name:  "removal moved to shadow",
			kind:  ledger.KindWouldRemove,
			state: "ledger",
			setup: func(k *modtest.Kit) {
				removals(t, k, modtest.MemberM, modtest.G1)
				k.ReloadConfig(strings.Replace(modtest.Config, "    name: community a\n",
					"    name: community a\n    mode: shadow\n", 1))
			},
			step:    stepOnce,
			changed: func(k *modtest.Kit) bool { return removal(k, modtest.MemberM).Mode == store.ModeShadow },
		},
		{
			name:  "removal of a current admin not sent",
			kind:  ledger.KindAdminSpared,
			state: "ledger",
			setup: func(k *modtest.Kit) {
				removals(t, k, modtest.MemberM, modtest.G1)
				k.Dir.Apply(&client.GroupChange{Group: modtest.G1, Promoted: []client.JID{modtest.Member}})
			},
			step:    stepOnce,
			changed: func(k *modtest.Kit) bool { return removal(k, modtest.MemberM).Status == store.Failed },
		},
		{
			name:  "ban phone number not resolved to a LID",
			kind:  ledger.KindLIDUnresolved,
			state: "bans",
			setup: func(k *modtest.Kit) { k.Ban(unknown) },
			step:  func(k *modtest.Kit) error { return k.Enforcer.ResolveBans(k.Ctx) },
			changed: func(k *modtest.Kit) bool {
				b, ok, err := k.Store.FindBan(k.Ctx, unknown.IDs(), "")
				if err != nil || !ok {
					t.Fatalf("ban: %v %v", ok, err)
				}
				return b.LIDFailed
			},
		},
	} {
		for _, failing := range []string{"reports", c.state} {
			t.Run(c.name+"/"+failing+" not writable", func(t *testing.T) {
				k := modtest.New(t, "")
				c.setup(k)
				lift := modtest.FailWrites(t, k.Store, failing, "")
				if err := c.step(k); err == nil {
					t.Fatalf("the step succeeded while %s could not be written", failing)
				}
				if c.changed(k) {
					t.Fatalf("the state changed while %s could not be written", failing)
				}
				if n := len(k.Reports(c.kind)); n != 0 {
					t.Fatalf("%d reports while %s could not be written", n, failing)
				}
				lift()
				for range 2 { // the second run finds nothing left to do
					if err := c.step(k); err != nil {
						t.Fatal(err)
					}
				}
				if !c.changed(k) {
					t.Fatal("the state did not change once it could be written")
				}
				if n := len(k.Reports(c.kind)); n != 1 {
					t.Fatalf("%d %s reports, want exactly 1", n, c.kind)
				}
			})
		}
	}
}

// TestSettleReportsOnlyAChange: the report is written, linked to its row,
// only when the change says the row changed (a row [Undo] overturned
// meanwhile is not reported on), and the admin chat is woken only then.
func TestSettleReportsOnlyAChange(t *testing.T) {
	k := modtest.New(t, "")
	removals(t, k, modtest.MemberM, modtest.G1)
	row := k.Find(modtest.MemberM, store.ActRemove, modtest.G1)[0]
	woken := 0
	k.Exec.Reported = func() { woken++ }
	rep := &store.Report{Kind: ledger.KindAdminSpared, Text: "spared"}
	for _, c := range []struct {
		name           string
		changed        bool
		rep            *store.Report
		reports, wakes int
	}{{"unchanged", false, rep, 0, 0}, {"changed, nothing to report", true, nil, 0, 0}, {"changed", true, rep, 1, 1}} {
		got, err := k.Exec.Settle(k.Ctx, row.ID, func(*sql.Tx, time.Time) (bool, error) { return c.changed, nil }, c.rep)
		if err != nil || got != c.changed {
			t.Fatalf("%s: changed %v (%v)", c.name, got, err)
		}
		reps := k.Reports(ledger.KindAdminSpared)
		if len(reps) != c.reports || woken != c.wakes {
			t.Fatalf("%s: %d reports, woken %d; want %d and %d", c.name, len(reps), woken, c.reports, c.wakes)
		}
	}
	linked, err := k.Store.LedgerForReport(k.Ctx, k.Reports(ledger.KindAdminSpared)[0].ID)
	if err != nil || len(linked) != 1 || linked[0].ID != row.ID {
		t.Fatalf("the report is linked to %+v (%v), want row %d", linked, err, row.ID)
	}
}

// stepOnce runs one executor step.
func stepOnce(k *modtest.Kit) error {
	_, err := k.Exec.Step(k.Ctx)
	return err
}

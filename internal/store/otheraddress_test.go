package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// otherAddressTables are the row counts the other-address lookup reads at
// today's size (#8, live database 2026-10-26): recent posts, evidence rows,
// action-log rows and the ban rows among them, ban list entries.
var otherAddressTables = struct{ seen, evidence, ledger, banRows, bans int }{58, 9, 102, 36, 33}

// fillOtherAddressTables writes scale times today's rows, every member with a
// LID and (for every other post, and every ban) a phone number, all synthetic
// IDs under the never-assigned 99999000000 prefix, then checks the counts.
func fillOtherAddressTables(tb testing.TB, s *Store, scale int) {
	tb.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	c := otherAddressTables
	lid := func(i int) string { return fmt.Sprintf("99999000000%d@lid", 100000+i) }
	phone := func(i int) string { return fmt.Sprintf("99999000000%d", 100000+i) + "@" + "s.whatsapp.net" }
	err := s.Write(ctx, func(tx *sql.Tx) error {
		for i := 0; i < c.seen*scale; i++ {
			alt, at := "", now.Add(-time.Duration(i)*time.Minute).UnixMilli()
			if i%2 == 0 {
				alt = phone(i)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO seen (dedupe_key, chat, msg_id, server_time, decided_at, sender, sender_alt)
VALUES (?, '99999000000111@g.us', ?, ?, ?, ?, ?)`, fmt.Sprintf("msg|P%d", i), fmt.Sprintf("P%d", i), at, at, lid(i), alt); err != nil {
				return err
			}
		}
		for i := 0; i < c.evidence*scale; i++ {
			if _, err := InsertEvidence(ctx, tx, Evidence{Chat: "99999000000111@g.us", Community: "99999000000999@g.us",
				Sender: lid(i), SenderAlt: phone(i), Subject: lid(i), MsgID: fmt.Sprintf("E%d", i), TargetID: fmt.Sprintf("E%d", i),
				MsgTime: now, Fields: "{}", Normalised: "{}"}, now); err != nil {
				return err
			}
		}
		for i := 0; i < c.ledger*scale; i++ {
			r := LedgerRow{Action: ActRevoke, Chat: "99999000000111@g.us", Target: lid(i), TriggerID: fmt.Sprintf("T%d", i),
				Community: "99999000000999@g.us", Mode: ModeEnforce, Status: Requested, ConfigHash: "h", MsgID: fmt.Sprintf("M%d", i)}
			ban := i < c.banRows*scale
			if ban {
				r.Action, r.Chat, r.Address, r.MsgID = ActBan, BanEverywhere, phone(i), ""
			}
			id, _, err := InsertLedger(ctx, tx, r, now)
			if err != nil {
				return err
			}
			if ban && i < c.bans*scale {
				if err := AddBan(ctx, tx, Ban{Member: lid(i), Scope: BanEverywhere, LID: lid(i), Phone: phone(i), Reason: "pitch",
					LedgerID: id}, now); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM seen`:                        c.seen * scale,
		`SELECT COUNT(*) FROM evidence`:                    c.evidence * scale,
		`SELECT COUNT(*) FROM ledger`:                      c.ledger * scale,
		`SELECT COUNT(*) FROM ledger WHERE action = 'ban'`: c.banRows * scale,
		`SELECT COUNT(*) FROM bans`:                        c.bans * scale,
	} {
		var n int
		if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil || n != want {
			tb.Fatalf("%s = %d (%v), want %d", q, n, err, want)
		}
	}
}

// BenchmarkOtherAddress: the other-address lookup at today's table sizes and
// at 100 times them, for an address nothing pairs (the lookup reads every
// source and finds nothing: its worst case).
func BenchmarkOtherAddress(b *testing.B) {
	ctx := context.Background()
	for _, scale := range []int{1, 100} {
		s, err := Open(ctx, filepath.Join(b.TempDir(), "g.db"), Options{})
		if err != nil {
			b.Fatal(err)
		}
		fillOtherAddressTables(b, s, scale)
		b.Run(fmt.Sprintf("x%d", scale), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if other, err := OtherAddressIn(ctx, s.db, "99999000000555@lid"); err != nil || other != "" {
					b.Fatalf("OtherAddressIn = %q (%v), want none", other, err)
				}
			}
		})
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// TestOtherAddressUsesIndexes: every read of the other-address lookup, the
// check that a ban row's ban is in force included, searches an index: no
// line of its query plan scans seen, evidence, ledger or bans.
func TestOtherAddressUsesIndexes(t *testing.T) {
	// Two reads each of seen, evidence and ledger, and the ban list under
	// each ledger read.
	searchesOnly(t, map[string]int{"seen": 2, "evidence": 2, "ledger": 2, "bans": 2},
		otherAddressQuery(banInForce), "99999000000555@lid")
}

// TestBanEntryInUsesIndexes: each read of BanEntryIn, which every ban written
// runs, searches an index of the ban list.
func TestBanEntryInUsesIndexes(t *testing.T) {
	searchesOnly(t, map[string]int{"bans": 4}, banEntryQuery,
		"99999000000555@lid", "447700900123@s.whatsapp.net", BanEverywhere)
}

// searchesOnly checks query's plan: every line naming seen, evidence, ledger
// or bans searches an index, and want counts the reads of each.
func searchesOnly(t *testing.T, want map[string]int, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	s := openAt(t, &now)
	rows, err := s.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	table := regexp.MustCompile(`\b(seen|evidence|ledger|bans)\b`)
	var plan []string
	read := map[string]int{}
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
		if name := table.FindString(detail); name != "" {
			read[name]++
			if !strings.HasPrefix(detail, "SEARCH ") {
				t.Errorf("%q: a read that does not search an index", detail)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("plan:\n%s", strings.Join(plan, "\n"))
	if fmt.Sprint(read) != fmt.Sprint(want) {
		t.Errorf("reads per table %v, want %v", read, want)
	}
}

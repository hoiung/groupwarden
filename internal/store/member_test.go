package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// TestOtherAddress: a member's other address is found from each record that
// pairs their two (a decided post, a reported post, a ban row and the phone it
// keeps), by either address; none is found where nothing pairs them, and a
// ban row keyed by the phone alone pairs it with nothing.
func TestOtherAddress(t *testing.T) {
	ctx := context.Background()
	const (
		group = "99999000000111@g.us" // synthetic IDs, listed in .secret-pii-allowlist
		lid   = "99999000000444@lid"
		phone = "447700900123@s.whatsapp.net"
	)
	for name, record := range map[string]func(s *Store, tx *sql.Tx, at time.Time) error{
		"seen": func(s *Store, tx *sql.Tx, at time.Time) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO seen (dedupe_key, chat, msg_id, server_time, decided_at, sender, sender_alt)
VALUES ('msg|P1', ?, 'P1', ?, ?, ?, ?)`, group, at.UnixMilli(), at.UnixMilli(), lid, phone)
			return err
		},
		"evidence": func(s *Store, tx *sql.Tx, at time.Time) error {
			_, err := InsertEvidence(ctx, tx, Evidence{Chat: group, Community: group, Sender: lid, SenderAlt: phone,
				Subject: lid, MsgID: "P1", TargetID: "P1", MsgTime: at, Fields: "{}", Normalised: "{}"}, at)
			return err
		},
		"ban-row": func(s *Store, tx *sql.Tx, at time.Time) error {
			_, _, err := InsertLedger(ctx, tx, LedgerRow{Action: ActBan, Chat: BanEverywhere, Target: lid, Address: phone,
				TriggerID: "T", Community: group, Mode: ModeEnforce, ConfigHash: "h"}, at)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			s := openAt(t, &now)
			if err := s.Write(ctx, func(tx *sql.Tx) error { return record(s, tx, now) }); err != nil {
				t.Fatal(err)
			}
			for addr, want := range map[string]string{lid: phone, phone: lid, "99999000000555@lid": ""} {
				if got, err := s.OtherAddress(ctx, addr); err != nil || got != want {
					t.Errorf("OtherAddress(%s) = %q (%v), want %q", addr, got, err, want)
				}
			}
		})
	}
	t.Run("phone-keyed-ban-row", func(t *testing.T) {
		now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
		s := openAt(t, &now)
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			_, _, err := InsertLedger(ctx, tx, LedgerRow{Action: ActBan, Chat: BanEverywhere, Target: phone, Address: phone,
				TriggerID: "T", Community: group, Mode: ModeEnforce, ConfigHash: "h"}, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got, err := s.OtherAddress(ctx, phone); err != nil || got != "" {
			t.Errorf("OtherAddress(%s) = %q (%v), want none", phone, got, err)
		}
	})
}

// TestOtherAddressBanRows: a ban row pairs a member's two addresses only
// while the ban is in force (its ban list entry links the row, or a pause
// holds it); the row of a ban since lifted or undone pairs nothing, as the
// number may have passed to someone else. OtherAddressEver still reads it,
// for `member show|forget`.
func TestOtherAddressBanRows(t *testing.T) {
	ctx := context.Background()
	const (
		lid   = "99999000000444@lid" // synthetic IDs, listed in .secret-pii-allowlist
		phone = "447700900123@s.whatsapp.net"
	)
	for name, c := range map[string]struct {
		status       Status
		listed, undo bool // its ban list entry written; [Undo] overturned it
		lifted       bool // the entry removed again, as a lift and [Undo] remove it
		pairs        bool
	}{
		"in-force-pairs":       {status: Requested, listed: true, pairs: true},
		"held-pairs":           {status: Intended, pairs: true},
		"lifted-pairs-nothing": {status: Requested, listed: true, lifted: true},
		"undone-pairs-nothing": {status: Requested, listed: true, undo: true, lifted: true},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			s := openAt(t, &now)
			if err := s.Write(ctx, func(tx *sql.Tx) error {
				id, _, err := InsertLedger(ctx, tx, LedgerRow{Action: ActBan, Chat: BanEverywhere, Target: lid, Address: phone,
					TriggerID: "T", Community: "c", Mode: ModeEnforce, Status: c.status, ConfigHash: "h"}, now)
				if err != nil {
					return err
				}
				if c.listed {
					if err := AddBan(ctx, tx, Ban{Member: lid, Scope: BanEverywhere, LID: lid, Phone: phone, LedgerID: id}, now); err != nil {
						return err
					}
				}
				if c.undo {
					if _, err := Overturn(ctx, tx, []int64{id}, "undone", now); err != nil {
						return err
					}
				}
				if c.lifted {
					_, err = RemoveBans(ctx, tx, []string{lid, phone}, "")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for addr, other := range map[string]string{lid: phone, phone: lid} {
				want := ""
				if c.pairs {
					want = other
				}
				if got, err := s.OtherAddress(ctx, addr); err != nil || got != want {
					t.Errorf("OtherAddress(%s) = %q (%v), want %q", addr, got, err, want)
				}
				if got, err := s.OtherAddressEver(ctx, addr); err != nil || got != other {
					t.Errorf("OtherAddressEver(%s) = %q (%v), want %q", addr, got, err, other)
				}
			}
		})
	}
}

// TestOtherAddressBanListEntries (#8 Ralph restart 1): a ban list entry pairs
// its LID and number, as of when it was written, in the same time-ordered
// read as the posts and ban rows: of two people paired with one number, the
// one recorded last holds it now, whichever kind of record each is. An entry
// keyed by the number, with no LID, pairs nothing.
func TestOtherAddressBanListEntries(t *testing.T) {
	ctx := context.Background()
	const (
		earlier = "99999000000777@lid" // synthetic IDs, listed in .secret-pii-allowlist
		holder  = "99999000000888@lid"
		number  = "447700900321@s.whatsapp.net"
		moved   = "447700900123@s.whatsapp.net"
	)
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	entry := func(lid string, at time.Time) func(context.Context, *sql.Tx) error {
		return func(ctx context.Context, tx *sql.Tx) error {
			return AddBan(ctx, tx, Ban{Member: lid, Scope: BanEverywhere, LID: lid, Phone: number, Reason: "r"}, at)
		}
	}
	// A ban the bot wrote from a post that carried both addresses: its row
	// pairs them, and its entry links the row.
	banRow := func(lid string, at time.Time) func(context.Context, *sql.Tx) error {
		return func(ctx context.Context, tx *sql.Tx) error {
			id, _, err := InsertLedger(ctx, tx, LedgerRow{Action: ActBan, Chat: BanEverywhere, Target: lid, Address: number,
				TriggerID: "T" + lid, Community: "c", Mode: ModeEnforce, Status: Requested, ConfigHash: "h"}, at)
			if err != nil {
				return err
			}
			return AddBan(ctx, tx, Ban{Member: lid, Scope: BanEverywhere, LID: lid, Phone: number, Reason: "r", LedgerID: id}, at)
		}
	}
	post := func(lid string, at time.Time) func(context.Context, *sql.Tx) error {
		return func(ctx context.Context, tx *sql.Tx) error {
			_, err := InsertEvidence(ctx, tx, Evidence{Chat: "99999000000111@g.us", Community: "c", Sender: lid,
				SenderAlt: number, Subject: lid, MsgID: "P" + lid, TargetID: "P" + lid, MsgTime: at, Fields: "{}",
				Normalised: "{}"}, at)
			return err
		}
	}
	for name, c := range map[string]struct {
		writes []func(context.Context, *sql.Tx) error
		want   map[string]string // OtherAddress of each address
	}{
		"entry-pairs-both-ways": {writes: []func(context.Context, *sql.Tx) error{entry(holder, t0)},
			want: map[string]string{number: holder, holder: number}},
		"later-entry-beats-earlier-ban-row": {writes: []func(context.Context, *sql.Tx) error{banRow(earlier, t0),
			entry(holder, t1)}, want: map[string]string{number: holder}},
		"later-ban-row-beats-earlier-entry": {writes: []func(context.Context, *sql.Tx) error{entry(holder, t0),
			banRow(earlier, t1)}, want: map[string]string{number: earlier}},
		"later-post-beats-earlier-entry": {writes: []func(context.Context, *sql.Tx) error{entry(holder, t0),
			post(earlier, t1)}, want: map[string]string{number: earlier}},
		"later-entry-beats-earlier-post": {writes: []func(context.Context, *sql.Tx) error{post(earlier, t0),
			entry(holder, t1)}, want: map[string]string{number: holder}},
		"number-keyed-entry-pairs-nothing": {writes: []func(context.Context, *sql.Tx) error{
			func(ctx context.Context, tx *sql.Tx) error {
				return AddBan(ctx, tx, Ban{Member: number, Scope: BanEverywhere, Phone: number, Reason: "r"}, t0)
			}}, want: map[string]string{number: ""}},
		// An entry that knows one address, written later, hides no pair.
		"a-later-number-keyed-entry-hides-nothing": {writes: []func(context.Context, *sql.Tx) error{post(earlier, t0),
			func(ctx context.Context, tx *sql.Tx) error {
				return AddBan(ctx, tx, Ban{Member: number, Scope: BanEverywhere, Phone: number, Reason: "r"}, t1)
			}}, want: map[string]string{number: earlier}},
		"a-later-lid-only-entry-hides-nothing": {writes: []func(context.Context, *sql.Tx) error{post(holder, t0),
			func(ctx context.Context, tx *sql.Tx) error {
				return AddBan(ctx, tx, Ban{Member: holder, Scope: BanEverywhere, LID: holder, Reason: "r"}, t1)
			}}, want: map[string]string{holder: number}},
		// Someone who changed their number: the entry written later names the
		// number they have now.
		"a-later-entry-gives-a-lid-its-new-number": {writes: []func(context.Context, *sql.Tx) error{post(holder, t0),
			func(ctx context.Context, tx *sql.Tx) error {
				return AddBan(ctx, tx, Ban{Member: holder, Scope: BanEverywhere, LID: holder, Phone: moved, Reason: "r"}, t1)
			}}, want: map[string]string{holder: moved}},
	} {
		t.Run(name, func(t *testing.T) {
			now := t1
			s := openAt(t, &now)
			if err := s.Write(ctx, func(tx *sql.Tx) error {
				for _, w := range c.writes {
					if err := w(ctx, tx); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for addr, want := range c.want {
				if got, err := s.OtherAddress(ctx, addr); err != nil || got != want {
					t.Errorf("OtherAddress(%s) = %q (%v), want %q", addr, got, err, want)
				}
				if got, err := s.OtherAddressEver(ctx, addr); err != nil || got != want {
					t.Errorf("OtherAddressEver(%s) = %q (%v), want %q", addr, got, err, want)
				}
			}
		})
	}
}

// TestForgetMemberCountsHeldBanByPhone: `member forget <phone>` keeps, and
// counts, a ban a pause holds that names the member by LID and keeps their
// phone, as /unban <number> finds it.
func TestForgetMemberCountsHeldBanByPhone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := openAt(t, &now)
	const lid, phone = "99999000000444@lid", "447700900123@s.whatsapp.net"
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		_, _, err := InsertLedger(ctx, tx, LedgerRow{Action: ActBan, Chat: BanEverywhere, Target: lid, Address: phone,
			TriggerID: "T", Community: "c", Mode: ModeEnforce, ConfigHash: "h"}, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f, err := s.ForgetMember(ctx, []string{phone})
	if err != nil || f.HeldBans != 1 {
		t.Fatalf("forgotten %+v (%v), want the held ban counted", f, err)
	}
	if held, err := s.HeldBan(ctx, []string{lid}, ""); err != nil || !held {
		t.Fatalf("held %v (%v) after forget, want the held ban kept", held, err)
	}
}

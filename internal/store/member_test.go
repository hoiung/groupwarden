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

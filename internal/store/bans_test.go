package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"
)

// TestResolveBanMergeKeepsPhone: a phone-only ban (`ban add <phone>`) whose
// LID already has a ban of the same scope with no phone known merges into it,
// and the merged ban still matches the phone number alone; a ban of another
// scope is re-keyed as before.
func TestResolveBanMergeKeepsPhone(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t, Options{})
	const lid, phone, other = "99999000000444@lid", "447700900123@s.whatsapp.net", "99999000000111@g.us"
	now := time.Now()
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		for _, b := range []Ban{
			{Member: lid, Scope: BanEverywhere, LID: lid, Reason: "auto"},
			{Member: phone, Scope: BanEverywhere, Phone: phone, Reason: "manual"},
			{Member: phone, Scope: other, Phone: phone, Reason: "manual"},
		} {
			if err := AddBan(ctx, tx, b, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveBan(ctx, phone, lid); err != nil {
		t.Fatal(err)
	}
	all, err := s.Bans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("bans %+v, want the merged everywhere ban and the re-keyed community ban", all)
	}
	for _, b := range all {
		if b.Member != lid || b.LID != lid || b.Phone != phone {
			t.Errorf("ban %+v, want member and LID %s with phone %s", b, lid, phone)
		}
	}
	for _, community := range []string{"", other, "99999000000222@g.us"} {
		if _, ok, err := s.FindBan(ctx, []string{phone}, community); err != nil || !ok {
			t.Errorf("FindBan by the phone number alone in %q: found=%v err=%v", community, ok, err)
		}
	}
}

// TestResolveBanKeepsAdminLink (#8 AC 3.2): when a number entry merges into
// the LID entry of the same scope, the entry kept links to an admin's ban row
// (the admin chat's or the ban command's) from either side; when both are an
// admin's it keeps the LID entry's, and when neither is, the LID entry's. A
// link to a row purged since counts as the bot's.
func TestResolveBanKeepsAdminLink(t *testing.T) {
	const lid, phone = "99999000000444@lid", "447700900123@s.whatsapp.net"
	const bot, chat, cli, purged = "M1", AdminBanTrigger + "1", CLITrigger + "1", "purged"
	// merge links the LID entry to a row of trigger lidSide and the number
	// entry to one of numberSide ("purged": a row deleted since), merges them,
	// and returns the trigger of the row the one entry left links to.
	merge := func(t *testing.T, lidSide, numberSide string) string {
		t.Helper()
		ctx := context.Background()
		s, _ := openTemp(t, Options{})
		now := time.Now()
		if err := s.Write(ctx, func(tx *sql.Tx) error {
			for i, side := range []struct{ trigger, member, lid string }{{lidSide, lid, lid}, {numberSide, phone, ""}} {
				trigger := side.trigger
				if trigger == purged {
					trigger = "M" + strconv.Itoa(i+2)
				}
				id, _, err := InsertLedger(ctx, tx, LedgerRow{Action: ActBan, Chat: BanEverywhere, Target: side.member,
					Address: phone, TriggerID: trigger, Mode: ModeEnforce, Status: Requested}, now)
				if err != nil {
					return err
				}
				if side.trigger == purged {
					if _, err := tx.ExecContext(ctx, `DELETE FROM ledger WHERE id = ?`, id); err != nil {
						return err
					}
				}
				b := Ban{Member: side.member, Scope: BanEverywhere, LID: side.lid, LedgerID: id}
				if side.lid == "" {
					b.Phone = phone
				}
				if err := AddBan(ctx, tx, b, now); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.ResolveBan(ctx, phone, lid); err != nil {
			t.Fatal(err)
		}
		all, err := s.Bans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 1 || all[0].Member != lid || all[0].Phone != phone {
			t.Fatalf("bans %+v, want one entry keyed %s with the number", all, lid)
		}
		row, err := s.Ledger(ctx, all[0].LedgerID)
		if errors.Is(err, sql.ErrNoRows) {
			return purged
		}
		if err != nil {
			t.Fatal(err)
		}
		return row.TriggerID
	}
	t.Run("number-side-admin", func(t *testing.T) {
		for name, admin := range map[string]string{"admin-chat": chat, "command-line": cli} {
			t.Run(name, func(t *testing.T) {
				if got := merge(t, bot, admin); got != admin {
					t.Fatalf("the entry links to %s, want the number side's admin ban %s", got, admin)
				}
			})
		}
	})
	for name, c := range map[string]struct{ lidSide, numberSide, want string }{
		"lid-side-admin":                    {lidSide: chat, numberSide: bot, want: chat},
		"both-admin":                        {lidSide: chat, numberSide: cli, want: chat},
		"both-bot":                          {lidSide: bot, numberSide: "M9", want: bot},
		"number-side-admin-lid-side-purged": {lidSide: purged, numberSide: cli, want: cli},
	} {
		t.Run(name, func(t *testing.T) {
			if got := merge(t, c.lidSide, c.numberSide); got != c.want {
				t.Fatalf("the entry links to %s, want %s", got, c.want)
			}
		})
	}
}

// TestRemoveBansInCommunity: lifting someone's bans in one community removes
// the bans covering it (its own and an everywhere ban) and leaves their ban in
// another community and other people's bans there; "" lifts every ban.
func TestRemoveBansInCommunity(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t, Options{})
	const lid, phone, other = "99999000000444@lid", "447700900123@s.whatsapp.net", "99999000000666@lid"
	const a, b = "99999000000111@g.us", "99999000000222@g.us"
	now := time.Now()
	if err := s.Write(ctx, func(tx *sql.Tx) error {
		for _, ban := range []Ban{
			{Member: lid, Scope: BanEverywhere, LID: lid},
			{Member: lid, Scope: a, LID: lid},
			{Member: phone, Scope: b, Phone: phone},
			{Member: other, Scope: a, LID: other},
		} {
			if err := AddBan(ctx, tx, ban, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	remove := func(community string) int64 {
		t.Helper()
		var n int64
		if err := s.Write(ctx, func(tx *sql.Tx) (err error) {
			n, err = RemoveBans(ctx, tx, []string{lid, phone}, community)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	left := func() map[string]string {
		t.Helper()
		all, err := s.Bans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, ban := range all {
			out[ban.Member+" "+ban.Scope] = ban.Reason
		}
		return out
	}
	if n := remove(a); n != 2 {
		t.Fatalf("lifted %d bans in %s, want its own and the everywhere ban", n, a)
	}
	if got := left(); len(got) != 2 || !hasKey(got, phone+" "+b) || !hasKey(got, other+" "+a) {
		t.Fatalf("bans left %v, want the ban in %s and the other member's", got, b)
	}
	if n := remove(""); n != 1 {
		t.Fatalf("lifted %d bans everywhere, want the one left", n)
	}
	if got := left(); len(got) != 1 || !hasKey(got, other+" "+a) {
		t.Fatalf("bans left %v, want only the other member's", got)
	}
}

func hasKey(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}

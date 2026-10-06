package store

import (
	"context"
	"database/sql"
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

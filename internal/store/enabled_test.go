package store

import (
	"context"
	"testing"
	"time"
)

// TestEnabledList: entries are kept sorted by ID, a second /enable of the
// same ID replaces the first, and Disable says whether there was one.
func TestEnabledList(t *testing.T) {
	ctx := context.Background()
	s, _ := openTemp(t, Options{})
	at := time.Date(2026, 10, 18, 13, 0, 0, 0, time.UTC)
	for _, e := range []Enabled{
		{ID: "99999000000777@g.us", Kind: KindCommunity, Name: "book club", By: "telegram:1 (A)", At: at},
		{ID: "99999000000333@g.us", Kind: KindGroup, Name: "public chat", By: "telegram:1 (A)", At: at},
		{ID: "99999000000777@g.us", Kind: KindCommunity, Name: "book club 2", By: "telegram:2 (B)", At: at.Add(time.Hour)},
	} {
		if err := s.Enable(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.EnabledList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "99999000000333@g.us" || list[0].Kind != KindGroup ||
		list[1].Name != "book club 2" || list[1].By != "telegram:2 (B)" || !list[1].At.Equal(at.Add(time.Hour)) {
		t.Fatalf("list %+v", list)
	}
	if err := s.Enable(ctx, Enabled{ID: "99999000000111@g.us", Kind: "everything", By: "x", At: at}); err == nil {
		t.Fatal("an unknown kind was stored")
	}
	for _, tc := range []struct {
		id   string
		want bool
	}{{"99999000000777@g.us", true}, {"99999000000777@g.us", false}} {
		removed, err := s.Disable(ctx, tc.id)
		if err != nil || removed != tc.want {
			t.Fatalf("Disable(%s) = %v %v, want %v", tc.id, removed, err, tc.want)
		}
	}
	if list, err := s.EnabledList(ctx); err != nil || len(list) != 1 {
		t.Fatalf("after Disable: %+v %v", list, err)
	}
}

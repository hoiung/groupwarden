package app

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/config/configtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
)

// TestEnableSweepsAfterTheJoin: /enable asks for the sweep only once its own
// join through the link is done. A sweep that listed a community's groups
// before the bot was in the community would try to join them by itself too
// early, and never try again. /disable asks for one after the removal.
func TestEnableSweepsAfterTheJoin(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "g.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	holder, _ := configtest.Holder(t, testSet)
	community := client.JID("99999000000777@g.us") // listed in .secret-pii-allowlist
	fake := &clienttest.Fake{Invites: map[string]client.Group{"CoMm1": {JID: community, Name: "book club", IsCommunity: true}}}
	var logs lockedBuffer
	a := &App{Adapter: fake, Store: st, Config: holder, Directory: &pipeline.Directory{}, Clock: SystemClock{},
		Log: slog.New(slog.NewTextHandler(&logs, nil)), sweepNow: make(chan struct{}, 1)}
	pendingAtJoin := -1
	fake.OnCall = func(call string) {
		if strings.HasPrefix(call, "JoinWithLink") {
			pendingAtJoin = len(a.sweepNow)
		}
	}
	by := telegram.Actor{ID: 501, Name: "Ann"}
	reply, err := controls{a}.Enable(ctx, "https://chat.whatsapp.com/CoMm1", by)
	if err != nil || !strings.Contains(reply, "the bot joined it") {
		t.Fatalf("enable: %q %v", reply, err)
	}
	if pendingAtJoin != 0 || len(a.sweepNow) != 1 {
		t.Fatalf("sweeps pending at the join %d, after /enable %d; want 0 then 1", pendingAtJoin, len(a.sweepNow))
	}
	// Who switched it on is kept with their Telegram ID, not the name alone.
	if list, err := st.EnabledList(ctx); err != nil || len(list) != 1 || list[0].By != by.String() {
		t.Fatalf("entry %+v %v, want By %q", list, err, by.String())
	}
	// The running config took the list: the log says so, with no error line.
	if out := logs.String(); !strings.Contains(out, `msg="enabled list loaded"`) || strings.Contains(out, "level=ERROR") {
		t.Fatalf("the log after /enable:\n%s", out)
	}
	<-a.sweepNow
	reply, err = controls{a}.Disable(ctx, "book club", by)
	if err != nil || !strings.Contains(reply, `no longer moderates the community "book club"`) || len(a.sweepNow) != 1 {
		t.Fatalf("disable: %q %v, %d sweeps pending", reply, err, len(a.sweepNow))
	}
	<-a.sweepNow
	// A /disable that removed nothing changes nothing to sweep for.
	if reply, err := (controls{a}).Disable(ctx, "https://chat.whatsapp.com/CoMm1", by); err != nil ||
		!strings.Contains(reply, "is not on") || len(a.sweepNow) != 0 {
		t.Fatalf("a /disable of what is off: %q %v, %d sweeps pending", reply, err, len(a.sweepNow))
	}
	// A join WhatsApp refuses, and one an admin must approve: the entry is
	// kept either way, and the sweep lists the group as absent meanwhile.
	group := client.JID("99999000000333@g.us") // listed in .secret-pii-allowlist
	fake.Invites["GrOuP1"] = client.Group{JID: group, Name: "public chat"}
	fake.FailNext("JoinWithLink", errors.New("join refused"))
	if reply, err := (controls{a}).Enable(ctx, "https://chat.whatsapp.com/GrOuP1", by); err != nil ||
		!strings.Contains(reply, "joining it failed") || len(a.sweepNow) != 1 {
		t.Fatalf("a refused join: %q %v, %d sweeps pending", reply, err, len(a.sweepNow))
	}
	<-a.sweepNow
	if _, err := st.Disable(ctx, string(group)); err != nil {
		t.Fatal(err)
	}
	fake.JoinPending = map[client.JID]bool{group: true}
	if reply, err := (controls{a}).Enable(ctx, "https://chat.whatsapp.com/GrOuP1", by); err != nil ||
		!strings.Contains(reply, "join request sent") || len(a.sweepNow) != 1 {
		t.Fatalf("a join to approve: %q %v, %d sweeps pending", reply, err, len(a.sweepNow))
	}
}

// TestAndList: the own settings a reply names read as English.
func TestAndList(t *testing.T) {
	for want, words := range map[string][]string{
		"":                               nil,
		"mode":                           {"mode"},
		"mode and retention":             {"mode", "retention"},
		"mode, word_lists and retention": {"mode", "word_lists", "retention"},
	} {
		if got := andList(words); got != want {
			t.Errorf("andList(%q) = %q, want %q", words, got, want)
		}
	}
}

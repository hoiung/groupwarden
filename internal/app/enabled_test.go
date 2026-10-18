package app

import (
	"context"
	"errors"
	"log/slog"
	"os"
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

// bookClub is the community the invite link CoMm1 points to (listed in
// .secret-pii-allowlist).
const bookClub client.JID = "99999000000777@g.us"

// enableApp is an App for /enable and /disable: the config is the test set
// plus extra, the fake's invite CoMm1 is the community "book club", and the
// log is returned to read.
func enableApp(t *testing.T, extra string) (*App, *clienttest.Fake, *lockedBuffer) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "g.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	holder, _ := configtest.Holder(t, testSet+extra)
	fake := &clienttest.Fake{Invites: map[string]client.Group{"CoMm1": {JID: bookClub, Name: "book club", IsCommunity: true}}}
	logs := &lockedBuffer{}
	return &App{Adapter: fake, Store: st, Config: holder, Directory: &pipeline.Directory{}, Clock: SystemClock{},
		Log: slog.New(slog.NewTextHandler(logs, nil)), sweepNow: make(chan struct{}, 1)}, fake, logs
}

// TestEnableSweepsAfterTheJoin: /enable asks for the sweep only once its own
// join through the link is done. A sweep that listed a community's groups
// before the bot was in the community would try to join them by itself too
// early, and never try again. /disable asks for one after the removal.
func TestEnableSweepsAfterTheJoin(t *testing.T) {
	ctx := context.Background()
	a, fake, logs := enableApp(t, "")
	st := a.Store
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

// TestDisableWithTheCorpusAhead: a config sync swaps in a corpus with a new
// spam sample before the bot reloads the rule that catches it; a /disable in
// that gap still switches the community off.
func TestDisableWithTheCorpusAhead(t *testing.T) {
	ctx := context.Background()
	corpus := filepath.Join(t.TempDir(), "corpus")
	sample := func(rel, text string) {
		p := filepath.Join(corpus, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("text: \""+text+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sample("spam/1.yaml", "bitcoin signals t.me/example_signals")
	sample("legit/general/1.yaml", "see you on Sunday")
	a, _, _ := enableApp(t, "corpus_dir: "+corpus+"\nword_lists:\n  crypto: [\"bitcoin\"]\nrules:\n  list:\n"+
		"    - name: pitch\n      action: delete_remove_ban\n      confirmed: true\n      when:\n        all:\n"+
		"          - words: [crypto]\n          - has: [any_link, handle]\n")
	by := telegram.Actor{ID: 501, Name: "Ann"}
	if _, err := (controls{a}).Enable(ctx, "https://chat.whatsapp.com/CoMm1", by); err != nil {
		t.Fatal(err)
	}
	sample("spam/2.yaml", "bitcoin to the moon, huge gains coming")
	reply, err := controls{a}.Disable(ctx, "book club", by)
	if _, on := a.Config.Current().Rules.ModeFor(string(bookClub)); err != nil || on ||
		!strings.Contains(reply, `Disabled by Ann: the bot no longer moderates the community "book club"`) {
		t.Fatalf("a /disable with the corpus ahead: %q %v, still on %v", reply, err, on)
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

package pipeline_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
)

// TestTooOldSpamReportedOnly: spam older than act_on_replay_max_age (a
// replay after the bot was offline) matches as usual but is only reported,
// without [Ban]: nothing is deleted, removed or banned, and every row it
// writes is shadow.
func TestTooOldSpamReportedOnly(t *testing.T) {
	k := modtest.New(t, "")
	m := k.Spam("OLD1", modtest.G1)
	m.Time = k.Clock.Now().Add(-48 * time.Hour)
	k.Deliver(m)
	k.Fire()
	if n := k.Fake.Count("Revoke") + k.Fake.Count("Remove"); n != 0 {
		t.Fatalf("a too-old match acted: %v", k.Fake.Calls())
	}
	if k.Banned(modtest.SpammerM, "") {
		t.Fatal("a too-old match banned the sender")
	}
	reps := k.Reports(ledger.KindWouldHaveActed)
	if len(reps) != 1 || len(reps[0].Buttons) != 0 ||
		!strings.Contains(reps[0].Text, "matched rule pitch, but the message is older than act_on_replay_max_age: reported only.") {
		t.Fatalf("would-have-acted reports %+v", reps)
	}
	if n := len(k.Reports(ledger.KindAction)); n != 0 {
		t.Fatalf("%d action reports for a too-old match", n)
	}
	for _, row := range k.Rows(modtest.SpammerM) {
		if row.Mode == store.ModeEnforce {
			t.Fatalf("an enforce row for a too-old match: %+v", row)
		}
	}

	// The same post inside the window is acted on.
	k.Deliver(k.Spam("NEW1", modtest.G1))
	k.Fire()
	if k.Fake.Count("Revoke "+string(modtest.G1)+" "+string(modtest.Spammer)+" NEW1") != 1 || !k.Banned(modtest.SpammerM, "") {
		t.Fatalf("calls %v", k.Fake.Calls())
	}
}

// TestKeywordOnlyPostCounted: a post with a keyword that no rule acts on is
// left alone and counted for the daily summary, per community and word
// list; a post in a group that is not moderated is not counted.
func TestKeywordOnlyPostCounted(t *testing.T) {
	k := modtest.New(t, "")
	k.Deliver(k.Msg("KW1", modtest.G1, modtest.Member, "anyone know a good bitcoin wallet?"))
	k.Deliver(k.Msg("KW2", modtest.G2, modtest.Member, "crypto talk on Sunday"))
	k.Deliver(k.Msg("KW3", modtest.GB, modtest.Member, "bitcoin is up"))
	k.Deliver(k.Msg("KW4", "99999000000333@g.us", modtest.Member, "bitcoin bitcoin"))
	k.Fire()
	if n := len(k.Fake.Calls()); n != 0 {
		t.Fatalf("a keyword-only post acted: %v", k.Fake.Calls())
	}
	counters, err := k.Store.Counters(k.Ctx, k.Clock.Now().UTC().Format("2006-01-02"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		pipeline.CounterKeywordOnly + string(modtest.Community) + "|crypto": 2,
		pipeline.CounterKeywordOnly + modtest.SetB + "|crypto":              1,
	}
	got := map[string]int{}
	for name, n := range counters {
		if strings.HasPrefix(name, pipeline.CounterKeywordOnly) {
			got[name] = n
		}
	}
	if len(got) != len(want) {
		t.Fatalf("keyword-only counters %v, want %v", got, want)
	}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s = %d, want %d", name, got[name], n)
		}
	}
}

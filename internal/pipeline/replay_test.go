package pipeline_test

import (
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
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

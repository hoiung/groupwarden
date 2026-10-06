package pipeline_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/modtest"
)

// TestDecryptErrorLogged: a message the bot could not decrypt is logged as it
// is decided, with its group (masked) and the reason, beside the priority
// report for the admin chat; a missing parent secret is only counted.
func TestDecryptErrorLogged(t *testing.T) {
	k := modtest.New(t, "")
	var logs bytes.Buffer
	k.Mod.Log = slog.New(slog.NewTextHandler(&logs, nil))
	k.Deliver(&client.Undecryptable{Chat: modtest.G1, Sender: modtest.Member, ID: "U1", Time: k.Clock.Now(),
		Reason: client.ReasonMissingParentSecret, Detail: "no secret"})
	if strings.Contains(logs.String(), "could not decrypt") {
		t.Fatalf("a missing parent secret is logged as a decrypt error:\n%s", logs.String())
	}
	k.Deliver(&client.Undecryptable{Chat: modtest.G1, Sender: modtest.Member, ID: "U2", Time: k.Clock.Now(),
		Reason: client.ReasonDecryptError, Detail: "gcm: message authentication failed"})
	want := `msg="could not decrypt a message" chat=group…0111 reason=decrypt_error detail="gcm: message authentication failed"`
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log %q, want a line with %q", logs.String(), want)
	}
	if reps := k.Reports(string(alert.DecryptError)); len(reps) != 1 || !reps[0].Priority {
		t.Fatalf("decrypt_error reports %+v, want one priority report", reps)
	}
}

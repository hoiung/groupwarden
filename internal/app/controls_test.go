package app

import (
	"testing"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
)

// TestBanReplySaysWhatWasDone: the reply to [Ban] says what was done and
// what waits for a pause (the ban itself applies at once), and keeps the
// report number readable through the masking every reply gets: "#12345",
// never "…2345".
func TestBanReplySaysWhatWasDone(t *testing.T) {
	member := client.Member{LID: modtest.Other1}
	m := mask.IDs(string(modtest.Other1))
	cases := []struct {
		name string
		res  pipeline.Banned
		want string
	}{
		{"done", pipeline.Banned{Member: member, Deleting: true, Removals: 3},
			"the message is being deleted; " + m + " is removed from 3 group(s) and banned."},
		{"removals paused", pipeline.Banned{Member: member, Deleting: true, Removals: 3, Paused: store.ScopeRemoveBan},
			"the message is being deleted; " + m + " is banned; their removal from 3 group(s) waits for the pause to end."},
		{"every action paused", pipeline.Banned{Member: member, Deleting: true, Removals: 3, Paused: store.ScopeAll},
			"the message is deleted once the pause ends; " + m + " is banned; their removal from 3 group(s) waits for the pause to end."},
		{"too old", pipeline.Banned{Member: member, TooOld: true, Removals: 1},
			"the message is older than act_on_replay_max_age, so it stays; " + m + " is removed from 1 group(s) and banned."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mask.IDs(banReply(c.res, 12345, telegram.Actor{ID: 501, Name: "Ann"}, true))
			want := "Banned by Ann (#12345): " + c.want + " To undo: /unban #12345"
			if got != want {
				t.Fatalf("reply\n%q\nwant\n%q", got, want)
			}
		})
	}
}

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
// never "…2345". In shadow mode nothing is banned, so the reply neither says
// "Banned" nor offers an /unban.
func TestBanReplySaysWhatWasDone(t *testing.T) {
	member := client.Member{LID: modtest.Other1}
	m := mask.IDs(string(modtest.Other1))
	banned := func(s string) string { return "Banned by Ann (#12345): " + s + " To undo: /unban #12345" }
	shadow := "Recorded by Ann (#12345): " + m + " would be removed from 2 group(s) and banned, but the community is in " +
		"shadow mode: nothing is banned or sent."
	cases := []struct {
		name string
		res  pipeline.Banned
		want string
	}{
		{"done", pipeline.Banned{Member: member, Deleting: true, Removals: 3},
			banned("the message is being deleted; " + m + " is removed from 3 group(s) and banned.")},
		{"removals paused", pipeline.Banned{Member: member, Deleting: true, Removals: 3, Paused: store.ScopeRemoveBan},
			banned("the message is being deleted; " + m + " is banned; their removal from 3 group(s) waits for the pause to end.")},
		{"every action paused", pipeline.Banned{Member: member, Deleting: true, Removals: 3, Paused: store.ScopeAll},
			banned("the message is deleted once the pause ends; " + m + " is banned; their removal from 3 group(s) waits for the pause to end.")},
		{"too old", pipeline.Banned{Member: member, TooOld: true, Removals: 1},
			banned("the message is older than act_on_replay_max_age, so it stays; " + m + " is removed from 1 group(s) and banned.")},
		{"shadow mode", pipeline.Banned{Member: member, Removals: 2, Shadow: true}, shadow},
		{"shadow mode while paused", pipeline.Banned{Member: member, Removals: 2, Shadow: true, Paused: store.ScopeRemoveBan}, shadow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mask.IDs(banReply(c.res, 12345, telegram.Actor{ID: 501, Name: "Ann"}, true))
			want := c.want
			if got != want {
				t.Fatalf("reply\n%q\nwant\n%q", got, want)
			}
		})
	}
}

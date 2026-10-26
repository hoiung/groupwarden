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
// what waits for a pause (the ban itself applies at once), how many of the
// member's other posts on record are being deleted and how many could not
// be, and keeps the report number readable through the masking every reply
// gets: "#12345", never "…2345". In shadow mode nothing is banned, so the
// reply neither says "Banned" nor offers an /unban.
func TestBanReplySaysWhatWasDone(t *testing.T) {
	member := client.Member{LID: modtest.Other1}
	m := mask.IDs(string(modtest.Other1))
	banned := func(s string) string { return "Banned by Ann (#12345): " + s + " To undo: /unban #12345" }
	none := "their other posts on record: 0 being deleted, 0 too old to delete"
	shadow := "Recorded by Ann (#12345): " + m + " would be removed from 2 group(s) and banned, but the community is in " +
		"shadow mode: nothing is banned or sent."
	cases := []struct {
		name string
		res  pipeline.Banned
		want string
	}{
		{"done", pipeline.Banned{Member: member, Deleting: true, Removals: 3},
			banned("the message is being deleted; " + none + "; " + m + " is removed from 3 group(s) and banned.")},
		{"other posts", pipeline.Banned{Member: member, Deleting: true, Removals: 3,
			Posts: pipeline.Posts{Deleting: 2, Refused: 1, Shadow: 1, Spared: 1}},
			banned("the message is being deleted; their other posts on record: 2 being deleted, 1 too old to delete " +
				"(older than act_on_replay_max_age), 1 left in a community in shadow mode, 1 left where they are a " +
				"current admin; " + m + " is removed from 3 group(s) and banned.")},
		{"removals paused", pipeline.Banned{Member: member, Deleting: true, Removals: 3, Paused: store.ScopeRemoveBan},
			banned("the message is being deleted; " + none + "; " + m + " is banned; their removal from 3 group(s) waits for the pause to end.")},
		{"every action paused", pipeline.Banned{Member: member, Deleting: true, Removals: 3, Paused: store.ScopeAll},
			banned("the message is deleted once the pause ends; their other posts on record: 0 deleted once the pause " +
				"ends, 0 too old to delete; " + m + " is banned; their removal from 3 group(s) waits for the pause to end.")},
		{"too old", pipeline.Banned{Member: member, TooOld: true, Removals: 1},
			banned("the message is older than act_on_replay_max_age, so it stays; " + none + "; " + m + " is removed from 1 group(s) and banned.")},
		{"shadow mode", pipeline.Banned{Member: member, Removals: 2, Shadow: true}, shadow},
		{"shadow mode while paused", pipeline.Banned{Member: member, Removals: 2, Shadow: true, Paused: store.ScopeRemoveBan}, shadow},
		{"shadow mode with other posts", pipeline.Banned{Member: member, Removals: 2, Shadow: true, Posts: pipeline.Posts{Shadow: 3}},
			"Recorded by Ann (#12345): " + m + " would be removed from 2 group(s) and banned, and 3 more post(s) of theirs " +
				"deleted, but the community is in shadow mode: nothing is banned or sent."},
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

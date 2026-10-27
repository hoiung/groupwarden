package pipeline_test

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
)

// TestLIDForNumber: the first source that knows a number's LID gives it, in
// the order the group list's pair, the saved WhatsApp IDs, WhatsApp, then the
// bot's records; WhatsApp's error is returned beside what the records give,
// and an answer from WhatsApp that is not a LID counts as no LID.
func TestLIDForNumber(t *testing.T) {
	k := modtest.New(t, "")
	phone := modtest.SpammerPhone // the group list pairs it with modtest.Spammer
	at := k.Clock.Now()
	if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
		_, err := store.InsertEvidence(k.Ctx, tx, store.Evidence{Chat: string(modtest.G1), Community: string(modtest.Community),
			Sender: string(modtest.Member), SenderAlt: string(phone), Subject: string(modtest.Member), MsgID: "R1", TargetID: "R1",
			MsgTime: at, Fields: "{}", Normalised: "{}"}, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	give := func(lid client.JID, err error) func(context.Context, client.JID) (client.JID, error) {
		return func(context.Context, client.JID) (client.JID, error) { return lid, err }
	}
	unreadable := errors.New("read the saved LID: disk I/O error")
	for name, c := range map[string]struct {
		src         pipeline.LIDSources
		want        client.JID
		askErr, err error
	}{
		// An answer from the saved IDs that is not a LID counts as none, as
		// WhatsApp's does; a failed read of them ends the lookup.
		"non-lid-saved-answer-is-no-lid": {src: pipeline.LIDSources{Saved: give(phone, nil)}},
		"saved-ids-unreadable": {src: pipeline.LIDSources{Saved: give("", unreadable), Ask: give(modtest.Other2, nil),
			Records: k.Store}, err: unreadable},
		"group-list-first": {src: pipeline.LIDSources{Pairs: k.Dir, Saved: give(modtest.Other1, nil),
			Ask: give(modtest.Other2, nil), Records: k.Store}, want: modtest.Spammer},
		"saved-ids-next": {src: pipeline.LIDSources{Saved: give(modtest.Other1, nil), Ask: give(modtest.Other2, nil),
			Records: k.Store}, want: modtest.Other1},
		"whatsapp-next": {src: pipeline.LIDSources{Saved: give("", nil), Ask: give(modtest.Other2, nil), Records: k.Store},
			want: modtest.Other2},
		"records-after-whatsapp-down": {src: pipeline.LIDSources{Saved: give("", nil), Ask: give("", client.ErrRateLimited),
			Records: k.Store}, want: modtest.Member, askErr: client.ErrRateLimited},
		"non-lid-answer-is-no-lid": {src: pipeline.LIDSources{Ask: give(phone, nil)}, askErr: client.ErrNoLID},
	} {
		t.Run(name, func(t *testing.T) {
			lid, askErr, err := pipeline.LIDForNumber(k.Ctx, phone, c.src)
			if !errors.Is(err, c.err) || lid != c.want || (c.askErr == nil) != (askErr == nil) ||
				(c.askErr != nil && !errors.Is(askErr, c.askErr)) {
				t.Fatalf("LIDForNumber = %q, %v, %v; want %q, %v, %v", lid, askErr, err, c.want, c.askErr, c.err)
			}
		})
	}
}

// TestIdentifyKeepsANumberItsHolderHoldsNow (#8 Gate 1): WhatsApp gives a
// number to someone new once its holder leaves it. A number Identify finds
// for a LID in the bot's records or on the ban list is kept only when the
// number's own pair, the group list's or else the one recorded last (a post,
// a ban row, a ban list entry: one time-ordered read), is that LID or none; a
// number given alone takes the LID recorded last the same way, and so does
// the records' step of LIDForNumber (RecordedPair). Read with and without a
// transaction.
func TestIdentifyKeepsANumberItsHolderHoldsNow(t *testing.T) {
	const free = client.JID("447700900321@s.whatsapp.net") // nobody in the group list holds it
	type post struct{ lid, phone client.JID }
	ban := func(lid, phone client.JID) store.Ban {
		return store.Ban{Member: string(lid), Scope: store.BanEverywhere, LID: string(lid), Phone: string(phone)}
	}
	for name, c := range map[string]struct {
		posts  []post        // in the order they were recorded, a minute apart from postAt
		postAt time.Duration // after the clock's start
		bans   []store.Ban   // ban list entries with no ledger row, a minute apart from the start
		want   map[client.JID]client.Member
		// recordsSay is LIDForNumber(free) through the records alone, when set
		recordsSay client.JID
	}{
		"recorded-number-nobody-else-holds": {posts: []post{{modtest.Other1, free}},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1, Phone: free}}},
		"group-list-pairs-it-with-another": {posts: []post{{modtest.Other1, modtest.SpammerPhone}},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1}}},
		"recorded-later-with-another": {posts: []post{{modtest.Other1, free}, {modtest.Other2, free}},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1},
				modtest.Other2: {LID: modtest.Other2, Phone: free}}},
		"ban-list-number-the-group-list-pairs-with-another": {bans: []store.Ban{ban(modtest.Other1, modtest.SpammerPhone)},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1}}},
		"ban-list-number-nobody-else-holds": {bans: []store.Ban{ban(modtest.Other1, free)},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1, Phone: free},
				free: {LID: modtest.Other1, Phone: free}}, recordsSay: modtest.Other1},
		// Two people's bans hold the number and nothing else pairs it: the
		// later ban names who holds it now, whichever order the LIDs sort in.
		"ban-list-two-holders-the-later-holds-it": {bans: []store.Ban{ban(modtest.Other1, free), ban(modtest.Other2, free)},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1},
				modtest.Other2: {LID: modtest.Other2, Phone: free}, free: {LID: modtest.Other2, Phone: free}},
			recordsSay: modtest.Other2},
		"ban-list-two-holders-written-the-other-way": {bans: []store.Ban{ban(modtest.Other2, free), ban(modtest.Other1, free)},
			want: map[client.JID]client.Member{modtest.Other2: {LID: modtest.Other2},
				modtest.Other1: {LID: modtest.Other1, Phone: free}, free: {LID: modtest.Other1, Phone: free}},
			recordsSay: modtest.Other1},
		// A post and a ban are records alike: the later one decides.
		"a-later-ban-beats-an-older-post": {posts: []post{{modtest.Other1, free}},
			bans: []store.Ban{ban(modtest.Other1, free), ban(modtest.Other2, free)},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1},
				modtest.Other2: {LID: modtest.Other2, Phone: free}}, recordsSay: modtest.Other2},
		"a-later-post-beats-an-older-ban": {posts: []post{{modtest.Other1, free}}, postAt: 10 * time.Minute,
			bans: []store.Ban{ban(modtest.Other1, free), ban(modtest.Other2, free)},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1, Phone: free},
				modtest.Other2: {LID: modtest.Other2}}, recordsSay: modtest.Other1},
	} {
		t.Run(name, func(t *testing.T) {
			k := modtest.New(t, "")
			if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
				for i, p := range c.posts {
					at := k.Clock.Now().Add(c.postAt + time.Duration(i)*time.Minute)
					id := "P" + strconv.Itoa(i)
					if _, err := store.InsertEvidence(k.Ctx, tx, store.Evidence{Chat: string(modtest.G1),
						Community: string(modtest.Community), Sender: string(p.lid), SenderAlt: string(p.phone),
						Subject: string(p.lid), MsgID: id, TargetID: id, MsgTime: at, Fields: "{}", Normalised: "{}"},
						at); err != nil {
						return err
					}
				}
				for i, b := range c.bans {
					b.Reason = "test"
					if err := store.AddBan(k.Ctx, tx, b, k.Clock.Now().Add(time.Duration(i)*time.Minute)); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for addr, want := range c.want {
				got, err := pipeline.Identify(k.Ctx, k.Dir, k.Store, nil, client.MemberOf(addr))
				if err != nil || got != want {
					t.Fatalf("Identify(%s) = %+v, %v; want %+v", addr, got, err, want)
				}
				if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
					got, err = pipeline.Identify(k.Ctx, k.Dir, k.Store, tx, client.MemberOf(addr))
					return err
				}); err != nil || got != want {
					t.Fatalf("Identify(%s) in a transaction = %+v, %v; want %+v", addr, got, err, want)
				}
			}
			if c.recordsSay != "" {
				if lid, _, err := pipeline.LIDForNumber(k.Ctx, free, pipeline.LIDSources{Records: k.Store}); err != nil ||
					lid != c.recordsSay {
					t.Fatalf("LIDForNumber(%s) through the records = %q, %v; want %q", free, lid, err, c.recordsSay)
				}
			}
		})
	}
}

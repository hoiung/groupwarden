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
// number's own pair, the group list's or else the one recorded last, is that
// LID or none; read with and without a transaction.
func TestIdentifyKeepsANumberItsHolderHoldsNow(t *testing.T) {
	const free = client.JID("447700900321@s.whatsapp.net") // nobody in the group list holds it
	type post struct{ lid, phone client.JID }
	for name, c := range map[string]struct {
		posts []post    // in the order they were recorded
		ban   store.Ban // a ban list entry with no ledger row, when it has a member
		want  map[client.JID]client.Member
	}{
		"recorded-number-nobody-else-holds": {posts: []post{{modtest.Other1, free}},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1, Phone: free}}},
		"group-list-pairs-it-with-another": {posts: []post{{modtest.Other1, modtest.SpammerPhone}},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1}}},
		"recorded-later-with-another": {posts: []post{{modtest.Other1, free}, {modtest.Other2, free}},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1},
				modtest.Other2: {LID: modtest.Other2, Phone: free}}},
		"ban-list-number-the-group-list-pairs-with-another": {ban: store.Ban{Member: string(modtest.Other1),
			Scope: store.BanEverywhere, LID: string(modtest.Other1), Phone: string(modtest.SpammerPhone)},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1}}},
		"ban-list-number-nobody-else-holds": {ban: store.Ban{Member: string(modtest.Other1),
			Scope: store.BanEverywhere, LID: string(modtest.Other1), Phone: string(free)},
			want: map[client.JID]client.Member{modtest.Other1: {LID: modtest.Other1, Phone: free}}},
	} {
		t.Run(name, func(t *testing.T) {
			k := modtest.New(t, "")
			if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
				for i, p := range c.posts {
					at := k.Clock.Now().Add(time.Duration(i) * time.Minute)
					id := "P" + strconv.Itoa(i)
					if _, err := store.InsertEvidence(k.Ctx, tx, store.Evidence{Chat: string(modtest.G1),
						Community: string(modtest.Community), Sender: string(p.lid), SenderAlt: string(p.phone),
						Subject: string(p.lid), MsgID: id, TargetID: id, MsgTime: at, Fields: "{}", Normalised: "{}"},
						at); err != nil {
						return err
					}
				}
				if c.ban.Member == "" {
					return nil
				}
				return store.AddBan(k.Ctx, tx, c.ban, k.Clock.Now())
			}); err != nil {
				t.Fatal(err)
			}
			for lid, want := range c.want {
				got, err := pipeline.Identify(k.Ctx, k.Dir, k.Store, nil, client.Member{LID: lid})
				if err != nil || got != want {
					t.Fatalf("Identify(%s) = %+v, %v; want %+v", lid, got, err, want)
				}
				if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
					got, err = pipeline.Identify(k.Ctx, k.Dir, k.Store, tx, client.Member{LID: lid})
					return err
				}); err != nil || got != want {
					t.Fatalf("Identify(%s) in a transaction = %+v, %v; want %+v", lid, got, err, want)
				}
			}
		})
	}
}

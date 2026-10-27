package pipeline_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

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

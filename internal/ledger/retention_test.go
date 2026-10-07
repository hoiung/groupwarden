package ledger_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
)

func revokeRow(t *testing.T, k *modtest.Kit) store.LedgerRow {
	t.Helper()
	rows := k.Find(modtest.SpammerM, store.ActRevoke, modtest.G1)
	if len(rows) != 1 {
		t.Fatalf("revoke rows %+v", rows)
	}
	return rows[0]
}

// TestEvidenceBeforeRevoke: the evidence copy (original and normalised text,
// rule, config hash, sender, group, times) is on disk before the delete is sent.
func TestEvidenceBeforeRevoke(t *testing.T) {
	k := modtest.New(t, "")
	var at store.Evidence
	k.Fake.OnCall = func(call string) {
		if !strings.HasPrefix(call, "Revoke") {
			return
		}
		e, ok, err := k.Store.Evidence(k.Ctx, revokeRow(t, k).EvidenceID)
		if err != nil || !ok {
			t.Errorf("no evidence copy when the delete was sent (%v)", err)
		}
		at = e
	}
	m := k.Spam("M1", modtest.G1)
	m.Fields = append(m.Fields, client.Field{Name: "caption", Text: "Ｂｉｔｃｏｉｎ", Match: "Ｂｉｔｃｏｉｎ"})
	k.Deliver(m)
	k.Fire()
	if k.Fake.Count("Revoke") != 1 {
		t.Fatalf("calls %v", k.Fake.Calls())
	}
	var fields []client.Field
	if err := json.Unmarshal([]byte(at.Fields), &fields); err != nil || len(fields) != 2 || fields[0].Text != modtest.SpamText {
		t.Fatalf("original fields %q (%v)", at.Fields, err)
	}
	if !strings.Contains(at.Normalised, "bitcoin") {
		t.Fatalf("normalised text %q lacks the folded word", at.Normalised)
	}
	if at.Rule != "pitch" || at.ConfigHash != k.Holder.Current().Hash || at.Subject != string(modtest.Spammer) ||
		at.Chat != string(modtest.G1) || at.Community != string(modtest.Community) || !at.MsgTime.Equal(modtest.T0) ||
		at.CreatedAt.IsZero() {
		t.Fatalf("evidence %+v", at)
	}
}

func withImage(m *client.Message, size uint64) *client.Message {
	m.Media = &client.Media{Kind: "image", MimeType: "image/jpeg", FileName: "offer.jpg", Size: size, Raw: []byte("raw")}
	return m
}

// TestAttachmentSavedInEvidence: an attachment within the size limit is saved
// next to the evidence copy.
func TestAttachmentSavedInEvidence(t *testing.T) {
	k := modtest.New(t, "")
	k.Fake.Download = func(_ context.Context, msg *client.Message) ([]byte, string, string, error) {
		if msg.Media == nil || string(msg.Media.Raw) != "raw" {
			t.Errorf("download got %+v", msg.Media)
		}
		return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
	}
	k.Deliver(withImage(k.Spam("M1", modtest.G1), 1000))
	if n, err := k.Media.Fetch(k.Ctx); err != nil || n != 1 {
		t.Fatalf("fetched %d (%v)", n, err)
	}
	e, _, _ := k.Store.Evidence(k.Ctx, revokeRow(t, k).EvidenceID)
	// The size recorded is the file's, not the 1000 bytes the post declared.
	if e.MediaState != store.MediaSaved || e.MediaKind != "image" || e.MediaName != "offer.jpg" || e.MediaSize != uint64(len("JPEGDATA")) {
		t.Fatalf("media %+v", e)
	}
	if b, err := os.ReadFile(e.MediaPath); err != nil || string(b) != "JPEGDATA" || !strings.HasSuffix(e.MediaPath, ".jpg") {
		t.Fatalf("file %s: %q %v", e.MediaPath, b, err)
	}
	if e.MediaRaw != nil {
		t.Fatal("the download description is kept after the download")
	}
	// A failed download is recorded, not retried forever.
	k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
		return nil, "", "", os.ErrDeadlineExceeded
	}
	k.Clock.Advance(time.Minute)
	k.Deliver(withImage(k.Msg("M2", modtest.GB, modtest.Member, modtest.SpamText), 1000))
	if _, err := k.Media.Fetch(k.Ctx); err != nil {
		t.Fatal(err)
	}
	evs, _ := k.Store.EvidenceFor(k.Ctx, modtest.MemberM.IDs())
	if len(evs) != 1 || evs[0].MediaState != store.MediaFailed || evs[0].MediaError == "" {
		t.Fatalf("failed download recorded as %+v", evs)
	}
}

// TestAttachmentDownloadNeverDelaysRevoke: the delete goes out while the
// attachment download is still running.
func TestAttachmentDownloadNeverDelaysRevoke(t *testing.T) {
	k := modtest.New(t, "")
	started, release := make(chan struct{}), make(chan struct{})
	k.Fake.Download = func(ctx context.Context, _ *client.Message) ([]byte, string, string, error) {
		close(started)
		<-release
		return []byte("x"), "image/jpeg", "", nil
	}
	k.Deliver(withImage(k.Spam("M1", modtest.G1), 1000))
	done := make(chan error, 1)
	go func() {
		_, err := k.Media.Fetch(k.Ctx)
		done <- err
	}()
	<-started
	k.Fire()
	if k.Fake.Count("Revoke") != 1 {
		t.Fatalf("the delete waited for the download: %v", k.Fake.Calls())
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestOversizeAttachmentRecordedNotKept: an attachment over
// evidence.max_attachment_mb is recorded by type, name and size only.
func TestOversizeAttachmentRecordedNotKept(t *testing.T) {
	k := modtest.New(t, "evidence:\n  max_attachment_mb: 5\n")
	k.Deliver(withImage(k.Spam("M1", modtest.G1), 5<<20+1))
	if n, err := k.Media.Fetch(k.Ctx); err != nil || n != 0 {
		t.Fatalf("fetched %d (%v)", n, err)
	}
	e, _, _ := k.Store.Evidence(k.Ctx, revokeRow(t, k).EvidenceID)
	if e.MediaState != store.MediaTooLarge || e.MediaKind != "image" || e.MediaName != "offer.jpg" || e.MediaSize != 5<<20+1 ||
		e.MediaRaw != nil || e.MediaPath != "" {
		t.Fatalf("oversize media %+v", e)
	}
	if k.Fake.Count("DownloadMedia") != 0 {
		t.Fatal("an oversize attachment was downloaded")
	}
	// The declared size is the sender's claim. A file declared small, or with
	// no size, that turns out over the limit is settled as too large, never
	// as a failure, and its size is recorded as unknown; the download was
	// asked to stop at the limit.
	k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
		return make([]byte, 5<<20+1), "image/jpeg", "offer.jpg", nil
	}
	for i, declared := range []uint64{1, 0} {
		k.Clock.Advance(time.Minute)
		k.Deliver(withImage(k.Msg(fmt.Sprintf("LIAR%d", i), modtest.GB, modtest.Member, modtest.SpamText), declared))
		if n, err := k.Media.Fetch(k.Ctx); err != nil || n != 1 {
			t.Fatalf("declared %d: fetched %d (%v)", declared, n, err)
		}
		if got := k.Fake.LastDownloadMax(); got != 5<<20 {
			t.Fatalf("declared %d: the download was capped at %d bytes, want %d", declared, got, 5<<20)
		}
	}
	evs, err := k.Store.EvidenceFor(k.Ctx, modtest.MemberM.IDs())
	if err != nil || len(evs) != 2 {
		t.Fatalf("evidence %+v (%v)", evs, err)
	}
	for _, e := range evs {
		if e.MediaState != store.MediaTooLarge || e.MediaSize != 0 || e.MediaPath != "" || e.MediaRaw != nil {
			t.Fatalf("an attachment declared small but over the limit: %+v", e)
		}
	}
}

// TestActionLogPurge: action-log rows (and the reports about them) go after
// retention.action_log_months; a row still queued is never purged.
func TestActionLogPurge(t *testing.T) {
	k := modtest.New(t, "")
	k.Deliver(k.Spam("OLD", modtest.G1))
	k.Fire()
	k.MarkAllSent()
	// A row still waiting in the outbox (removals paused) when the window passes.
	if err := k.Store.SetPause(k.Ctx, store.Pause{Source: store.SourceBreaker, Scope: store.ScopeRemoveBan, Reason: "t",
		Since: k.Clock.Now()}); err != nil {
		t.Fatal(err)
	}
	k.Deliver(k.Msg("QUEUED", modtest.GB, modtest.Member, modtest.SpamText))
	k.Fire()
	k.Clock.Advance(200 * 24 * time.Hour)
	k.Deliver(k.Msg("NEW", modtest.G1, modtest.Other1, modtest.SpamText))
	k.Fire()
	k.Clock.Advance(190 * 24 * time.Hour) // OLD and QUEUED are 390 days old, NEW 190
	k.MarkAllSent()
	res, err := purger(k).Purge(k.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows := k.Rows(modtest.SpammerM); len(rows) != 0 || res.Ledger == 0 {
		t.Fatalf("%d rows of a 390-day-old decision survived action_log_months 12 (purged %d)", len(rows), res.Ledger)
	}
	if reps := k.Reports(ledger.KindAction); len(reps) != 1 || reps[0].Subject != string(modtest.Other1) {
		t.Fatalf("reports left %+v, want only the 190-day-old one", reps)
	}
	queued := 0
	for _, r := range k.Rows(modtest.MemberM) {
		if r.Status == store.Intended && r.Mode == store.ModeEnforce {
			queued++
		}
	}
	if queued == 0 {
		t.Fatal("a row still in the outbox was purged")
	}
	if len(k.Rows(modtest.Other1M)) == 0 {
		t.Fatal("a 190-day-old row was purged")
	}
}

func purger(k *modtest.Kit) *ledger.Purger {
	return &ledger.Purger{Store: k.Store, Config: k.Holder, Log: k.Log, Now: k.Clock.Now}
}

// TestEvidencePurge: evidence copies and their files go after
// retention.evidence_days, using the longest value across communities.
func TestEvidencePurge(t *testing.T) {
	k := modtest.New(t, "")
	k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
		return []byte("x"), "image/jpeg", "a.jpg", nil
	}
	k.Deliver(withImage(k.Spam("OLD", modtest.G1), 10))
	if _, err := k.Media.Fetch(k.Ctx); err != nil {
		t.Fatal(err)
	}
	old, _, _ := k.Store.Evidence(k.Ctx, revokeRow(t, k).EvidenceID)
	k.Clock.Advance(20 * 24 * time.Hour)
	k.Deliver(k.Msg("NEW", modtest.GB, modtest.Member, modtest.SpamText))
	k.Clock.Advance(11 * 24 * time.Hour) // OLD is 31 days old, NEW 11
	if _, err := purger(k).Purge(k.Ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := k.Store.Evidence(k.Ctx, old.ID); ok {
		t.Fatal("a 31-day-old copy survived retention.evidence_days 30")
	}
	if _, err := os.Stat(old.MediaPath); !os.IsNotExist(err) {
		t.Fatalf("its attachment file is still there (%v)", err)
	}
	if evs, _ := k.Store.EvidenceFor(k.Ctx, modtest.MemberM.IDs()); len(evs) != 1 {
		t.Fatalf("the 11-day-old copy went too (%d left)", len(evs))
	}

	// One community keeps evidence 45 days: one purge serves all, so every
	// copy is kept that long.
	k2 := modtest.NewConfig(t, strings.Replace(modtest.Config, "    name: set b\n",
		"    name: set b\n    retention:\n      evidence_days: 45\n", 1))
	k2.Deliver(k2.Spam("A", modtest.G1))
	k2.Clock.Advance(15 * 24 * time.Hour)
	k2.Deliver(k2.Msg("B", modtest.G1, modtest.Member, modtest.SpamText))
	k2.Clock.Advance(31 * 24 * time.Hour) // A is 46 days old, B 31
	if _, err := purger(k2).Purge(k2.Ctx); err != nil {
		t.Fatal(err)
	}
	if evs, _ := k2.Store.EvidenceFor(k2.Ctx, modtest.SpammerM.IDs()); len(evs) != 0 {
		t.Fatal("a 46-day-old copy survived the longest window (45)")
	}
	if evs, _ := k2.Store.EvidenceFor(k2.Ctx, modtest.MemberM.IDs()); len(evs) != 1 {
		t.Fatal("a 31-day-old copy was purged although one community keeps 45 days")
	}
}

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/store/sessiontest"
)

const (
	cmdCommunity = "99999000000999@g.us"
	cmdGroup     = "99999000000111@g.us"
	spammerLID   = "99999000000444@lid"
	spammerPhone = "447700900123@s.whatsapp.net"
	memberLID    = "99999000000666@lid"
)

// sub runs a two-word command ("member show", "ban add") with the test config.
func (te *testEnv) sub(args ...string) int {
	te.out.Reset()
	te.errb.Reset()
	return te.run(append([]string{args[0], args[1], "--config", te.cfgPath}, args[2:]...))
}

// seeded is a data dir holding a banned spammer's record (decision, actions,
// evidence with an attachment file, report, a queued message, WhatsApp
// contact and LID rows) and another member's.
type seeded struct {
	st      *store.Store
	session *sql.DB
	file    string
}

func seed(t *testing.T, te *testEnv) seeded {
	t.Helper()
	te.provision(t)
	ctx := context.Background()
	dataDir := filepath.Join(te.dir, "data")
	st, err := store.Open(ctx, filepath.Join(dataDir, "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	write := func(p ledger.Plan) {
		t.Helper()
		if err := st.Write(ctx, func(tx *sql.Tx) error {
			_, err := ledger.Write(ctx, tx, p, time.Now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	spammer := client.Member{LID: spammerLID, Phone: spammerPhone}
	fields := `[{"name":"body","text":"cheap crypto https://example.org","match":"cheap crypto https://example.org"}]`
	write(ledger.Plan{Trigger: "M1", Target: spammer, Rule: "pitch", ConfigHash: "h1",
		Evidence: &store.Evidence{Chat: cmdGroup, Community: cmdCommunity, Sender: spammerLID, SenderAlt: spammerPhone,
			MsgID: "M1", TargetID: "M1", MsgTime: time.Now(), Fields: fields, Normalised: `["cheap crypto https://example.org"]`,
			MediaKind: "image", MediaState: store.MediaPending},
		Intents: []ledger.Intent{{Action: store.ActRevoke, Chat: cmdGroup, Community: cmdCommunity, Enforce: true},
			{Action: store.ActRemove, Chat: cmdGroup, Community: cmdCommunity, Enforce: true}},
		Ban: []string{store.BanEverywhere}, BanEnforce: true, BanCommunity: cmdCommunity,
		Reports: []store.Report{{Kind: ledger.KindAction, Community: cmdCommunity, Text: "deleted"}}})
	rows, err := st.LedgerForTargets(ctx, spammer.IDs())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Status == store.Intended {
			if _, err := st.Finish(ctx, r.ID, store.Requested, "", 0, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	file := filepath.Join(dataDir, "evidence", "1.jpg")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("JPEG"), 0o600); err != nil {
		t.Fatal(err)
	}
	evs, err := st.EvidenceFor(ctx, spammer.IDs())
	if err != nil || len(evs) != 1 {
		t.Fatalf("evidence %d %v", len(evs), err)
	}
	if err := st.SetMedia(ctx, evs[0].ID, store.MediaSaved, file, "", 4); err != nil {
		t.Fatal(err)
	}
	write(ledger.Plan{Trigger: "M2", Target: client.Member{LID: memberLID}, Rule: "watch", ConfigHash: "h1",
		Evidence: &store.Evidence{Chat: cmdGroup, Community: cmdCommunity, Sender: memberLID, MsgID: "M2", TargetID: "M2",
			MsgTime: time.Now(), Fields: `[]`, Normalised: `[]`},
		Reports: []store.Report{{Kind: ledger.KindLog, Community: cmdCommunity, Text: "matched"}}})
	for id, sender := range map[string]string{"Q1": spammerLID, "Q2": memberLID} {
		b, err := json.Marshal(&client.Message{Chat: cmdGroup, Sender: client.JID(sender), ID: id, TargetID: id})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.InboxPut(ctx, "msg|"+cmdGroup+"|"+id, "message", b); err != nil {
			t.Fatal(err)
		}
	}
	db := sessiontest.Create(t, filepath.Join(dataDir, "whatsmeow.db"))
	sessiontest.Exec(t, db, `INSERT INTO whatsmeow_contacts (our_jid, their_jid, push_name) VALUES ('bot', ?, 'Crypto King'), ('bot', ?, 'Friend')`,
		spammerPhone, memberLID)
	sessiontest.Exec(t, db, `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES ('99999000000444', '447700900123')`)
	return seeded{st: st, session: db, file: file}
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestLedgerSummary: `ledger summary` counts shadow and enforce rows by
// action for each community.
func TestLedgerSummary(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	if code := te.sub("ledger", "summary"); code != 0 || te.out.String() != "no actions recorded\n" {
		t.Fatalf("empty ledger: exit %d %q", code, te.out)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(te.dir, "data", "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	in := func(a store.Action, chat, community string, enforce bool) ledger.Intent {
		return ledger.Intent{Action: a, Chat: client.JID(chat), Community: community, Enforce: enforce}
	}
	for _, p := range []ledger.Plan{
		{Trigger: "M1", Target: client.Member{LID: spammerLID}, ConfigHash: "h", Ban: []string{store.BanEverywhere}, BanEnforce: true,
			BanCommunity: cmdCommunity, Intents: []ledger.Intent{in(store.ActRevoke, cmdGroup, cmdCommunity, true),
				in(store.ActRemove, cmdGroup, cmdCommunity, true), in(store.ActRemove, cmdCommunity, cmdCommunity, true),
				in(store.ActRemove, "99999000000888@g.us", "set-b", true)}},
		{Trigger: "M2", Target: client.Member{LID: memberLID}, ConfigHash: "h",
			Intents: []ledger.Intent{in(store.ActRevoke, cmdGroup, cmdCommunity, false)}},
	} {
		if err := st.Write(ctx, func(tx *sql.Tx) error {
			_, err := ledger.Write(ctx, tx, p, time.Now())
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if code := te.sub("ledger", "summary"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.errb)
	}
	want := cmdCommunity + " enforce ban=1 remove=2 revoke=1\n" + cmdCommunity + " shadow revoke=1\nset-b enforce remove=1\n"
	if te.out.String() != want {
		t.Fatalf("summary:\n%s\nwant:\n%s", te.out, want)
	}
}

// TestBanCLIAddRemoveList: `ban add|remove|list` by LID or phone number.
func TestBanCLIAddRemoveList(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	if code := te.sub("ban", "add", "+44 7700 900123"); code != 0 ||
		!strings.Contains(te.out.String(), "banned "+spammerPhone+" (scope *)") || !strings.Contains(te.out.String(), "next sweep") {
		t.Fatalf("ban add phone: exit %d %q %q", code, te.out, te.errb)
	}
	if code := te.sub("ban", "add", memberLID); code != 0 || !strings.Contains(te.out.String(), "banned "+memberLID) {
		t.Fatalf("ban add LID: exit %d %q", code, te.out)
	}
	if code := te.sub("ban", "list"); code != 0 || !strings.Contains(te.out.String(), "2 ban(s)") ||
		!strings.Contains(te.out.String(), memberLID+" scope=* lid="+memberLID) ||
		!strings.Contains(te.out.String(), spammerPhone+" scope=* lid=- phone="+spammerPhone) {
		t.Fatalf("ban list: exit %d %q", code, te.out)
	}
	if code := te.sub("ban", "remove", memberLID); code != 0 || te.out.String() != "unbanned "+memberLID+"\n" {
		t.Fatalf("ban remove: exit %d %q", code, te.out)
	}
	if code := te.sub("ban", "list"); code != 0 || !strings.Contains(te.out.String(), "1 ban(s)") || strings.Contains(te.out.String(), memberLID) {
		t.Fatalf("list after remove: %q", te.out)
	}
	if code := te.sub("ban", "remove", memberLID); code != exitFail || !strings.Contains(te.out.String(), "is not banned") {
		t.Fatalf("removing a ban that is not there: exit %d %q", code, te.out)
	}
	if code := te.sub("ban", "add", "not-a-number"); code != exitUsage {
		t.Fatalf("bad member: exit %d", code)
	}
	// Each change is in the action log with who made it.
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(te.dir, "data", "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.LedgerForTargets(ctx, []string{memberLID})
	if err != nil || len(rows) != 2 || rows[0].Action != store.ActBan || rows[1].Action != store.ActUnban || rows[1].Actor != "cli" {
		t.Fatalf("ledger %+v (%v)", rows, err)
	}
	// Under all_communities one ban covers every community, so --community is refused.
	te.cfgPath = te.writeConfig(t, "communities:\n  \""+cmdCommunity+"\": {}\n")
	for _, sub := range []string{"add", "remove"} {
		if code := te.sub("ban", sub, memberLID, "--community", cmdCommunity); code != exitUsage ||
			!strings.Contains(te.errb.String(), "all_communities") {
			t.Fatalf("ban %s --community under all_communities: exit %d %q", sub, code, te.errb)
		}
	}
}

// TestBanCLIPerCommunity: under bans.scope per_community, `ban remove
// --community` lifts the ban in that community only; without --community it
// lifts every ban.
func TestBanCLIPerCommunity(t *testing.T) {
	const otherCommunity = "99999000000888@g.us"
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	te.cfgPath = te.writeConfig(t, "communities:\n  \""+cmdCommunity+"\": {}\n  \""+otherCommunity+"\": {}\nbans:\n  scope: per_community\n")
	for _, c := range []string{cmdCommunity, otherCommunity} {
		if code := te.sub("ban", "add", memberLID, "--community", c); code != 0 {
			t.Fatalf("ban add in %s: exit %d %q", c, code, te.errb)
		}
	}
	if code := te.sub("ban", "remove", memberLID, "--community", cmdCommunity); code != 0 ||
		te.out.String() != "unbanned "+memberLID+" in "+cmdCommunity+"\n" {
		t.Fatalf("ban remove in one community: exit %d %q %q", code, te.out, te.errb)
	}
	if code := te.sub("ban", "list"); code != 0 || !strings.Contains(te.out.String(), "1 ban(s)") ||
		!strings.Contains(te.out.String(), memberLID+" scope="+otherCommunity+" ") {
		t.Fatalf("after lifting the ban in %s: %q, want the ban in %s kept", cmdCommunity, te.out, otherCommunity)
	}
	if code := te.sub("ban", "remove", memberLID, "--community", cmdCommunity); code != exitFail ||
		te.out.String() != memberLID+" is not banned in "+cmdCommunity+"\n" {
		t.Fatalf("lifting it again: exit %d %q", code, te.out)
	}
	if code := te.sub("ban", "add", memberLID, "--community", "99999000000777@g.us"); code != exitUsage ||
		!strings.Contains(te.errb.String(), "not a configured community") {
		t.Fatalf("unknown community: exit %d %q", code, te.errb)
	}
	// A community switched on with /enable is one --community can name.
	est, err := store.Open(context.Background(), filepath.Join(te.dir, "data", "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := est.Enable(context.Background(), store.Enabled{ID: "99999000000777@g.us", Kind: store.KindCommunity, By: "test",
		At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := est.Close(); err != nil {
		t.Fatal(err)
	}
	if code := te.sub("ban", "add", memberLID, "--community", "99999000000777@g.us"); code != 0 {
		t.Fatalf("ban add in a community switched on with /enable: exit %d %q", code, te.errb)
	}
	// Switched off with /disable, it keeps the ban, and --community lifts it.
	est, err = store.Open(context.Background(), filepath.Join(te.dir, "data", "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := est.Disable(context.Background(), "99999000000777@g.us"); err != nil {
		t.Fatal(err)
	}
	if err := est.Close(); err != nil {
		t.Fatal(err)
	}
	if code := te.sub("ban", "remove", memberLID, "--community", "99999000000777@g.us"); code != 0 ||
		te.out.String() != "unbanned "+memberLID+" in 99999000000777@g.us\n" {
		t.Fatalf("ban remove in a community switched off: exit %d %q %q", code, te.out, te.errb)
	}
	if code := te.sub("ban", "add", memberLID, "--community", "99999000000777@g.us"); code != exitUsage {
		t.Fatalf("ban add in a community switched off: exit %d %q", code, te.errb)
	}
	if code := te.sub("ban", "add", memberLID, "--community", cmdCommunity); code != 0 {
		t.Fatalf("ban add again: exit %d %q", code, te.errb)
	}
	if code := te.sub("ban", "remove", memberLID); code != 0 || te.out.String() != "unbanned "+memberLID+"\n" {
		t.Fatalf("ban remove everywhere: exit %d %q", code, te.out)
	}
	if code := te.sub("ban", "list"); code != 0 || te.out.String() != "0 ban(s)\n" {
		t.Fatalf("after lifting every ban: %q", te.out)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(te.dir, "data", "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows, err := st.LedgerForTargets(ctx, []string{memberLID})
	if err != nil {
		t.Fatal(err)
	}
	var lifted []string
	for _, r := range rows {
		if r.Action == store.ActUnban {
			lifted = append(lifted, r.Chat)
		}
	}
	if len(lifted) != 3 || lifted[0] != cmdCommunity || lifted[1] != "99999000000777@g.us" || lifted[2] != "" {
		t.Fatalf("unban rows lift %q, want %s, the switched-off community, then every community", lifted, cmdCommunity)
	}
}

// TestBanCLIHeldBan: a ban a pause holds is listed apart from active bans,
// survives `member forget`, and `ban remove` lifts it, so it never applies
// once the pause ends.
func TestBanCLIHeldBan(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(te.dir, "data", "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, err := ledger.Write(ctx, tx, ledger.Plan{Trigger: "M1", Target: client.Member{LID: memberLID}, ConfigHash: "h",
			Ban: []string{store.BanEverywhere}, BanEnforce: true, BanHeld: true, BanCommunity: cmdCommunity, Reason: "pitch"}, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code := te.sub("ban", "list"); code != 0 ||
		!strings.Contains(te.out.String(), memberLID+" scope=* held by a pause (applies once the pause ends) since=") ||
		!strings.HasSuffix(te.out.String(), "\n0 ban(s)\n1 ban(s) held by a pause\n") {
		t.Fatalf("ban list: exit %d %q", code, te.out)
	}
	// Forgetting the member keeps the held ban, saying so.
	if code := te.sub("member", "forget", memberLID); code != 0 ||
		!strings.Contains(te.out.String(), "kept: 1 ban(s) a pause holds") {
		t.Fatalf("member forget: exit %d %q %q", code, te.out, te.errb)
	}
	if code := te.sub("ban", "remove", memberLID); code != 0 || te.out.String() != "unbanned "+memberLID+"\n" {
		t.Fatalf("ban remove of a held ban: exit %d %q %q", code, te.out, te.errb)
	}
	if code := te.sub("ban", "list"); code != 0 || te.out.String() != "0 ban(s)\n" {
		t.Fatalf("after lifting the held ban: %q", te.out)
	}
	if held, err := st.HeldBans(ctx); err != nil || len(held) != 0 {
		t.Fatalf("held bans left %+v (%v)", held, err)
	}
}

// TestBanCLIHeldBanByPhone: `ban remove <phone>` lifts a ban a pause holds
// that names the member by LID and keeps their phone, with nothing else tying
// the two (someone who joined since the group list was last read).
func TestBanCLIHeldBanByPhone(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(te.dir, "data", "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, err := ledger.Write(ctx, tx, ledger.Plan{Trigger: "S1", Target: client.Member{LID: spammerLID, Phone: spammerPhone},
			ConfigHash: "h", Ban: []string{store.BanEverywhere}, BanEnforce: true, BanHeld: true, BanCommunity: cmdCommunity,
			Reason: "pitch"}, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code := te.sub("ban", "remove", spammerPhone); code != 0 {
		t.Fatalf("ban remove <phone> of a held ban: exit %d %q %q", code, te.out, te.errb)
	}
	if held, err := st.HeldBans(ctx); err != nil || len(held) != 0 {
		t.Fatalf("held bans left %+v (%v)", held, err)
	}
}

// TestBanCLIRemoveByPhoneFindsLIDBan: `ban remove <phone>` lifts a ban list
// entry that names the member by LID alone (an [Add to ban list] for someone
// who joined since the group list was last read, written before the entry
// carried the phone), found through the record of their post.
func TestBanCLIRemoveByPhoneFindsLIDBan(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(te.dir, "data", "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seenPost(t, st, "k1", "M1", spammerLID, spammerPhone)
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, err := ledger.Write(ctx, tx, ledger.Plan{Trigger: "A1", Target: client.Member{LID: spammerLID}, ConfigHash: "h",
			Ban: []string{store.BanEverywhere}, BanEnforce: true, BanCommunity: cmdCommunity, Reason: "added"}, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code := te.sub("ban", "remove", spammerPhone); code != 0 {
		t.Fatalf("ban remove <phone> of a LID-only ban: exit %d %q %q", code, te.out, te.errb)
	}
	if bans, err := st.Bans(ctx); err != nil || len(bans) != 0 {
		t.Fatalf("bans left %+v (%v)", bans, err)
	}
}

// TestBanCLIRemoveKeepsAnEarlierHoldersBan (#8, Expected Behavior 5): with
// the saved WhatsApp IDs pairing the spammer's LID with a number another LID
// held when it was banned, `ban remove <lid>` lifts what covers the person
// named and leaves the earlier holder banned on their LID, without the
// number; `ban remove <phone>` names the number and lifts every ban of it.
func TestBanCLIRemoveKeepsAnEarlierHoldersBan(t *testing.T) {
	const earlier = "99999000000777@lid"
	ctx := context.Background()
	for name, c := range map[string]struct {
		arg           string
		earlierBanned bool
	}{"by-lid": {arg: spammerLID, earlierBanned: true}, "by-phone": {arg: spammerPhone}} {
		t.Run(name, func(t *testing.T) {
			te, st := provisionedStore(t)
			db := sessiontest.Create(t, filepath.Join(te.dir, "data", "whatsmeow.db"))
			sessiontest.Exec(t, db, `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES ('99999000000444', '447700900123')`)
			writePlan(t, st, ledger.Plan{Trigger: "M1", Target: client.Member{LID: earlier, Phone: spammerPhone},
				Ban: []string{store.BanEverywhere}, BanEnforce: true, Reason: "pitch"})
			if code := te.sub("ban", "remove", c.arg); code != 0 || te.out.String() != "unbanned "+c.arg+"\n" {
				t.Fatalf("ban remove %s: exit %d %q %q", c.arg, code, te.out, te.errb)
			}
			if _, banned, err := st.FindBan(ctx, []string{spammerLID, spammerPhone}, ""); err != nil || banned {
				t.Fatalf("the person named is still banned (%v)", err)
			}
			b, banned, err := st.FindBan(ctx, []string{earlier}, "")
			if err != nil || banned != c.earlierBanned || banned && b.Phone != "" {
				t.Fatalf("earlier holder's ban %+v, banned %v (%v); want banned %v, without the number", b, banned, err,
					c.earlierBanned)
			}
		})
	}
}

// TestBanCLIRemoveOfAnEarlierHolderKeepsTheHoldersBan (#8 Gate 1): someone
// who held a number before and its holder now are both banned with it.
// `ban remove <lid>` of the earlier one finds the number is someone else's
// now (pair: the saved WhatsApp IDs, else the record of the number made
// last), so it lifts their ban alone and the holder's keeps the number. In
// the saved-ids case the earlier one's ban was recorded last, so only the
// saved IDs tell.
func TestBanCLIRemoveOfAnEarlierHolderKeepsTheHoldersBan(t *testing.T) {
	const earlier = "99999000000777@lid"
	ctx := context.Background()
	for name, saved := range map[string]bool{"saved-ids": true, "records": false} {
		t.Run(name, func(t *testing.T) {
			te, st := provisionedStore(t)
			order := []client.Member{{LID: earlier, Phone: spammerPhone}, {LID: spammerLID, Phone: spammerPhone}}
			if saved {
				db := sessiontest.Create(t, filepath.Join(te.dir, "data", "whatsmeow.db"))
				sessiontest.Exec(t, db, `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES ('99999000000444', '447700900123')`)
				order[0], order[1] = order[1], order[0]
			}
			now := time.Now()
			for i, who := range order {
				p := ledger.Plan{Trigger: "M" + who.Key(), Target: who, ConfigHash: "h", BanCommunity: cmdCommunity,
					Ban: []string{store.BanEverywhere}, BanEnforce: true, Reason: "pitch"}
				if err := st.Write(ctx, func(tx *sql.Tx) error {
					_, err := ledger.Write(ctx, tx, p, now.Add(time.Duration(i-2)*time.Hour))
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if code := te.sub("ban", "remove", earlier); code != 0 || te.out.String() != "unbanned "+earlier+"\n" {
				t.Fatalf("ban remove %s: exit %d %q %q", earlier, code, te.out, te.errb)
			}
			if _, banned, err := st.FindBan(ctx, []string{earlier}, ""); err != nil || banned {
				t.Fatalf("the earlier holder is still banned (%v)", err)
			}
			b, banned, err := st.FindBan(ctx, []string{spammerLID}, "")
			if err != nil || !banned || b.Member != spammerLID || b.Phone != spammerPhone {
				t.Fatalf("holder's ban %+v, banned %v (%v); want it kept with the number", b, banned, err)
			}
			if _, banned, err := st.FindBan(ctx, []string{spammerPhone}, ""); err != nil || !banned {
				t.Fatalf("the number alone is no longer banned (%v)", err)
			}
		})
	}
}

// TestBanCLIResolvesAtOnce: `ban add` pairs the address given with the other
// through the command line's steps of pipeline.LIDForNumber (the WhatsApp
// session store's saved IDs, then groupwarden's records; one hop) and writes
// the ban on the LID with the number at once; with no LID known the number is
// the key until the sweep. Someone already on the list keeps their one entry,
// and the command line's ban is recorded on it when it was the bot's.
func TestBanCLIResolvesAtOnce(t *testing.T) {
	const other = "99999000000777@lid"
	ctx := context.Background()
	open := provisionedStore
	// saved pairs the spammer's LID and number in the WhatsApp session store.
	saved := func(t *testing.T, te *testEnv) {
		t.Helper()
		db := sessiontest.Create(t, filepath.Join(te.dir, "data", "whatsmeow.db"))
		sessiontest.Exec(t, db, `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES ('99999000000444', '447700900123')`)
	}
	ban := func(t *testing.T, st *store.Store, p ledger.Plan) {
		t.Helper()
		p.Ban, p.BanEnforce, p.Reason = []string{store.BanEverywhere}, true, "pitch"
		writePlan(t, st, p)
	}
	add := func(t *testing.T, te *testEnv, arg, want string) {
		t.Helper()
		if code := te.sub("ban", "add", arg); code != 0 || te.out.String() != want {
			t.Fatalf("ban add %s: exit %d %q %q, want %q", arg, code, te.out, te.errb, want)
		}
	}
	// resolved: the one entry is keyed by the LID, with the number.
	resolved := func(t *testing.T, st *store.Store) {
		t.Helper()
		bans, err := st.Bans(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(bans) != 1 || bans[0].Member != spammerLID || bans[0].LID != spammerLID || bans[0].Phone != spammerPhone ||
			bans[0].Scope != store.BanEverywhere {
			t.Fatalf("bans %+v, want one keyed by %s with %s, everywhere", bans, spammerLID, spammerPhone)
		}
	}
	const number = "+44 7700 900123"
	banned, already := "banned "+spammerLID+" (scope *)\n", "already banned "+spammerLID+" (scope *)\n"
	unknown := "banned " + spammerPhone + " (scope *)\nthe running bot looks up this number's LID at its next sweep\n"
	t.Run("from-saved-ids", func(t *testing.T) {
		for _, arg := range []string{number, spammerLID} {
			te, st := open(t)
			saved(t, te)
			add(t, te, arg, banned)
			resolved(t, st)
		}
	})
	t.Run("from-record", func(t *testing.T) {
		for _, arg := range []string{number, spammerLID} {
			te, st := open(t)
			seenPost(t, st, "k1", "M1", spammerLID, spammerPhone) // a post it decided
			add(t, te, arg, banned)
			resolved(t, st)
		}
	})
	t.Run("from-held-ban", func(t *testing.T) {
		te, st := open(t)
		ban(t, st, ledger.Plan{Trigger: "S1", Target: client.Member{LID: spammerLID, Phone: spammerPhone}, BanHeld: true})
		add(t, te, number, banned)
		resolved(t, st)
		// The ban the pause held is in force now, as an added ban settles it.
		if held, err := st.HeldBans(ctx); err != nil || len(held) != 0 {
			t.Fatalf("held bans %+v (%v), want the held one settled", held, err)
		}
	})
	t.Run("saved-ids-win", func(t *testing.T) {
		te, st := open(t)
		saved(t, te)
		seenPost(t, st, "k1", "M1", other, spammerPhone) // the records pair the number with someone else
		add(t, te, number, banned)
		resolved(t, st)
	})
	t.Run("unknown-waits-for-sweep", func(t *testing.T) {
		te, st := open(t)
		add(t, te, number, unknown)
		if bans, err := st.Bans(ctx); err != nil || len(bans) != 1 || bans[0].Member != spammerPhone || bans[0].LID != "" {
			t.Fatalf("bans %+v (%v), want one keyed by the number", bans, err)
		}
	})
	t.Run("already-banned", func(t *testing.T) {
		te, st := open(t)
		saved(t, te)
		ban(t, st, ledger.Plan{Trigger: "M1", Target: client.Member{LID: spammerLID}}) // the bot's, by LID alone
		db, err := sql.Open("sqlite", filepath.Join(te.dir, "data", "groupwarden.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		link := func(t *testing.T) (id int64, trigger string) {
			t.Helper()
			if err := db.QueryRowContext(ctx, `SELECT l.id, l.trigger_id FROM bans b JOIN ledger l ON l.id = b.ledger_id`).
				Scan(&id, &trigger); err != nil {
				t.Fatal(err)
			}
			return id, trigger
		}
		add(t, te, number, already)
		id, trigger := link(t)
		if !strings.HasPrefix(trigger, store.CLITrigger) {
			t.Errorf("the entry links to a row triggered %q, want the ban command's (%s...)", trigger, store.CLITrigger)
		}
		resolved(t, st)
		// Again, by the LID: the entry keeps the command line's link.
		add(t, te, spammerLID, already)
		resolved(t, st)
		if again, _ := link(t); again != id {
			t.Fatalf("the entry links to row %d, want the command line's first ban (row %d) kept", again, id)
		}
		if n := count(t, db, `SELECT COUNT(*) FROM reports WHERE kind = ?`, ledger.KindBanCLI); n != 0 {
			t.Fatalf("%d %s report(s), want none: nobody was added", n, ledger.KindBanCLI)
		}
	})
	// An earlier `ban add` of the number found no LID, then the bot banned
	// them by LID alone: once the pair is known, `ban add` merges the two.
	t.Run("merges-number-entry", func(t *testing.T) {
		te, st := open(t)
		add(t, te, number, unknown)
		ban(t, st, ledger.Plan{Trigger: "M1", Target: client.Member{LID: spammerLID}})
		saved(t, te)
		add(t, te, number, already)
		resolved(t, st)
	})
}

type shown struct {
	Addresses []string
	Bans      []map[string]any
	Ledger    []map[string]any
	Evidence  []map[string]any
	Posts     []map[string]any
	Reports   []map[string]any
	Whatsmeow struct {
		Contacts []map[string]string `json:"whatsmeow_contacts"`
		LIDMap   []map[string]string `json:"whatsmeow_lid_map"`
	} `json:"whatsmeow"`
}

// TestMemberShowIncludesWhatsmeowContact: `member show` prints every record
// about the person, the WhatsApp session store's contact and LID rows
// included, found by either of their addresses.
func TestMemberShowIncludesWhatsmeowContact(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	s := seed(t, te)
	seenPost(t, s.st, "k1", "M1", spammerLID, spammerPhone)
	seenPost(t, s.st, "k2", "M2", memberLID, "")
	show := func() shown {
		t.Helper()
		if code := te.sub("member", "show", spammerLID); code != 0 {
			t.Fatalf("exit %d: %s", code, te.errb)
		}
		var rec shown
		if err := json.Unmarshal([]byte(te.out.String()), &rec); err != nil {
			t.Fatalf("%v\n%s", err, te.out)
		}
		return rec
	}
	rec := show()
	// An attachment's size is shown when known: the file's, once downloaded.
	if m, _ := rec.Evidence[0]["Media"].(string); !strings.HasSuffix(m, " 4 bytes (saved)") {
		t.Fatalf("evidence media %q, want the file's size", m)
	}
	if len(rec.Whatsmeow.Contacts) != 1 || rec.Whatsmeow.Contacts[0]["their_jid"] != spammerPhone ||
		rec.Whatsmeow.Contacts[0]["push_name"] != "Crypto King" {
		t.Fatalf("contacts %+v", rec.Whatsmeow.Contacts)
	}
	if len(rec.Whatsmeow.LIDMap) != 1 || rec.Whatsmeow.LIDMap[0]["pn"] != "447700900123" {
		t.Fatalf("LID map %+v", rec.Whatsmeow.LIDMap)
	}
	if len(rec.Ledger) != 3 || len(rec.Evidence) != 1 || len(rec.Bans) != 1 || len(rec.Reports) != 1 {
		t.Fatalf("ledger %d evidence %d bans %d reports %d", len(rec.Ledger), len(rec.Evidence), len(rec.Bans), len(rec.Reports))
	}
	// The seen table's record of their posts (store migration 12's sender).
	if len(rec.Posts) != 1 || rec.Posts[0]["MsgID"] != "M1" || rec.Posts[0]["Sender"] != spammerLID {
		t.Fatalf("posts %+v, want their one post on record", rec.Posts)
	}
	if strings.Contains(te.out.String(), "Friend") || strings.Contains(te.out.String(), memberLID) {
		t.Fatal("another member's records were printed")
	}
	// The phone number finds the same person.
	if code := te.sub("member", "show", "447700900123"); code != 0 || !strings.Contains(te.out.String(), "Crypto King") {
		t.Fatalf("by phone: exit %d", code)
	}
	// A download stopped at the limit leaves the size unknown.
	if err := s.st.SetMedia(te.ctx, int64(rec.Evidence[0]["ID"].(float64)), store.MediaTooLarge, "", "", 0); err != nil {
		t.Fatal(err)
	}
	if m, _ := show().Evidence[0]["Media"].(string); !strings.HasSuffix(m, " size unknown (too_large)") {
		t.Fatalf("evidence media %q, want the size unknown", m)
	}
}

// TestMemberForgetKeepsActiveBan: `member forget` deletes the person's
// records but keeps an active ban, saying why.
func TestMemberForgetKeepsActiveBan(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	s := seed(t, te)
	if code := te.sub("member", "forget", spammerLID); code != 0 {
		t.Fatalf("exit %d: %s", code, te.errb)
	}
	if !strings.Contains(te.out.String(), "kept: ban of "+spammerLID) || !strings.Contains(te.out.String(), "ban remove") {
		t.Fatalf("output %q", te.out)
	}
	ctx := context.Background()
	if _, banned, err := s.st.FindBan(ctx, []string{spammerLID}, ""); err != nil || !banned {
		t.Fatalf("the active ban went (%v)", err)
	}
	if rows, _ := s.st.LedgerForTargets(ctx, []string{spammerLID}); len(rows) != 0 {
		t.Fatalf("%d action-log rows left", len(rows))
	}
	if evs, _ := s.st.EvidenceFor(ctx, []string{spammerLID}); len(evs) != 0 {
		t.Fatal("evidence left")
	}
	if _, err := os.Stat(s.file); !os.IsNotExist(err) {
		t.Fatalf("attachment file still there (%v)", err)
	}
	if evs, _ := s.st.EvidenceFor(ctx, []string{memberLID}); len(evs) != 1 {
		t.Fatal("another member's evidence went too")
	}
}

// TestMemberForgetByPhoneFindsLIDRecords: `member forget <phone>` reaches the
// records the bot keeps under the person's LID alone (someone who joined
// since the group list was last read), through the record of a post that
// paired the two, with no WhatsApp session store to ask.
func TestMemberForgetByPhoneFindsLIDRecords(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(te.dir, "data", "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seenPost(t, st, "k1", "M1", spammerLID, spammerPhone)
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, err := ledger.Write(ctx, tx, ledger.Plan{Trigger: "M2", Target: client.Member{LID: spammerLID}, ConfigHash: "h",
			Evidence: &store.Evidence{Chat: cmdGroup, Community: cmdCommunity, Sender: spammerLID, MsgID: "M2",
				TargetID: "M2", MsgTime: time.Now(), Fields: `[]`, Normalised: `[]`},
			Intents: []ledger.Intent{{Action: store.ActRevoke, Chat: cmdGroup, Community: cmdCommunity}}}, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if code := te.sub("member", "forget", spammerPhone); code != 0 {
		t.Fatalf("exit %d: %s", code, te.errb)
	}
	// A shadow row: one still queued in the outbox is kept until it is sent.
	if rows, err := st.LedgerForTargets(ctx, []string{spammerLID}); err != nil || len(rows) != 0 {
		t.Fatalf("%d action-log rows under the LID left (%v)", len(rows), err)
	}
	if evs, err := st.EvidenceFor(ctx, []string{spammerLID}); err != nil || len(evs) != 0 {
		t.Fatalf("%d evidence rows under the LID left (%v)", len(evs), err)
	}
}

// TestMemberForgetClearsInboxAndWhatsmeowRows: forgetting also deletes the
// person's queued messages and their WhatsApp contact and LID rows, and
// nobody else's.
func TestMemberForgetClearsInboxAndWhatsmeowRows(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	s := seed(t, te)
	if code := te.sub("member", "forget", "447700900123"); code != 0 {
		t.Fatalf("exit %d: %s", code, te.errb)
	}
	ctx := context.Background()
	rows, err := s.st.InboxOldest(ctx, 10)
	if err != nil || len(rows) != 1 || !strings.Contains(string(rows[0].Payload), memberLID) {
		t.Fatalf("inbox %+v (%v), want only the other member's message", rows, err)
	}
	if n := count(t, s.session, `SELECT COUNT(*) FROM whatsmeow_contacts WHERE their_jid = ?`, spammerPhone); n != 0 {
		t.Fatalf("%d contact rows left", n)
	}
	if n := count(t, s.session, `SELECT COUNT(*) FROM whatsmeow_lid_map`); n != 0 {
		t.Fatalf("%d LID map rows left", n)
	}
	if n := count(t, s.session, `SELECT COUNT(*) FROM whatsmeow_contacts WHERE their_jid = ?`, memberLID); n != 1 {
		t.Fatal("another member's contact row went too")
	}
	if !strings.Contains(te.out.String(), "1 queued messages") || !strings.Contains(te.out.String(), "2 WhatsApp contact/LID rows") {
		t.Fatalf("output %q", te.out)
	}
}

// TestMemberForgetStripsTelegramReports: forgetting queues, for every report
// about the person, the removal of its message text and the deletion of any
// attachment post still showing.
func TestMemberForgetStripsTelegramReports(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	s := seed(t, te)
	ctx := context.Background()
	reps, err := s.st.ReportsFor(ctx, []string{spammerLID})
	if err != nil || len(reps) != 1 {
		t.Fatalf("reports %d %v", len(reps), err)
	}
	if code := te.sub("member", "forget", spammerLID); code != 0 {
		t.Fatalf("exit %d: %s", code, te.errb)
	}
	edits, err := s.st.ReportEdits(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range edits {
		if e.ReportID != reps[0].ID {
			t.Fatalf("an edit was queued for another report: %+v", e)
		}
		got[e.Op] = true
	}
	if len(edits) != 2 || !got[store.EditStripText] || !got[store.EditDeleteAttachment] {
		t.Fatalf("edits %+v", edits)
	}
	// Forgetting again queues nothing twice.
	if code := te.sub("member", "forget", spammerLID); code != 0 {
		t.Fatal("second forget failed")
	}
	if again, _ := s.st.ReportEdits(ctx); len(again) != 2 {
		t.Fatalf("%d edits after a second forget", len(again))
	}
}

// TestKnownIDsOneHop: the command line acts on the one person named. `ban
// remove` takes the address given and its pair: the saved WhatsApp IDs'
// first, else the bot's records'. `member show|forget` gather one step from
// the address given (its saved pair, its record pair over every ban row of
// theirs, the addresses on the ban entries that name it) and never a second,
// so a number that passed from one person to another reaches only one of
// them, and a lifted ban still leads `member forget` to their records.
func TestKnownIDsOneHop(t *testing.T) {
	const (
		x, y     = "99999000000777@lid", "99999000000888@lid"
		old, now = "447700900456@s.whatsapp.net", "447700900789@s.whatsapp.net"
	)
	ctx := context.Background()
	open, write := provisionedStore, writePlan
	ban := func(t *testing.T, st *store.Store, trigger string, m client.Member) {
		t.Helper()
		write(t, st, ledger.Plan{Trigger: trigger, Target: m, Ban: []string{store.BanEverywhere}, BanEnforce: true, Reason: "test"})
	}
	banned := func(t *testing.T, st *store.Store, id string) bool {
		t.Helper()
		_, ok, err := st.FindBan(ctx, []string{id}, "")
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	// moved: x held old, was banned and lifted, and holds now, banned again
	// by LID; y holds old now, banned, with a reported post under their LID
	// alone. The session store pairs each LID with the number it holds now.
	moved := func(t *testing.T) (*testEnv, *store.Store) {
		t.Helper()
		te, st := open(t)
		ban(t, st, "X1", client.Member{LID: x, Phone: old})
		write(t, st, ledger.Plan{Trigger: "X2", Target: client.Member{LID: x, Phone: old}, Lift: true, Reason: "lifted"})
		ban(t, st, "X3", client.Member{LID: x})
		write(t, st, ledger.Plan{Trigger: "Y1", Target: client.Member{LID: y, Phone: old}, Rule: "pitch",
			Evidence: &store.Evidence{Chat: cmdGroup, Community: cmdCommunity, Sender: y, MsgID: "Y1", TargetID: "Y1",
				MsgTime: time.Now(), Fields: `[]`, Normalised: `[]`},
			Ban: []string{store.BanEverywhere}, BanEnforce: true, Reason: "pitch"})
		db := sessiontest.Create(t, filepath.Join(te.dir, "data", "whatsmeow.db"))
		sessiontest.Exec(t, db, `INSERT INTO whatsmeow_lid_map (lid, pn) VALUES ('99999000000777', '447700900789'), ('99999000000888', '447700900456')`)
		return te, st
	}
	t.Run("saved-ids-win", func(t *testing.T) {
		te, st := moved(t)
		seenPost(t, st, "k1", "M1", x, old) // the bot's latest record pairs old with x
		if code := te.sub("ban", "remove", old); code != 0 {
			t.Fatalf("exit %d: %s", code, te.errb)
		}
		if banned(t, st, y) || !banned(t, st, x) {
			t.Fatalf("y banned %v, x banned %v; want only y, who holds the number now, lifted", banned(t, st, y), banned(t, st, x))
		}
	})
	t.Run("other-entry-ignored", func(t *testing.T) {
		te, _ := moved(t)
		if code := te.sub("member", "show", x); code != 0 {
			t.Fatalf("exit %d: %s", code, te.errb)
		}
		var rec shown
		if err := json.Unmarshal([]byte(te.out.String()), &rec); err != nil {
			t.Fatalf("%v\n%s", err, te.out)
		}
		if slices.Contains(rec.Addresses, y) || !slices.Contains(rec.Addresses, old) || !slices.Contains(rec.Addresses, now) {
			t.Fatalf("addresses %v, want x's own (its saved pair %s, its old number %s) and not %s", rec.Addresses, now, old, y)
		}
	})
	t.Run("ban-remove-lifts-one-person", func(t *testing.T) {
		te, st := moved(t)
		if code := te.sub("ban", "remove", x); code != 0 {
			t.Fatalf("exit %d: %s", code, te.errb)
		}
		if banned(t, st, x) || !banned(t, st, y) {
			t.Fatalf("x banned %v, y banned %v; want only x lifted", banned(t, st, x), banned(t, st, y))
		}
	})
	t.Run("member-forget-keeps-other-person", func(t *testing.T) {
		te, st := moved(t)
		if code := te.sub("member", "forget", x); code != 0 {
			t.Fatalf("exit %d: %s", code, te.errb)
		}
		evs, err := st.EvidenceFor(ctx, []string{y})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := st.LedgerForTargets(ctx, []string{y})
		if err != nil || len(evs) != 1 || len(rows) == 0 {
			t.Fatalf("y's evidence %d, action-log rows %d (%v); want y's records kept", len(evs), len(rows), err)
		}
	})
	// The bot banned them by LID and number, the ban was lifted, and there is
	// no session store: `member forget <phone>` still finds their LID through
	// the lifted ban's row.
	t.Run("member-forget-after-lift", func(t *testing.T) {
		te, st := open(t)
		ban(t, st, "M1", client.Member{LID: spammerLID, Phone: spammerPhone})
		if code := te.sub("ban", "remove", spammerLID); code != 0 {
			t.Fatalf("ban remove: exit %d: %s", code, te.errb)
		}
		if code := te.sub("member", "forget", spammerPhone); code != 0 {
			t.Fatalf("member forget: exit %d: %s", code, te.errb)
		}
		db, err := sql.Open("sqlite", filepath.Join(te.dir, "data", "groupwarden.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if n := count(t, db, `SELECT COUNT(*) FROM ledger WHERE target = ?`, spammerLID); n != 0 {
			t.Fatalf("%d action-log rows under the LID left", n)
		}
		if n := count(t, db, `SELECT COUNT(*) FROM ledger WHERE address = ?`, spammerPhone); n != 0 {
			t.Fatalf("%d action-log rows holding the number left", n)
		}
	})
	// A post reported under their LID alone, then a number ban resolved on
	// the LID: only the ban entry pairs the two (the ban's row is keyed by
	// the number), and `member forget <phone>` reaches the post through it.
	t.Run("member-forget-entry-only-pair", func(t *testing.T) {
		te, st := open(t)
		write(t, st, ledger.Plan{Trigger: "M1", Target: client.Member{LID: spammerLID}, Rule: "watch",
			Evidence: &store.Evidence{Chat: cmdGroup, Community: cmdCommunity, Sender: spammerLID, MsgID: "M1", TargetID: "M1",
				MsgTime: time.Now(), Fields: `[]`, Normalised: `[]`}})
		ban(t, st, "C1", client.Member{Phone: spammerPhone})
		if err := st.ResolveBan(ctx, spammerPhone, spammerLID); err != nil {
			t.Fatal(err)
		}
		if code := te.sub("member", "forget", spammerPhone); code != 0 {
			t.Fatalf("exit %d: %s", code, te.errb)
		}
		if evs, err := st.EvidenceFor(ctx, []string{spammerLID}); err != nil || len(evs) != 0 {
			t.Fatalf("%d evidence rows under the LID left (%v)", len(evs), err)
		}
	})
}

// provisionedStore is a provisioned test environment and its groupwarden.db,
// open (closed when the test ends).
func provisionedStore(t *testing.T) (*testEnv, *store.Store) {
	t.Helper()
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	st, err := store.Open(context.Background(), filepath.Join(te.dir, "data", "groupwarden.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return te, st
}

// writePlan writes p to st, counting towards cmdCommunity.
func writePlan(t *testing.T, st *store.Store, p ledger.Plan) {
	t.Helper()
	p.ConfigHash, p.BanCommunity = "h", cmdCommunity
	if err := st.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := ledger.Write(context.Background(), tx, p, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// seenPost records a decided post of sender (alt: their other address) in
// the seen table, as the inbox worker does.
func seenPost(t *testing.T, st *store.Store, key, msgID, sender, alt string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.InboxPut(ctx, key, "message", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	rows, err := st.InboxOldest(ctx, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("inbox %+v (%v)", rows, err)
	}
	if err := st.Decide(ctx, rows[0], store.Seen{Chat: cmdGroup, MsgID: msgID, ServerTime: time.Now(), Sender: sender,
		SenderAlt: alt}, func(*sql.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

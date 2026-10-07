package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
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
	if len(lifted) != 2 || lifted[0] != cmdCommunity || lifted[1] != "" {
		t.Fatalf("unban rows lift %q, want %s then every community", lifted, cmdCommunity)
	}
}

type shown struct {
	Addresses []string
	Bans      []map[string]any
	Ledger    []map[string]any
	Evidence  []map[string]any
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

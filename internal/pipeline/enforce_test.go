package pipeline_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
)

func change(k *modtest.Kit, group, actor client.JID, joined, left []client.JID) *client.GroupChange {
	k.Clock.Advance(time.Minute)
	return &client.GroupChange{Group: group, Actor: actor, Time: k.Clock.Now(), Joined: joined, Left: left}
}

func removed(k *modtest.Kit, group, member client.JID) int {
	return k.Fake.Count("Remove " + string(group) + " " + string(member))
}

// TestEditIntoSpamRevokesOriginal: a clean message edited into spam is
// deleted by the ORIGINAL message's ID, aged by the original's server time.
func TestEditIntoSpamRevokesOriginal(t *testing.T) {
	k := modtest.New(t, "")
	orig := k.Msg("ORIG", modtest.G1, modtest.Spammer, "hello everyone")
	k.Deliver(orig)
	k.Clock.Advance(5 * time.Minute)
	edit := k.Msg("EDIT", modtest.G1, modtest.Spammer, modtest.SpamText)
	edit.TargetID, edit.IsEdit = "ORIG", true
	k.Deliver(edit)
	rows := k.Find(modtest.SpammerM, store.ActRevoke, modtest.G1)
	if len(rows) != 1 || rows[0].MsgID != "ORIG" || !rows[0].MsgTime.Equal(orig.Time) || rows[0].TriggerID != "EDIT" {
		t.Fatalf("revoke rows %+v, want the original's ID and time", rows)
	}
	k.Fire()
	if k.Fake.Count("Revoke "+string(modtest.G1)+" "+string(modtest.Spammer)+" ORIG") != 1 || k.Fake.Count("Revoke "+string(modtest.G1)+" "+string(modtest.Spammer)+" EDIT") != 0 {
		t.Fatalf("calls %v", k.Fake.Calls())
	}
}

// TestJoinedGroupModeratedAtOnce: a linked group the bot has just joined is
// moderated from the join notification itself, with no group list read
// (the read a join asks for can fail): spam posted there is deleted and its
// sender removed and banned. Before the join the group is not moderated.
func TestJoinedGroupModeratedAtOnce(t *testing.T) {
	const g3 client.JID = "99999000000333@g.us"
	k := modtest.New(t, "")
	k.Deliver(k.Spam("BEFORE", g3))
	k.Fire()
	if n := k.Fake.Count("Revoke " + string(g3)); n != 0 {
		t.Fatalf("a group the bot is not in was moderated: %v", k.Fake.Calls())
	}
	bot := client.Participant{JID: modtest.Bot, LID: modtest.Bot, Phone: modtest.BotPhone, IsAdmin: true}
	spammer := client.Participant{JID: modtest.Spammer, LID: modtest.Spammer, Phone: modtest.SpammerPhone}
	k.Deliver(&client.JoinedGroup{Group: g3, Time: k.Clock.Now(), Info: client.Group{JID: g3, Name: "events",
		Parent: modtest.Community, Participants: []client.Participant{bot, spammer}}})
	k.Deliver(k.Spam("AFTER", g3))
	k.Fire()
	if n := k.Fake.Count("Revoke " + string(g3) + " " + string(modtest.Spammer) + " AFTER"); n != 1 {
		t.Fatalf("spam in the joined group was not deleted: %v", k.Fake.Calls())
	}
	if removed(k, g3, modtest.Spammer) != 1 || !k.Banned(modtest.SpammerM, "") {
		t.Fatalf("the sender was not removed from the joined group and banned: %v", k.Fake.Calls())
	}
	if n := k.Fake.Count("JoinedGroups"); n != 0 {
		t.Fatalf("the group list was read %d times", n)
	}
}

// TestBannedRejoinByLinkRemoved: a banned person joining through an invite
// link is removed and the admins are told.
func TestBannedRejoinByLinkRemoved(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.Other1M)
	ev := change(k, modtest.G2, modtest.Other1, []client.JID{modtest.Other1}, nil)
	ev.JoinReason = "invite"
	k.Deliver(ev)
	k.Fire()
	if removed(k, modtest.G2, modtest.Other1) != 1 {
		t.Fatalf("calls %v", k.Fake.Calls())
	}
	reps := k.Reports(ledger.KindBannedRejoin)
	if len(reps) != 1 || !strings.Contains(reps[0].Text, "invite link") || reps[0].Priority {
		t.Fatalf("reports %+v", reps)
	}
	// Someone not banned joins and stays.
	k.Deliver(change(k, modtest.G2, modtest.Other2, []client.JID{modtest.Other2}, nil))
	k.Fire()
	if removed(k, modtest.G2, modtest.Other2) != 0 {
		t.Fatal("removed someone who is not banned")
	}
	if n := k.Logged("banned member joined; removal planned"); n != 1 {
		t.Fatalf("%d log lines for the banned join, want 1", n)
	}
}

// TestBannedMemberReportsSayWhatWaits: under a pause the reports of a banned
// member who joined, or whose join request a sweep found, say the removal or
// rejection waits for the pause to end; without one they say it is done.
func TestBannedMemberReportsSayWhatWaits(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprintf("paused=%v", paused), func(t *testing.T) {
			k := modtest.New(t, "")
			if paused {
				if err := k.Store.SetPause(k.Ctx, store.Pause{Source: store.SourceBreaker, Scope: store.ScopeRemoveBan,
					Reason: "test", Since: k.Clock.Now()}); err != nil {
					t.Fatal(err)
				}
			}
			k.Ban(modtest.Other1M)
			ev := change(k, modtest.G2, modtest.Other1, []client.JID{modtest.Other1}, nil)
			ev.JoinReason = "invite"
			k.Deliver(ev)
			k.Fake.Requests = map[client.JID][]client.JoinRequest{modtest.G1: {{JID: modtest.Other1}}}
			if err := k.Enforcer.CheckJoinRequests(k.Ctx, modtest.G1, "run1"); err != nil {
				t.Fatal(err)
			}
			removed, rejected := "removed.", "request rejected."
			if paused {
				removed, rejected = "removed once the pause ends.", "request rejected once the pause ends."
			}
			reps := k.Reports(ledger.KindBannedRejoin)
			if len(reps) != 2 || !strings.HasSuffix(reps[0].Text, "(by invite link): "+removed) ||
				!strings.HasSuffix(reps[1].Text, "(banned member asked to join): "+rejected) {
				t.Fatalf("reports %+v", reps)
			}
		})
	}
}

// TestBannedJoinRequestRejected: a banned person's pending join request is
// rejected when the sweep polls requests; other requests are left alone.
func TestBannedJoinRequestRejected(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.Other1M)
	k.Fake.Requests = map[client.JID][]client.JoinRequest{modtest.G1: {{JID: modtest.Other1}, {JID: modtest.Other2}}}
	if err := k.Enforcer.CheckJoinRequests(k.Ctx, modtest.G1, "run1"); err != nil {
		t.Fatal(err)
	}
	k.Fire()
	if n := k.Fake.Count("RejectJoinRequests " + string(modtest.G1) + " " + string(modtest.Other1)); n != 1 {
		t.Fatalf("rejected %d times: %v", n, k.Fake.Calls())
	}
	if k.Fake.Count("RejectJoinRequests "+string(modtest.G1)+" "+string(modtest.Other2)) != 0 {
		t.Fatal("rejected someone who is not banned")
	}
	if r := k.Find(modtest.Other1M, store.ActReject, modtest.G1); len(r) != 1 || r[0].Status != store.Requested {
		t.Fatalf("reject rows %+v", r)
	}
}

// TestCommunityOrPerGroupRemove: where the bot is a community admin and the
// community enforces, the ban removes at community level as well as from each
// group the person is in; elsewhere it removes group by group.
func TestCommunityOrPerGroupRemove(t *testing.T) {
	k := modtest.New(t, "")
	k.Dir.Apply(&client.GroupChange{Group: modtest.GB, Joined: []client.JID{modtest.Spammer}})
	k.Deliver(k.Spam("M1", modtest.G1))
	k.Fire()
	for _, g := range []client.JID{modtest.Community, modtest.G1, modtest.G2, modtest.GB} {
		if removed(k, g, modtest.Spammer) != 1 {
			t.Errorf("not removed from %s: %v", g, k.Fake.Calls())
		}
	}
	// The bot is not a community admin: group by group only.
	k2 := modtest.New(t, "")
	groups := modtest.Groups()
	groups[0].Participants = groups[0].Participants[1:] // the bot is no longer in the community's admin list
	modtest.Load(t, k2.Dir, groups)
	k2.Deliver(k2.Spam("M1", modtest.G1))
	k2.Fire()
	if removed(k2, modtest.Community, modtest.Spammer) != 0 {
		t.Fatal("removed at community level without being a community admin")
	}
	if removed(k2, modtest.G1, modtest.Spammer) != 1 || removed(k2, modtest.G2, modtest.Spammer) != 1 {
		t.Fatalf("calls %v", k2.Fake.Calls())
	}
}

// TestFanOutNeverRemovesHumanAdmin: a banned person who is a current human
// admin is not removed; the admins get a priority report instead.
func TestFanOutNeverRemovesHumanAdmin(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.AdminM)
	if err := k.Enforcer.CheckPresent(k.Ctx, modtest.G1, "run1"); err != nil {
		t.Fatal(err)
	}
	k.Deliver(change(k, modtest.G2, modtest.Member, []client.JID{modtest.Admin}, nil))
	k.Fire()
	if k.Fake.Count("Remove") != 0 {
		t.Fatalf("removed a human admin: %v", k.Fake.Calls())
	}
	if rows := k.Find(modtest.AdminM, store.ActRemove, modtest.G2); len(rows) != 0 {
		t.Fatalf("a removal of a human admin was planned: %+v", rows)
	}
	reps := k.Reports(ledger.KindAdminSpared)
	if len(reps) == 0 || !reps[0].Priority {
		t.Fatalf("reports %+v, want a priority report", reps)
	}

	// Someone promoted to admin after their removal was planned is not
	// removed either: the fire-time check sees the promotion.
	k.Ban(modtest.Other1M)
	if err := k.Store.SetPause(k.Ctx, store.Pause{Source: store.SourceBreaker, Scope: store.ScopeRemoveBan, Reason: "t",
		Since: k.Clock.Now()}); err != nil {
		t.Fatal(err)
	}
	k.Deliver(change(k, modtest.G1, modtest.Member, []client.JID{modtest.Other1}, nil))
	k.Fire()
	if len(k.Find(modtest.Other1M, store.ActRemove, modtest.G1)) != 1 {
		t.Fatal("no removal planned for a banned non-admin")
	}
	k.Dir.Apply(&client.GroupChange{Group: modtest.G1, Promoted: []client.JID{modtest.Other1}})
	if err := k.Exec.Resume(k.Ctx); err != nil {
		t.Fatal(err)
	}
	k.Fire()
	if k.Fake.Count("Remove") != 0 {
		t.Fatalf("removed someone promoted to admin before the removal fired: %v", k.Fake.Calls())
	}
	if r := k.Find(modtest.Other1M, store.ActRemove, modtest.G1); r[0].Status != store.Failed || !strings.Contains(r[0].Reason, "admin") {
		t.Fatalf("row %+v", r[0])
	}
	spared := k.Reports(ledger.KindAdminSpared)
	if len(spared) <= len(reps) {
		t.Fatalf("%d admin reports, want more than %d (one per spared removal)", len(spared), len(reps))
	}
	if last := spared[len(spared)-1]; !strings.Contains(last.Text, "the member is a current admin in community a.") {
		t.Fatalf("the fire-time report %q does not name the community", last.Text)
	}
}

// TestHumanAdminAddNotReversed: a banned person added by a current human
// admin stays; the add lifts the ban, is recorded with who did it, and the
// admins are told.
func TestHumanAdminAddNotReversed(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.Other1M)
	k.Deliver(change(k, modtest.G1, modtest.Admin, []client.JID{modtest.Other1}, nil))
	k.Fire()
	if k.Fake.Count("Remove") != 0 {
		t.Fatalf("reversed a human admin's add: %v", k.Fake.Calls())
	}
	if k.Banned(modtest.Other1M, "") {
		t.Fatal("the ban was not lifted")
	}
	var lift *store.LedgerRow
	for _, r := range k.Rows(modtest.Other1M) {
		if r.Action == store.ActUnban {
			lift = &r
		}
	}
	if lift == nil || lift.Actor != string(modtest.Admin) || lift.Reason != "ban lifted by human re-add" || lift.CreatedAt.IsZero() {
		t.Fatalf("lift row %+v", lift)
	}
	if reps := k.Reports(ledger.KindBanLifted); len(reps) != 1 {
		t.Fatalf("reports %+v", reps)
	}
	if n := k.Logged("ban lifted: a human admin re-added a banned member"); n != 1 {
		t.Fatalf("%d log lines for the lift, want 1", n)
	}
}

// TestHumanAdminAddLiftsOnlyThatCommunity: under bans.scope per_community, a
// human admin re-adding someone banned in two communities lifts the ban in
// their own community only.
func TestHumanAdminAddLiftsOnlyThatCommunity(t *testing.T) {
	k := modtest.New(t, "bans:\n  scope: per_community\n")
	a, b := string(modtest.Community), modtest.SetB
	if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
		_, err := ledger.Write(k.Ctx, tx, ledger.Plan{Trigger: "T1", Target: modtest.Other1M, ConfigHash: k.Holder.Current().Hash,
			Ban: []string{a, b}, BanEnforce: true, BanCommunity: a, Reason: "spam"}, k.Clock.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	k.Deliver(change(k, modtest.G1, modtest.Admin, []client.JID{modtest.Other1}, nil))
	k.Fire()
	if k.Fake.Count("Remove") != 0 {
		t.Fatalf("reversed a human admin's add: %v", k.Fake.Calls())
	}
	if k.Banned(modtest.Other1M, a) || !k.Banned(modtest.Other1M, b) {
		t.Fatalf("banned in %s: %v, in %s: %v; want only the ban in %s lifted", a, k.Banned(modtest.Other1M, a), b,
			k.Banned(modtest.Other1M, b), a)
	}
	if rows := k.Find(modtest.Other1M, store.ActUnban, modtest.Community); len(rows) != 1 || rows[0].Actor != string(modtest.Admin) {
		t.Fatalf("unban rows in %s: %+v", a, rows)
	}
}

// TestReaddedNextSpamRemovedAndRebanned: someone re-added by a human admin
// who then spams is deleted, removed and banned again, like anyone.
func TestReaddedNextSpamRemovedAndRebanned(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.Other1M)
	k.Deliver(change(k, modtest.G1, modtest.Admin, []client.JID{modtest.Other1}, nil))
	k.Deliver(k.Msg("M1", modtest.G1, modtest.Other1, modtest.SpamText))
	k.Fire()
	if k.Fake.Count("Revoke "+string(modtest.G1)+" "+string(modtest.Other1)+" M1") != 1 || removed(k, modtest.G1, modtest.Other1) != 1 {
		t.Fatalf("calls %v", k.Fake.Calls())
	}
	if !k.Banned(modtest.Other1M, string(modtest.Community)) {
		t.Fatal("not banned again")
	}
}

// TestUnknownActorNotHumanAdd: a banned person whose join names no actor is
// removed, with a priority report.
func TestUnknownActorNotHumanAdd(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.Other1M)
	k.Deliver(change(k, modtest.G1, "", []client.JID{modtest.Other1}, nil))
	k.Fire()
	if removed(k, modtest.G1, modtest.Other1) != 1 || !k.Banned(modtest.Other1M, "") {
		t.Fatalf("calls %v", k.Fake.Calls())
	}
	reps := k.Reports(ledger.KindUnknownActor)
	if len(reps) != 1 || !reps[0].Priority {
		t.Fatalf("reports %+v", reps)
	}
}

// TestHumanRemovalOffersBan: a human admin removing someone offers
// [Add to ban list] [No]; the bot's own removals and people leaving do not.
func TestHumanRemovalOffersBan(t *testing.T) {
	k := modtest.New(t, "")
	k.Deliver(change(k, modtest.G1, modtest.Admin, nil, []client.JID{modtest.Member}))
	k.Deliver(change(k, modtest.G2, modtest.Bot, nil, []client.JID{modtest.Spammer}))
	k.Deliver(change(k, modtest.GB, modtest.Member, nil, []client.JID{modtest.Member}))
	reps := k.Reports(ledger.KindHumanRemoval)
	if len(reps) != 1 || reps[0].Subject != string(modtest.Member) {
		t.Fatalf("reports %+v, want one about the member an admin removed", reps)
	}
	if !slices.Equal(reps[0].Buttons, []string{ledger.ButtonAddToBanList, ledger.ButtonNo}) {
		t.Fatalf("buttons %v", reps[0].Buttons)
	}
	if n := k.Logged("a human admin removed a member; offering a ban"); n != 1 {
		t.Fatalf("%d log lines for the offer, want 1", n)
	}
	if k.Banned(modtest.MemberM, "") {
		t.Fatal("banned without an admin pressing [Add to ban list]")
	}
}

// TestBanKeyIsLID: the ban list is keyed on the LID, with the phone number
// kept too, whichever address the message came from.
func TestBanKeyIsLID(t *testing.T) {
	k := modtest.New(t, "")
	m := k.Spam("M1", modtest.G1)
	m.Sender, m.SenderAlt = modtest.SpammerPhone, modtest.Spammer // a phone-addressed group
	k.Deliver(m)
	b, ok, err := k.Store.FindBan(k.Ctx, []string{string(modtest.SpammerPhone)}, "")
	if err != nil || !ok || b.Member != string(modtest.Spammer) || b.LID != string(modtest.Spammer) || b.Phone != string(modtest.SpammerPhone) {
		t.Fatalf("ban %+v (%v)", b, err)
	}
	// Only the phone number is in the message, but the directory knows the LID.
	k2 := modtest.New(t, "")
	m2 := k2.Spam("M1", modtest.G1)
	m2.Sender, m2.SenderAlt = modtest.SpammerPhone, ""
	k2.Deliver(m2)
	if b, ok, _ := k2.Store.FindBan(k2.Ctx, []string{string(modtest.Spammer)}, ""); !ok || b.Member != string(modtest.Spammer) {
		t.Fatalf("ban %+v", b)
	}
}

// TestPhoneToLIDFailureReported: a ban known only by phone number is
// resolved to a LID; when that fails the phone key stays and the admins get
// a priority report.
func TestPhoneToLIDFailureReported(t *testing.T) {
	k := modtest.New(t, "")
	unknown := client.Member{Phone: "447700900456@s.whatsapp.net"}
	k.Ban(unknown)
	known := client.Member{Phone: "447700900789@s.whatsapp.net"}
	k.Ban(known)
	k.Fake.LIDs = map[client.JID]client.JID{known.Phone: modtest.Other2}
	if err := k.Enforcer.ResolveBans(k.Ctx); err != nil {
		t.Fatal(err)
	}
	b, ok, _ := k.Store.FindBan(k.Ctx, unknown.IDs(), "")
	if !ok || b.Member != string(unknown.Phone) || b.LID != "" || !b.LIDFailed {
		t.Fatalf("failed lookup: %+v", b)
	}
	reps := k.Reports(ledger.KindLIDUnresolved)
	if len(reps) != 1 || !reps[0].Priority {
		t.Fatalf("reports %+v", reps)
	}
	if b, ok, _ := k.Store.FindBan(k.Ctx, []string{string(modtest.Other2)}, ""); !ok || b.Member != string(modtest.Other2) || b.Phone != string(known.Phone) {
		t.Fatalf("resolved ban %+v", b)
	}
	// A failed lookup is not retried on every sweep.
	if err := k.Enforcer.ResolveBans(k.Ctx); err != nil {
		t.Fatal(err)
	}
	if n := k.Fake.Count("ResolvePhoneToLID " + string(unknown.Phone)); n != 1 {
		t.Fatalf("looked up %d times", n)
	}
}

// TestLIDFailureReportedOncePerPhone: a phone number banned in two scopes is
// looked up once and reported once; a ban lifted while its lookup runs is not
// reported (there is nothing left to keep on the phone number).
func TestLIDFailureReportedOncePerPhone(t *testing.T) {
	k := modtest.New(t, "")
	phone := client.JID("447700900456@s.whatsapp.net")
	for _, scope := range []string{store.BanEverywhere, string(modtest.Community)} {
		if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
			return store.AddBan(k.Ctx, tx, store.Ban{Member: string(phone), Scope: scope, Phone: string(phone),
				Reason: "manual"}, k.Clock.Now())
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := k.Enforcer.ResolveBans(k.Ctx); err != nil {
		t.Fatal(err)
	}
	if n := k.Fake.Count("ResolvePhoneToLID " + string(phone)); n != 1 {
		t.Fatalf("looked up %d times, want once for both bans", n)
	}
	if n := len(k.Reports(ledger.KindLIDUnresolved)); n != 1 {
		t.Fatalf("%d reports, want one for the phone number", n)
	}

	lifted := client.Member{Phone: "447700900789@s.whatsapp.net"}
	k.Ban(lifted)
	k.Fake.OnCall = func(call string) {
		if call == "ResolvePhoneToLID "+string(lifted.Phone) {
			k.Unban(lifted)
		}
	}
	if err := k.Enforcer.ResolveBans(k.Ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(k.Reports(ledger.KindLIDUnresolved)); n != 1 {
		t.Fatalf("%d reports after a ban lifted during its lookup, want still 1", n)
	}
}

// TestMetaAIWatchOnlyMatchExempt: Meta AI matching a watch-only rule ("lure")
// is reported as exempt, with no action rows and no [Ban] button, whether it
// posts from the bot server or from an address the ban list could hold; the
// spam behind it is still actioned.
func TestMetaAIWatchOnlyMatchExempt(t *testing.T) {
	k := modtest.New(t, "")
	onBot := client.JID(strings.TrimSuffix(string(modtest.Other1), "@lid") + "@bot")
	for i, sender := range []client.JID{onBot, modtest.Other2} {
		m := k.Msg(fmt.Sprintf("AI%d", i), modtest.G1, sender, "crypto tips, inbox me")
		m.FromMetaAI = true
		k.Deliver(m)
	}
	reps := k.Reports(ledger.KindExempt)
	if len(reps) != 2 || len(k.Reports(ledger.KindWouldHaveActed)) != 0 {
		t.Fatalf("exempt reports %+v, would-have-acted %d", reps, len(k.Reports(ledger.KindWouldHaveActed)))
	}
	for _, r := range reps {
		if len(r.Buttons) != 0 || !strings.Contains(r.Text, "Meta AI") {
			t.Fatalf("report %+v", r)
		}
	}
	if rows := k.Rows(modtest.Other2M); len(rows) != 0 {
		t.Fatalf("Meta AI got action rows %+v", rows)
	}
	k.Deliver(k.Spam("S1", modtest.G1))
	k.Fire()
	if !k.Banned(modtest.SpammerM, "") {
		t.Fatal("the spam behind Meta AI's post was not actioned")
	}
}

// TestUnaddressableSenderPostDeleted: spam from a sender whose address is
// neither a LID nor a phone number (a server the ban list cannot hold) is
// deleted, as every spam post is; nobody is removed or banned, and a
// priority report tells the admins to remove the sender by hand. The spam
// behind it is still decided and actioned.
func TestUnaddressableSenderPostDeleted(t *testing.T) {
	k := modtest.New(t, "")
	hosted := client.JID(strings.TrimSuffix(string(modtest.Spammer), "@lid") + "@hosted")
	k.Deliver(k.Msg("HOST1", modtest.G1, hosted, modtest.SpamText))
	k.Fire()
	if n := k.Fake.Count("Revoke " + string(modtest.G1) + " " + string(hosted) + " HOST1"); n != 1 {
		t.Fatalf("revoked the unaddressable post %d times, want 1: calls %v", n, k.Fake.Calls())
	}
	if k.Fake.Count("Remove") != 0 || k.Banned(client.Member{LID: hosted}, "") {
		t.Fatalf("removed or banned an unaddressable sender: calls %v", k.Fake.Calls())
	}
	reps := k.Reports(ledger.KindUnaddressable)
	if len(reps) != 1 || !reps[0].Priority || !strings.Contains(reps[0].Text, ": the post is deleted, but the sender's "+
		"address (") || !strings.HasSuffix(reps[0].Text, "is not one the bot can remove or ban. Remove them by hand.") {
		t.Fatalf("reports %+v, want one priority report that the post is deleted", reps)
	}
	if strings.Contains(reps[0].Text, strings.TrimSuffix(string(modtest.Spammer), "@lid")) {
		t.Fatalf("report names the sender unmasked: %q", reps[0].Text)
	}
	if r, _, _ := k.Store.Report(k.Ctx, reps[0].ID); r.Action != "deleted for everyone; the sender cannot be removed or banned" {
		t.Fatalf("action line %q", r.Action)
	}
	if n := k.Logged("rule matched a sender the bot cannot remove or ban"); n != 1 {
		t.Fatalf("%d log lines for the unaddressable sender, want 1", n)
	}
	// A human admin removing that address is not offered for the ban list:
	// there is nobody the list could hold.
	k.Deliver(change(k, modtest.G1, modtest.Admin, nil, []client.JID{hosted}))
	if reps := k.Reports(ledger.KindHumanRemoval); len(reps) != 0 {
		t.Fatalf("offered a ban of an unaddressable member: %+v", reps)
	}
	if n := k.Logged("a human admin removed an address the bot cannot ban; not offered"); n != 1 {
		t.Fatalf("%d log lines for the removal not offered, want 1", n)
	}
	k.Deliver(k.Spam("S1", modtest.G1))
	k.Fire()
	if !k.Banned(modtest.SpammerM, "") || removed(k, modtest.G1, modtest.Spammer) != 1 {
		t.Fatalf("the spam behind it was not actioned: calls %v", k.Fake.Calls())
	}
}

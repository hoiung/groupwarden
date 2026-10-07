package pipeline

import (
	"errors"
	"testing"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/rules"
)

// TestDirectoryCommunityAndAdmins: a group resolves to its configured
// community (by parent or standalone set), and a member counts as an admin
// only when they administer a moderated group or community, under either of
// their addresses.
func TestDirectoryCommunityAndAdmins(t *testing.T) {
	const (
		community = client.JID("99999000000999@g.us")
		linked    = client.JID("99999000000111@g.us")
		standalon = client.JID("99999000000888@g.us")
		elsewhere = client.JID("99999000000222@g.us")
		otherComm = client.JID("99999000000777@g.us")
		adminLID  = client.JID("99999000000444@lid")
		adminTel  = client.JID("447700900123@s.whatsapp.net")
		outsider  = client.JID("99999000000555@lid")
		owner     = client.JID("99999000000666@lid")
		member    = client.JID("99999000000777@lid")
	)
	rs, err := rules.Compile(rules.Spec{Mode: rules.Shadow, BanScope: rules.AllCommunities, MinWordLength: 3,
		Communities: []rules.CommunitySpec{{ID: string(community)}, {ID: "set", Groups: []string{string(standalon)}}}})
	if err != nil {
		t.Fatal(err)
	}
	d := &Directory{}
	load(t, d, []client.Group{
		{JID: community, IsCommunity: true, Participants: []client.Participant{{JID: owner, IsSuperAdmin: true}}},
		{JID: linked, Parent: community, Participants: []client.Participant{
			{JID: adminLID, Phone: adminTel, LID: adminLID, IsAdmin: true}, {JID: member}}},
		{JID: standalon},
		{JID: elsewhere, Parent: otherComm, Participants: []client.Participant{{JID: outsider, IsAdmin: true}}},
	})
	for g, want := range map[client.JID]string{linked: string(community), standalon: "set", elsewhere: "", "99999000000333@g.us": ""} {
		if got := d.Community(g, rs); got != want {
			t.Errorf("Community(%s) = %q, want %q", g, got, want)
		}
	}
	cases := []struct {
		name        string
		member, alt client.JID
		want        bool
	}{
		{"admin by LID", adminLID, "", true},
		{"admin by phone", adminTel, "", true},
		{"admin by the alternate address", "99999000000888@lid", adminTel, true},
		{"community owner", owner, "", true},
		{"admin of an unmoderated group only", outsider, "", false},
		{"plain member", member, "", false},
	}
	for _, c := range cases {
		if got := d.IsAdmin(c.member, c.alt, rs); got != c.want {
			t.Errorf("%s: IsAdmin = %v, want %v", c.name, got, c.want)
		}
	}
	load(t, d, nil)
	// A refresh replaces everything: with no entries, the admin and the
	// linked group's parent are both forgotten.
	if d.IsAdmin(adminLID, "", rs) || d.Community(linked, rs) != "" {
		t.Error("the refresh did not replace the directory")
	}
}

// load fills d from groups, as a group list read from WhatsApp does.
func load(t *testing.T, d *Directory, groups []client.Group) {
	t.Helper()
	if err := d.Refresh(func() ([]client.Group, error) { return groups, nil }); err != nil {
		t.Fatal(err)
	}
}

// TestRefreshKeepsChangesMadeDuringTheFetch: WhatsApp's group list can be
// built before changes the worker applies while it is on its way. Those
// changes (a promotion, a demotion, a join, a member leaving, the bot found
// not to be an admin) are applied again on top of the list, so the older
// list does not undo them; the list itself still replaces everything else.
func TestRefreshKeepsChangesMadeDuringTheFetch(t *testing.T) {
	const (
		community = client.JID("99999000000999@g.us")
		g1        = client.JID("99999000000111@g.us")
		joined    = client.JID("99999000000222@g.us")
		gone      = client.JID("99999000000333@g.us")
		promoted  = client.JID("99999000000444@lid")
		demoted   = client.JID("99999000000555@lid")
		leaver    = client.JID("99999000000666@lid")
		listedOld = client.JID("99999000000777@lid") // in the old directory only
		bot       = client.JID("99999000000888@lid")
	)
	rs, err := rules.Compile(rules.Spec{Mode: rules.Shadow, BanScope: rules.AllCommunities, MinWordLength: 3,
		Communities: []rules.CommunitySpec{{ID: string(community)}}})
	if err != nil {
		t.Fatal(err)
	}
	d := &Directory{}
	d.SetSelf(client.Self{LID: bot})
	load(t, d, []client.Group{{JID: gone, Parent: community, Participants: []client.Participant{{JID: listedOld}}}})
	// The list as WhatsApp built it, before any of the changes below.
	list := []client.Group{
		{JID: community, IsCommunity: true},
		{JID: g1, Parent: community, Participants: []client.Participant{
			{JID: bot, IsAdmin: true}, {JID: promoted}, {JID: demoted, IsAdmin: true}, {JID: leaver}}},
	}
	err = d.Refresh(func() ([]client.Group, error) {
		d.Apply(&client.GroupChange{Group: g1, Promoted: []client.JID{promoted}, Demoted: []client.JID{demoted},
			Left: []client.JID{leaver}})
		d.Join(client.Group{JID: joined, Parent: community})
		d.BotNotAdmin(g1)
		return list, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		what      string
		got, want bool
	}{
		{"a promotion made during the fetch", d.IsAdmin(promoted, "", rs), true},
		{"a demotion made during the fetch", d.IsAdmin(demoted, "", rs), false},
		{"a member who left during the fetch", d.Present(g1, client.MemberOf(leaver)), false},
		{"the bot found not to be an admin during the fetch", d.BotIsAdmin(g1), false},
		{"a group joined during the fetch", d.Community(joined, rs) == string(community), true},
		{"a group the new list no longer has", d.Known(gone), false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.what, c.got, c.want)
		}
	}
	// Once no refresh is in flight, changes are not kept for later: the
	// next list is the truth.
	d.Apply(&client.GroupChange{Group: g1, Promoted: []client.JID{leaver}})
	load(t, d, list)
	if d.IsAdmin(leaver, "", rs) || !d.IsAdmin(demoted, "", rs) {
		t.Error("a change made outside a refresh was applied again on top of the next list")
	}
	if len(d.journal) != 0 {
		t.Errorf("journal holds %d changes with no refresh in flight", len(d.journal))
	}
}

// TestFailedRefreshKeepsTheDirectory: a group list that cannot be read
// leaves the directory as it was (and keeps nothing for later).
func TestFailedRefreshKeepsTheDirectory(t *testing.T) {
	const (
		g1     = client.JID("99999000000111@g.us")
		member = client.JID("99999000000444@lid")
	)
	d := &Directory{}
	load(t, d, []client.Group{{JID: g1, Participants: []client.Participant{{JID: member}}}})
	failure := errors.New("rate limited")
	err := d.Refresh(func() ([]client.Group, error) {
		d.Apply(&client.GroupChange{Group: g1, Joined: []client.JID{"99999000000555@lid"}})
		return nil, failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("Refresh = %v, want the fetch's error", err)
	}
	if !d.Present(g1, client.MemberOf(member)) || !d.Present(g1, client.MemberOf("99999000000555@lid")) {
		t.Error("a failed refresh changed the directory")
	}
	if len(d.journal) != 0 || d.refreshing != 0 {
		t.Errorf("after a failed refresh: journal %d, refreshing %d", len(d.journal), d.refreshing)
	}
}

// TestJoinLearnsTheGroup: the bot joining a group records it with its
// community, members and admins from the join notification; a group already
// listed is left as the list has it.
func TestJoinLearnsTheGroup(t *testing.T) {
	const (
		community = client.JID("99999000000999@g.us")
		g1        = client.JID("99999000000111@g.us")
		g2        = client.JID("99999000000222@g.us")
		admin     = client.JID("99999000000444@lid")
		member    = client.JID("99999000000555@lid")
	)
	rs, err := rules.Compile(rules.Spec{Mode: rules.Shadow, BanScope: rules.AllCommunities, MinWordLength: 3,
		Communities: []rules.CommunitySpec{{ID: string(community)}}})
	if err != nil {
		t.Fatal(err)
	}
	d := &Directory{}
	load(t, d, []client.Group{{JID: g2, Parent: community, Participants: []client.Participant{{JID: member}}}})
	d.Join(client.Group{JID: g1, Parent: community, Participants: []client.Participant{{JID: admin, IsAdmin: true},
		{JID: member}}})
	if d.Community(g1, rs) != string(community) || !d.IsAdmin(admin, "", rs) || !d.Present(g1, client.MemberOf(member)) {
		t.Errorf("joined group: community %q, admin %v, member %v", d.Community(g1, rs), d.IsAdmin(admin, "", rs),
			d.Present(g1, client.MemberOf(member)))
	}
	d.Join(client.Group{JID: g2})
	if d.Community(g2, rs) != string(community) || !d.Present(g2, client.MemberOf(member)) {
		t.Error("a join replaced a group the list already had")
	}
}

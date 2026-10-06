package pipeline

import (
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
	d.Update([]client.Group{
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
	d.Update(nil)
	// Update replaces everything: with no entries, the admin and the linked
	// group's parent are both forgotten.
	if d.IsAdmin(adminLID, "", rs) || d.Community(linked, rs) != "" {
		t.Error("Update did not replace the directory")
	}
}

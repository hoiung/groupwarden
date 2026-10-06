package pipeline

import (
	"sort"
	"sync"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/rules"
)

// Directory is what the bot last learned about its groups from WhatsApp:
// each group's parent community, its members and its admins. The app
// refreshes it at connect and on every sweep (network calls happen there,
// never while a message is decided); membership and admin events keep it
// current in between. Decisions only read it.
type Directory struct {
	mu     sync.RWMutex
	groups map[client.JID]*dirGroup
	self   client.Member
}

type dirGroup struct {
	parent       client.JID
	community    bool
	announcement bool
	admins       map[client.JID]bool // every address of every admin
	members      map[client.JID]bool // every address of every member
	alt          map[client.JID]client.JID
}

func newDirGroup(g client.Group) *dirGroup {
	return &dirGroup{parent: g.Parent, community: g.IsCommunity, announcement: g.IsAnnouncement,
		admins: map[client.JID]bool{}, members: map[client.JID]bool{}, alt: map[client.JID]client.JID{}}
}

// Update replaces the directory with groups (as JoinedGroups lists them).
func (d *Directory) Update(groups []client.Group) {
	next := make(map[client.JID]*dirGroup, len(groups))
	for _, g := range groups {
		dg := newDirGroup(g)
		for _, p := range g.Participants {
			addrs := []client.JID{p.JID.Bare(), p.Phone.Bare(), p.LID.Bare()}
			for _, j := range addrs {
				if j == "" {
					continue
				}
				dg.members[j] = true
				if p.IsAdmin || p.IsSuperAdmin {
					dg.admins[j] = true
				}
			}
			if p.Phone != "" && p.LID != "" {
				dg.alt[p.Phone.Bare()], dg.alt[p.LID.Bare()] = p.LID.Bare(), p.Phone.Bare()
			}
		}
		next[g.JID] = dg
	}
	d.mu.Lock()
	d.groups = next
	d.mu.Unlock()
}

// SetSelf records the bot's own addresses.
func (d *Directory) SetSelf(self client.Self) {
	d.mu.Lock()
	d.self = client.MemberOf(self.Phone, self.LID)
	d.mu.Unlock()
}

// IsSelf reports whether j is the bot.
func (d *Directory) IsSelf(j client.JID) bool {
	if j == "" {
		return false
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	b := j.Bare()
	return b == d.self.LID || b == d.self.Phone
}

// Community returns the configured community group belongs to under rs
// ("" = not moderated).
func (d *Directory) Community(group client.JID, rs *rules.Ruleset) string {
	d.mu.RLock()
	var parent client.JID
	if g := d.groups[group]; g != nil {
		parent = g.parent
		if g.community {
			parent = group
		}
	}
	d.mu.RUnlock()
	return rs.CommunityOf(string(group), string(parent))
}

// moderatedCommunity reports which configured community a directory entry
// counts towards (a community parent counts as its own community).
func moderatedCommunity(jid client.JID, g *dirGroup, rs *rules.Ruleset) string {
	if g.community {
		return rs.CommunityOf(string(jid), string(jid))
	}
	return rs.CommunityOf(string(jid), string(g.parent))
}

// IsAdmin reports whether a member (by either address) is a current admin
// of any moderated group or community under rs.
func (d *Directory) IsAdmin(member, alt client.JID, rs *rules.Ruleset) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	m, a := member.Bare(), alt.Bare()
	for jid, g := range d.groups {
		if !(g.admins[m] || (a != "" && g.admins[a])) {
			continue
		}
		if moderatedCommunity(jid, g, rs) != "" {
			return true
		}
	}
	return false
}

// Complete fills in the address the directory knows for a member given by
// one address.
func (d *Directory) Complete(m client.Member) client.Member {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, g := range d.groups {
		if m.LID != "" && m.Phone == "" {
			if p, ok := g.alt[m.LID]; ok {
				m.Phone = p
			}
		}
		if m.Phone != "" && m.LID == "" {
			if l, ok := g.alt[m.Phone]; ok {
				m.LID = l
			}
		}
	}
	return m
}

// Present reports whether the member is in group, by either address.
func (d *Directory) Present(group client.JID, m client.Member) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	g := d.groups[group]
	if g == nil {
		return false
	}
	for _, id := range m.IDs() {
		if g.members[client.JID(id)] {
			return true
		}
	}
	return false
}

// AddressIn returns the address group uses for the member (the one WhatsApp
// expects in a remove call), or the member's key when the group is unknown.
func (d *Directory) AddressIn(group client.JID, m client.Member) client.JID {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if g := d.groups[group]; g != nil {
		for _, id := range m.IDs() {
			if g.members[client.JID(id)] {
				return client.JID(id)
			}
		}
	}
	return client.JID(m.Key())
}

// BotIsAdmin reports whether the bot is an admin of group (or community).
func (d *Directory) BotIsAdmin(group client.JID) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	g := d.groups[group]
	return g != nil && ((d.self.LID != "" && g.admins[d.self.LID]) || (d.self.Phone != "" && g.admins[d.self.Phone]))
}

// IsCommunity reports whether jid is a community the bot knows.
func (d *Directory) IsCommunity(jid client.JID) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	g := d.groups[jid]
	return g != nil && g.community
}

// Groups lists the chat groups (not community parents) that belong to the
// configured community, sorted.
func (d *Directory) Groups(community string, rs *rules.Ruleset) []client.JID {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []client.JID
	for jid, g := range d.groups {
		if !g.community && moderatedCommunity(jid, g, rs) == community {
			out = append(out, jid)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Moderated lists every moderated chat group, sorted.
func (d *Directory) Moderated(rs *rules.Ruleset) []client.JID {
	var out []client.JID
	for _, c := range rs.Communities() {
		out = append(out, d.Groups(c, rs)...)
	}
	return out
}

// Members lists every member of group, each by both addresses where known.
func (d *Directory) Members(group client.JID) []client.Member {
	d.mu.RLock()
	defer d.mu.RUnlock()
	g := d.groups[group]
	if g == nil {
		return nil
	}
	seen := map[client.JID]bool{}
	var out []client.Member
	for j := range g.members {
		if seen[j] {
			continue
		}
		m := client.MemberOf(j, g.alt[j])
		for _, id := range m.IDs() {
			seen[client.JID(id)] = true
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// AnnouncementGroups lists the announcement groups the bot is in.
func (d *Directory) AnnouncementGroups() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []string
	for jid, g := range d.groups {
		if g.announcement {
			out = append(out, string(jid))
		}
	}
	sort.Strings(out)
	return out
}

// Apply updates group membership and admin status from a change event.
func (d *Directory) Apply(ch *client.GroupChange) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.groups == nil {
		d.groups = map[client.JID]*dirGroup{}
	}
	g := d.groups[ch.Group]
	if g == nil {
		g = newDirGroup(client.Group{JID: ch.Group})
		d.groups[ch.Group] = g
	}
	both := func(j client.JID) []client.JID {
		b := j.Bare()
		if a, ok := g.alt[b]; ok {
			return []client.JID{b, a}
		}
		return []client.JID{b}
	}
	for _, j := range ch.Joined {
		for _, a := range both(j) {
			g.members[a] = true
		}
	}
	for _, j := range ch.Left {
		for _, a := range both(j) {
			delete(g.members, a)
			delete(g.admins, a)
		}
	}
	for _, j := range ch.Promoted {
		for _, a := range both(j) {
			g.admins[a] = true
		}
	}
	for _, j := range ch.Demoted {
		for _, a := range both(j) {
			delete(g.admins, a)
		}
	}
}

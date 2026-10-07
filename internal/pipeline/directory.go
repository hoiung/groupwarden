package pipeline

import (
	"sort"
	"sync"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/rules"
)

// Label names a group for the admins: its name and masked ID (the masked ID
// alone when the name is unknown).
func Label(name string, group client.JID) string {
	if name == "" {
		return mask.IDs(string(group))
	}
	return name + " (" + mask.IDs(string(group)) + ")"
}

// CommunityLabel names a configured community for the admins: its configured
// name, else its masked ID.
func CommunityLabel(c *config.Config, id string) string {
	if cm, ok := c.Communities[id]; ok && cm.Name != "" {
		return cm.Name
	}
	return mask.IDs(id)
}

// Directory is what the bot last learned about its groups from WhatsApp:
// each group's parent community, its members and its admins. The app
// refreshes it at connect and on every sweep (network calls happen there,
// never while a message is decided); membership, admin and join events keep
// it current in between. Decisions only read it.
type Directory struct {
	mu     sync.RWMutex
	groups map[client.JID]*dirGroup
	self   client.Member
	// refreshing counts refreshes waiting on WhatsApp's group list; while
	// one is, every change is also kept in journal, to apply again on top
	// of the list (which may predate it).
	refreshing int
	journal    []change

	initOnce, loadOnce sync.Once
	loaded             chan struct{}
}

// change is one update to the directory's groups, applied with d.mu held.
type change func(groups map[client.JID]*dirGroup)

// apply makes c, keeping it for any refresh in flight. d.mu must be held.
func (d *Directory) apply(c change) {
	if d.groups == nil {
		d.groups = map[client.JID]*dirGroup{}
	}
	c(d.groups)
	if d.refreshing > 0 {
		d.journal = append(d.journal, c)
	}
}

func (d *Directory) loadedChan() chan struct{} {
	d.initOnce.Do(func() { d.loaded = make(chan struct{}) })
	return d.loaded
}

// Loaded is closed once the directory has been filled from WhatsApp's group
// list for the first time: before that no message can be matched to its
// community.
func (d *Directory) Loaded() <-chan struct{} { return d.loadedChan() }

// IsLoaded reports whether Loaded is closed.
func (d *Directory) IsLoaded() bool {
	select {
	case <-d.Loaded():
		return true
	default:
		return false
	}
}

type dirGroup struct {
	name         string
	parent       client.JID
	community    bool
	announcement bool
	admins       map[client.JID]bool // every address of every admin
	members      map[client.JID]bool // every address of every member
	alt          map[client.JID]client.JID
}

func newDirGroup(g client.Group) *dirGroup {
	return &dirGroup{name: g.Name, parent: g.Parent, community: g.IsCommunity, announcement: g.IsAnnouncement,
		admins: map[client.JID]bool{}, members: map[client.JID]bool{}, alt: map[client.JID]client.JID{}}
}

// GroupName is a group's (or community's) name as WhatsApp last gave it
// ("" when unknown).
func (d *Directory) GroupName(jid string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if g := d.groups[client.JID(jid)]; g != nil {
		return g.name
	}
	return ""
}

// Label names group for the admins (see Label).
func (d *Directory) Label(group client.JID) string { return Label(d.GroupName(string(group)), group) }

// Known reports whether the bot is in group (as last listed or changed).
func (d *Directory) Known(group client.JID) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.groups[group] != nil
}

// HumanAdmins counts the admins of group other than the bot, one per person
// (an admin known by both addresses counts once).
func (d *Directory) HumanAdmins(group client.JID) int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	g := d.groups[group]
	if g == nil {
		return 0
	}
	seen := map[client.JID]bool{}
	n := 0
	for j := range g.admins {
		if seen[j] || j == d.self.LID || j == d.self.Phone {
			continue
		}
		seen[j] = true
		if a, ok := g.alt[j]; ok {
			if a == d.self.LID || a == d.self.Phone {
				continue
			}
			seen[a] = true
		}
		n++
	}
	return n
}

// listed builds a directory entry from the group as WhatsApp describes it.
func listed(g client.Group) *dirGroup {
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
	return dg
}

// Refresh replaces the directory with the groups fetch lists (WhatsApp's
// group list). fetch runs without the lock, and the list it returns may
// have been built before changes applied meanwhile (a promotion the worker
// decided while the list was on its way): those are applied again on top
// of it, so the older list cannot undo them. On an error the directory is
// left as it was.
func (d *Directory) Refresh(fetch func() ([]client.Group, error)) error {
	d.mu.Lock()
	d.refreshing++
	d.mu.Unlock()
	groups, err := fetch()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.refreshing--
	replay := d.journal
	if d.refreshing == 0 {
		d.journal = nil
	}
	if err != nil {
		return err
	}
	next := make(map[client.JID]*dirGroup, len(groups))
	for _, g := range groups {
		next[g.JID] = listed(g)
	}
	for _, c := range replay {
		c(next)
	}
	d.groups = next
	d.loadOnce.Do(func() { close(d.loadedChan()) })
	return nil
}

// Join records a group the bot joined, as the join notification described
// it, so its community is known without waiting for the next group list.
// A group the directory already lists is left as it is: a list read after
// the join is at least as new as the notification.
func (d *Directory) Join(g client.Group) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.apply(func(groups map[client.JID]*dirGroup) {
		if groups[g.JID] == nil {
			groups[g.JID] = listed(g)
		}
	})
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

// BotNotAdmin records that the bot is no longer an admin of group (WhatsApp
// refused it an admin-only call there). The next refresh relearns it.
func (d *Directory) BotNotAdmin(group client.JID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.apply(func(groups map[client.JID]*dirGroup) {
		if g := groups[group]; g != nil {
			for _, j := range []client.JID{d.self.LID, d.self.Phone} {
				if j != "" {
					delete(g.admins, j)
				}
			}
		}
	})
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
	d.apply(func(groups map[client.JID]*dirGroup) { d.applyChange(groups, ch) })
}

// applyChange is Apply on groups. d.mu must be held.
func (d *Directory) applyChange(groups map[client.JID]*dirGroup, ch *client.GroupChange) {
	g := groups[ch.Group]
	if g == nil {
		g = newDirGroup(client.Group{JID: ch.Group})
		groups[ch.Group] = g
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
	// The bot itself left: it is no longer in the group at all.
	for _, j := range ch.Left {
		if b := j.Bare(); b != "" && (b == d.self.LID || b == d.self.Phone) {
			delete(groups, ch.Group)
			return
		}
	}
}

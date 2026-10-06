package pipeline

import (
	"sync"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/rules"
)

// Directory is what the bot last learned about its groups from WhatsApp:
// each group's parent community and its admins. The app refreshes it at
// connect and on every sweep (network calls happen there, never while a
// message is decided); decisions only read it.
type Directory struct {
	mu     sync.RWMutex
	groups map[client.JID]dirGroup
}

type dirGroup struct {
	parent    client.JID
	community bool
	admins    map[client.JID]bool // every address of every admin
}

// Update replaces the directory with groups (as JoinedGroups lists them).
func (d *Directory) Update(groups []client.Group) {
	next := make(map[client.JID]dirGroup, len(groups))
	for _, g := range groups {
		dg := dirGroup{parent: g.Parent, community: g.IsCommunity, admins: map[client.JID]bool{}}
		for _, p := range g.Participants {
			if !p.IsAdmin && !p.IsSuperAdmin {
				continue
			}
			for _, j := range []client.JID{p.JID, p.Phone, p.LID} {
				if j != "" {
					dg.admins[j] = true
				}
			}
		}
		next[g.JID] = dg
	}
	d.mu.Lock()
	d.groups = next
	d.mu.Unlock()
}

// Community returns the configured community group belongs to under rs
// ("" = not moderated).
func (d *Directory) Community(group client.JID, rs *rules.Ruleset) string {
	d.mu.RLock()
	g := d.groups[group]
	d.mu.RUnlock()
	return rs.CommunityOf(string(group), string(g.parent))
}

// moderatedCommunity reports which configured community a directory entry
// counts towards (a community parent counts as its own community).
func moderatedCommunity(jid client.JID, g dirGroup, rs *rules.Ruleset) string {
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
	for jid, g := range d.groups {
		if !(g.admins[member] || (alt != "" && g.admins[alt])) {
			continue
		}
		if moderatedCommunity(jid, g, rs) != "" {
			return true
		}
	}
	return false
}

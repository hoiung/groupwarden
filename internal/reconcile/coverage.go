package reconcile

import (
	"context"
	"sort"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/mask"
)

// Where the bot stands in a group of a configured community.
const (
	StateAbsent   = "absent"    // the bot is not in the group
	StateNotAdmin = "not_admin" // in it, but not an admin: it cannot act there
	StateCovered  = "covered"   // an admin: moderated
)

// GroupCoverage is one group of a configured community.
type GroupCoverage struct {
	JID          client.JID
	Name         string
	Announcement bool
	State        string
	HumanAdmins  int
}

// CommunityCoverage is every group of one configured community. Err is set
// when WhatsApp could not list them.
type CommunityCoverage struct {
	Community string
	Groups    []GroupCoverage
	Err       string
}

// Coverage is the last discovery, at At.
type Coverage struct {
	At          time.Time
	Communities []CommunityCoverage
}

// discover lists, for each configured community, every group it has (a
// WhatsApp Community's linked groups and announcement group, or the listed
// groups of a standalone set) and where the bot stands in each.
func (s *Sweep) discover(ctx context.Context, res *Result) Coverage {
	rs := s.Config.Current().Rules
	cov := Coverage{At: s.now()}
	for _, c := range rs.Communities() {
		cc := CommunityCoverage{Community: c}
		var refs []client.GroupRef
		if set := rs.SetGroups(c); len(set) > 0 {
			for _, g := range set {
				refs = append(refs, client.GroupRef{JID: client.JID(g), Name: s.Directory.GroupName(g)})
			}
		} else if err := s.call(ctx, res, func() error {
			var err error
			refs, err = s.Enforcer.Adapter.SubGroups(ctx, client.JID(c))
			return err
		}); err != nil {
			cc.Err = mask.IDs(err.Error())
			s.Log.Error("sweep: could not list a community's groups", "community", mask.IDs(c), "err", cc.Err)
		}
		for _, r := range refs {
			g := GroupCoverage{JID: r.JID, Name: r.Name, Announcement: r.IsAnnouncement, State: StateAbsent}
			if s.Directory.Known(r.JID) {
				g.State, g.HumanAdmins = StateNotAdmin, s.Directory.HumanAdmins(r.JID)
				if s.Directory.BotIsAdmin(r.JID) {
					g.State = StateCovered
				}
			}
			cc.Groups = append(cc.Groups, g)
		}
		sort.Slice(cc.Groups, func(i, j int) bool { return cc.Groups[i].JID < cc.Groups[j].JID })
		cov.Communities = append(cov.Communities, cc)
	}
	return cov
}

// Coverage is the coverage found by the last sweep (zero before the first).
func (s *Sweep) Coverage() Coverage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.coverage
}

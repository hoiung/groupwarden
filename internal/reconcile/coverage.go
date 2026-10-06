package reconcile

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
)

// Coverage of one group of a configured community. The store's coverage table
// keeps, per group, what the admins were last told; every sweep compares it
// with what the bot finds and reports only the changes.
//
//	(no row) ──sweep──► absent | not_admin | covered   listed: named once to the admins
//	                                                   (the community's first list, or "new group")
//	absent ──added by a human, or joined by itself──► not_admin ──promoted──► covered
//	covered ──demote event | permission refusal | refresh shows no admin──► not_admin   priority, once
//	covered ──removal event | refresh shows the bot gone──► absent                      priority, once
//	not_admin|absent ──► covered, and not_admin ──► absent: logged only
//
// join_tried is set once the bot tried to join a group by itself or has been
// in it: it never joins that group by itself again (a removal is a human's
// decision). few_admins is set while the bot is in a group with fewer than 2
// human admins: one routine alert per episode.

// minHumanAdmins: fewer human admins than this in a group draws an alert
// (if the bot's number is banned, the humans must still run the group).
const minHumanAdmins = 2

// GroupCoverage is one group of a configured community.
type GroupCoverage struct {
	JID          client.JID
	Name         string
	Announcement bool
	State        string // store.CoverageAbsent / CoverageNotAdmin / CoverageCovered
	HumanAdmins  int
}

// CommunityCoverage is every group of one configured community. Err is set
// when WhatsApp could not list them.
type CommunityCoverage struct {
	Community string
	Groups    []GroupCoverage
	Err       string
}

// Coverage is every configured community's groups as last listed (at At),
// each with where the bot stands in it now.
type Coverage struct {
	At          time.Time
	Communities []CommunityCoverage
}

// listing is one community's groups as WhatsApp (or the config, for a
// standalone set) listed them at the last sweep.
type listing struct {
	community string
	refs      []client.GroupRef
	set       bool // a standalone set: there is no community to join through
	err       string
}

// discover lists, for each configured community, every group it has: a
// WhatsApp Community's linked groups and announcement group, or the listed
// groups of a standalone set.
func (s *Sweep) discover(ctx context.Context, res *Result) []listing {
	rs := s.Config.Current().Rules
	var out []listing
	for _, c := range rs.Communities() {
		l := listing{community: c}
		if set := rs.SetGroups(c); len(set) > 0 {
			l.set = true
			for _, g := range set {
				l.refs = append(l.refs, client.GroupRef{JID: client.JID(g)})
			}
		} else if err := s.call(ctx, res, func() error {
			var err error
			l.refs, err = s.Enforcer.Adapter.SubGroups(ctx, client.JID(c))
			return err
		}); err != nil {
			l.err = mask.IDs(err.Error())
			s.Log.Error("sweep: could not list a community's groups", "community", mask.IDs(c), "err", l.err)
		}
		sort.Slice(l.refs, func(i, j int) bool { return l.refs[i].JID < l.refs[j].JID })
		out = append(out, l)
	}
	return out
}

// Coverage is the groups the last sweep listed (zero before the first), each
// with where the bot stands in it now: a demotion or a refused call shows at
// once, not at the next sweep.
func (s *Sweep) Coverage() Coverage {
	s.mu.Lock()
	at, lists := s.at, s.lists
	s.mu.Unlock()
	cov := Coverage{At: at}
	for _, l := range lists {
		cc := CommunityCoverage{Community: l.community, Err: l.err}
		for _, r := range l.refs {
			cc.Groups = append(cc.Groups, s.groupCoverage(r))
		}
		cov.Communities = append(cov.Communities, cc)
	}
	return cov
}

// groupCoverage is where the bot stands in r, from the directory.
func (s *Sweep) groupCoverage(r client.GroupRef) GroupCoverage {
	g := GroupCoverage{JID: r.JID, Name: r.Name, Announcement: r.IsAnnouncement, State: store.CoverageAbsent}
	if g.Name == "" {
		g.Name = s.Directory.GroupName(string(r.JID))
	}
	if s.Directory.Known(r.JID) {
		g.State, g.HumanAdmins = store.CoverageNotAdmin, s.Directory.HumanAdmins(r.JID)
		if s.Directory.BotIsAdmin(r.JID) {
			g.State = store.CoverageCovered
		}
	}
	return g
}

// joinResult is how a join the bot tried by itself went.
type joinResult struct {
	joined, pending bool
	err             string
}

// autoJoin asks WhatsApp to join, by itself, every linked group of a
// community that the bot is not in and has never tried to join or been in.
// WhatsApp joins it at once or holds it for an admin's approval; a refusal
// leaves a /join link as the way in.
func (s *Sweep) autoJoin(ctx context.Context, res *Result, lists []listing, prev map[string]store.CoverageRow) map[client.JID]joinResult {
	out := map[client.JID]joinResult{}
	for _, l := range lists {
		if l.set || l.err != "" {
			continue
		}
		for _, r := range l.refs {
			if s.Directory.Known(r.JID) || prev[string(r.JID)].JoinTried {
				continue
			}
			if ctx.Err() != nil {
				return out
			}
			var jr joinResult
			err := s.call(ctx, res, func() error {
				var err error
				jr.joined, jr.pending, err = s.Enforcer.Adapter.JoinLinkedGroup(ctx, client.JID(l.community), r.JID)
				return err
			})
			res.Joins++
			if err != nil {
				jr.err = mask.IDs(err.Error())
				s.Log.Warn("sweep: could not join a linked group by itself", "community", mask.IDs(l.community),
					"group", mask.IDs(string(r.JID)), "err", jr.err)
			} else {
				s.Log.Info("sweep: joined a linked group by itself", "community", mask.IDs(l.community),
					"group", mask.IDs(string(r.JID)), "joined", jr.joined, "pending", jr.pending)
			}
			out[r.JID] = jr
		}
	}
	return out
}

// todo says where the bot stands in a group and what an admin must do.
func todo(state string, jr joinResult, tried bool) string {
	switch {
	case state == store.CoverageCovered:
		return "covered (the bot is an admin there)"
	case state == store.CoverageNotAdmin:
		return "the bot is in it but not an admin: a human admin must promote it"
	case tried && jr.err != "":
		return "the bot could not join it by itself (" + jr.err + "): send /join <invite link> for it"
	case tried && jr.pending:
		return "the bot asked to join: an admin of that group must approve it, then promote the bot"
	case tried:
		return "the bot joined it by itself: a human admin must promote it"
	}
	return "the bot is not in it: send /join <invite link> for it"
}

// settle joins absent linked groups by itself, then, in one transaction,
// compares what the sweep found with what the admins were last told and
// records it: a community's first list, a new group, a lost group and too
// few human admins each become a report.
func (s *Sweep) settle(ctx context.Context, res *Result, lists []listing) error {
	prev, err := s.Enforcer.Store.CoverageRows(ctx)
	if err != nil {
		return err
	}
	joins := s.autoJoin(ctx, res, lists, prev)
	cfg := s.Config.Current().Config
	now := s.Enforcer.Store.Now()
	queued := 0
	err = s.Enforcer.Store.Write(ctx, func(tx *sql.Tx) error {
		queued = 0
		prev, err := store.CoverageRows(ctx, tx)
		if err != nil {
			return err
		}
		add := func(r store.Report) error {
			if _, err := store.InsertReport(ctx, tx, r, nil, now); err != nil {
				return err
			}
			queued++
			return nil
		}
		keep := map[string]bool{}
		for _, l := range lists {
			if l.err != "" { // not listed this time: keep what the admins know
				for g, p := range prev {
					if p.Community == l.community {
						keep[g] = true
					}
				}
				continue
			}
			cl := pipeline.CommunityLabel(cfg, l.community)
			first := true
			for _, p := range prev {
				if p.Community == l.community && p.Listed {
					first = false
					break
				}
			}
			var lines []string
			for _, r := range l.refs {
				g := s.groupCoverage(r)
				keep[string(g.JID)] = true
				p := prev[string(g.JID)]
				jr, tried := joins[g.JID]
				label := pipeline.Label(g.Name, g.JID)
				row := store.CoverageRow{Group: string(g.JID), Community: l.community, State: g.State, Listed: true,
					FewAdmins: p.FewAdmins, JoinTried: p.JoinTried || tried || g.State != store.CoverageAbsent}
				switch {
				case !p.Listed && first:
					lines = append(lines, label+": "+todo(g.State, jr, tried))
				case !p.Listed:
					s.Log.Info("sweep: new group in a community", "community", mask.IDs(l.community),
						"group", mask.IDs(string(g.JID)), "state", g.State)
					if err := add(store.Report{Kind: string(alert.Coverage), Community: l.community,
						Text: "New group in " + cl + ": " + label + ": " + todo(g.State, jr, tried) + "."}); err != nil {
						return err
					}
				case p.State == store.CoverageCovered && g.State != store.CoverageCovered:
					s.Log.Error("sweep: the bot no longer covers a group", "community", mask.IDs(l.community),
						"group", mask.IDs(string(g.JID)), "state", g.State)
					if err := add(lostReport(l.community, cl, label, g.State)); err != nil {
						return err
					}
				case p.State != g.State:
					s.Log.Info("sweep: coverage changed", "community", mask.IDs(l.community),
						"group", mask.IDs(string(g.JID)), "from", p.State, "to", g.State)
				}
				switch {
				case g.State == store.CoverageAbsent || g.HumanAdmins >= minHumanAdmins:
					row.FewAdmins = false
				case !p.FewAdmins:
					row.FewAdmins = true
					s.Log.Warn("sweep: too few human admins", "group", mask.IDs(string(g.JID)), "human_admins", g.HumanAdmins)
					if err := add(store.Report{Kind: string(alert.FewHumanAdmins), Community: l.community,
						Text: fmt.Sprintf("Only %d human admin(s) in %s (%s): keep at least %d, so the group still has "+
							"admins if the bot's number is banned.", g.HumanAdmins, label, cl, minHumanAdmins)}); err != nil {
						return err
					}
				}
				if err := store.PutCoverage(ctx, tx, row, now); err != nil {
					return err
				}
			}
			if len(lines) > 0 {
				s.Log.Info("sweep: listed a community's groups", "community", mask.IDs(l.community), "groups", len(lines))
				if err := add(store.Report{Kind: string(alert.Coverage), Community: l.community,
					Text: fmt.Sprintf("The bot checked the %d group(s) of %s:\n%s", len(lines), cl,
						strings.Join(lines, "\n"))}); err != nil {
					return err
				}
			}
		}
		// A group no longer linked to its community, or of a community no
		// longer configured, is forgotten.
		for g := range prev {
			if !keep[g] {
				if err := store.DeleteCoverage(ctx, tx, g); err != nil {
					return err
				}
			}
		}
		return nil
	})
	res.Reports += queued
	if err == nil && queued > 0 {
		s.Enforcer.Notify()
	}
	return err
}

// lostReport is the priority report about a group the bot covered and no
// longer does (state is where it now stands).
func lostReport(community, cl, label, state string) store.Report {
	text := fmt.Sprintf("The bot is no longer an admin in %s (%s): it cannot delete or remove there until a human "+
		"admin promotes it again.", label, cl)
	if state == store.CoverageAbsent {
		text = fmt.Sprintf("The bot is no longer in %s (%s): that group is not moderated until a human admin adds the "+
			"bot back and promotes it.", label, cl)
	}
	return store.Report{Kind: string(alert.CoverageLost), Priority: true, Community: community, Text: text}
}

// LostAdmin is called when WhatsApp refused the bot an admin-only call in
// group (cause says which). The group shows as not covered at once, and a
// priority report says a human admin must promote the bot again, unless the
// admins already know.
func (s *Sweep) LostAdmin(ctx context.Context, group client.JID, cause string) {
	cur := s.Config.Current()
	community := s.Directory.Community(group, cur.Rules)
	label := s.Directory.Label(group)
	s.Directory.BotNotAdmin(group)
	if community == "" {
		s.Log.Warn("WhatsApp refused an admin-only call in a group that is not moderated", "group",
			mask.IDs(string(group)), "cause", cause)
		return
	}
	now := s.Enforcer.Store.Now()
	reported := false
	err := s.Enforcer.Store.Write(ctx, func(tx *sql.Tx) error {
		reported = false
		rows, err := store.CoverageRows(ctx, tx)
		if err != nil {
			return err
		}
		if p, ok := rows[string(group)]; ok && p.State != store.CoverageCovered {
			return nil // already reported as not covered
		}
		if err := store.MarkCoverage(ctx, tx, string(group), community, store.CoverageNotAdmin, now); err != nil {
			return err
		}
		_, err = store.InsertReport(ctx, tx, store.Report{Kind: string(alert.CoverageLost), Priority: true,
			Community: community, Text: fmt.Sprintf("The bot is not an admin in %s (%s): %s there. It cannot delete or "+
				"remove in that group until a human admin promotes it again.", label,
				pipeline.CommunityLabel(cur.Config, community), cause)}, nil, now)
		reported = err == nil
		return err
	})
	if err != nil {
		s.Log.Error("could not record a lost admin right", "group", mask.IDs(string(group)), "err", mask.IDs(err.Error()))
		return
	}
	s.Log.Error("the bot is not an admin in a moderated group", "group", mask.IDs(string(group)), "cause", cause,
		"reported", reported)
	if reported {
		s.Enforcer.Notify()
	}
}

package rules

import (
	"strings"

	"github.com/hoiung/groupwarden/internal/client"
)

// RuleCheck is how one acting rule fared against an input: how many of its
// top-level conditions held and a description of each that did not.
type RuleCheck struct {
	Rule   string
	Met    int
	Failed []string
}

// Closest explains why no acting rule caught an input: of the rules that
// would delete (enabled for in.Community), the one meeting the most of its
// top-level conditions (fewest failed on a tie, then config order), with the
// conditions it failed. ok is false when no acting rule is enabled.
func (rs *Ruleset) Closest(in Input) (best RuleCheck, ok bool) {
	sc := rs.scopeFor(in.Community)
	msg := rs.prepare(in.Fields)
	var name *prepared
	if in.PushName != "" {
		name = rs.prepare([]client.Field{{Name: "push_name", Text: in.PushName}})
	}
	for _, r := range rs.rules {
		if sc.disabled[r.name] || r.action != DeleteRemoveBan {
			continue
		}
		conds := r.when.All
		if conds == nil {
			conds = []Node{r.when}
		}
		rc := RuleCheck{Rule: r.name}
		input := msg
		if r.on == OnPushName {
			input = name
		}
		for _, c := range conds {
			if input != nil && input.holds(c, sc) {
				rc.Met++
				continue
			}
			rc.Failed = append(rc.Failed, Describe(c))
		}
		if r.on == OnPushName && name == nil { // ranks below any rule that reads the message
			rc.Met, rc.Failed = -1, []string{"the sample has no push name"}
		}
		if !ok || rc.Met > best.Met || (rc.Met == best.Met && len(rc.Failed) < len(best.Failed)) {
			best, ok = rc, true
		}
	}
	return best, ok
}

// Describe writes a condition the way the config spells it.
func Describe(n Node) string {
	sub := func(ns []Node) string {
		parts := make([]string, 0, len(ns))
		for _, k := range ns {
			parts = append(parts, Describe(k))
		}
		return strings.Join(parts, "; ")
	}
	switch {
	case n.Words != nil:
		return "words: " + strings.Join(n.Words, " or ")
	case n.Has != nil:
		return "has: " + strings.Join(n.Has, " or ")
	case n.Any != nil:
		return "any of (" + sub(n.Any) + ")"
	}
	return "all of (" + sub(n.All) + ")"
}

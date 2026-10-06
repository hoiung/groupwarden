package rules

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hoiung/groupwarden/internal/normalise"
)

// maxBranches bounds how many ways one rule may match, so a deeply nested
// any/all tree cannot make validation slow.
const maxBranches = 1024

var groupID = regexp.MustCompile(`^[0-9]+@g\.us$`)

type wordList struct {
	name     string
	patterns []normalise.Pattern
	leet     bool
}

type rule struct {
	name      string
	action    Action
	confirmed bool
	on        string
	when      Node
}

// scope is one configured community's effective settings.
type scope struct {
	id       string
	mode     Mode
	disabled map[string]bool
	lists    map[string]*wordList
}

// Ruleset is a compiled, validated rule set. It is immutable, so a reload
// swaps a whole Ruleset and a message never sees half of each.
type Ruleset struct {
	spec     Spec
	never    []normalise.Pattern
	allowed  map[string]bool
	rules    []*rule
	global   *scope
	scopes   map[string]*scope
	groupSet map[string]string // standalone group → its set's name
	banScope BanScope
	hash     string
}

// Compile validates spec and builds the ruleset. Every problem is returned
// at once, each with the config path it came from.
func Compile(spec Spec) (*Ruleset, error) {
	c := &compiler{rs: &Ruleset{
		spec: spec, allowed: map[string]bool{}, scopes: map[string]*scope{}, groupSet: map[string]string{},
		banScope: spec.BanScope,
	}}
	c.compile()
	if len(c.errs) > 0 {
		sort.SliceStable(c.errs, func(i, j int) bool { return strings.Join(c.errs[i].Path, "/") < strings.Join(c.errs[j].Path, "/") })
		return nil, c.errs
	}
	c.rs.hash = hashSpec(c.rs.spec)
	return c.rs, nil
}

type compiler struct {
	rs   *Ruleset
	errs Errors
}

func (c *compiler) fail(path []string, format string, args ...any) {
	c.errs = append(c.errs, &Error{Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (c *compiler) compile() {
	spec := &c.rs.spec
	// The schema supplies both defaults; an unset value here is a caller bug.
	if spec.Mode != Shadow && spec.Mode != Enforce {
		c.fail([]string{"mode"}, "mode must be shadow or enforce (got %q)", spec.Mode)
	}
	if spec.BanScope != AllCommunities && spec.BanScope != PerCommunity {
		c.fail([]string{"bans", "scope"}, "bans.scope must be all_communities or per_community (got %q)", spec.BanScope)
	}
	for i, d := range spec.AllowedDomains {
		reg, ok := RegistrableDomain(d)
		if !ok {
			c.fail([]string{"allowed_domains", strconv.Itoa(i)}, "allowed_domains: %q is not a domain name like \"example.com\"", d)
			continue
		}
		c.rs.allowed[reg] = true
	}
	for i, n := range spec.NeverMatch {
		p, err := normalise.Compile(n)
		if err != nil {
			c.fail([]string{"never_match", strconv.Itoa(i)}, "never_match: %v", err)
			continue
		}
		c.rs.never = append(c.rs.never, p)
	}
	leet := map[string]bool{}
	for i, n := range spec.LeetWordLists {
		if _, ok := spec.WordLists[n]; !ok {
			c.fail([]string{"leet_word_lists", strconv.Itoa(i)}, "leet_word_lists: %q is not a word list", n)
		}
		leet[n] = true
	}
	c.rs.global = &scope{mode: spec.Mode, disabled: map[string]bool{}, lists: map[string]*wordList{}}
	for _, name := range sortedKeys(spec.WordLists) {
		wl := &wordList{name: name, leet: leet[name]}
		c.addWords(wl, spec.WordLists[name], []string{"word_lists", name})
		c.rs.global.lists[name] = wl
	}
	c.compileRules()
	c.compileCommunities()
}

// addWords compiles entries into wl, checking wildcards and length.
func (c *compiler) addWords(wl *wordList, entries []string, path []string) {
	for i, e := range entries {
		p, err := normalise.Compile(e)
		at := append(append([]string{}, path...), strconv.Itoa(i))
		if err != nil {
			c.fail(at, "%s: %v", strings.Join(path, "."), err)
			continue
		}
		if !p.Phrase() && p.Length() < c.rs.spec.MinWordLength {
			c.fail(at, "%s: %q is shorter than rules.min_word_length (%d); use a longer word or a phrase",
				strings.Join(path, "."), e, c.rs.spec.MinWordLength)
			continue
		}
		wl.patterns = append(wl.patterns, p)
	}
}

func (c *compiler) compileRules() {
	seen := map[string]int{}
	for i, rs := range c.rs.spec.Rules {
		path := []string{"rules", "list", strconv.Itoa(i)}
		if j, dup := seen[rs.Name]; dup {
			c.fail(append(path, "name"), "rule %q is defined twice (rules %d and %d)", rs.Name, j+1, i+1)
		}
		seen[rs.Name] = i
		on := rs.On
		if on == "" {
			on = OnMessage
		}
		if rs.Action != Log && rs.Action != DeleteRemoveBan {
			c.fail(append(path, "action"), "rule %q: action must be log or delete_remove_ban", rs.Name)
			continue
		}
		if !c.checkNode(rs.Name, rs.When, append(path, "when")) {
			continue
		}
		if rs.Action == DeleteRemoveBan {
			c.checkActing(rs, append(path, "when"))
		}
		c.rs.rules = append(c.rs.rules, &rule{name: rs.Name, action: rs.Action, confirmed: rs.Confirmed, on: on, when: rs.When})
	}
}

// checkNode checks references and shape; false when the rule is unusable.
func (c *compiler) checkNode(rule string, n Node, path []string) bool {
	set := 0
	for _, b := range []bool{n.All != nil, n.Any != nil, n.Words != nil, n.Has != nil} {
		if b {
			set++
		}
	}
	if set != 1 {
		c.fail(path, "rule %q: each condition must be exactly one of all, any, words or has", rule)
		return false
	}
	ok := true
	for key, kids := range map[string][]Node{"all": n.All, "any": n.Any} {
		if kids == nil {
			continue
		}
		if len(kids) == 0 {
			c.fail(append(path, key), "rule %q: %s needs at least one condition", rule, key)
			return false
		}
		for i, k := range kids {
			ok = c.checkNode(rule, k, append(append([]string{}, path...), key, strconv.Itoa(i))) && ok
		}
	}
	for i, w := range n.Words {
		if _, found := c.rs.spec.WordLists[w]; !found {
			c.fail(append(append([]string{}, path...), "words", strconv.Itoa(i)), "rule %q: %q is not a word list", rule, w)
			ok = false
		}
	}
	for i, h := range n.Has {
		if !isBuiltin(h) {
			c.fail(append(append([]string{}, path...), "has", strconv.Itoa(i)),
				"rule %q: %q is not a built-in condition (%s)", rule, h, strings.Join(Builtins, ", "))
			ok = false
		}
	}
	if (n.Words != nil && len(n.Words) == 0) || (n.Has != nil && len(n.Has) == 0) {
		c.fail(path, "rule %q: words and has need at least one name", rule)
		ok = false
	}
	return ok
}

// branch is one way a rule can match: the word lists and built-ins it needs.
type branch struct {
	lists, has map[string]bool
}

// expand lists every way n can match. ok is false past maxBranches.
func expand(n Node) ([]branch, bool) {
	switch {
	case n.Words != nil:
		out := make([]branch, 0, len(n.Words))
		for _, w := range n.Words {
			out = append(out, branch{lists: map[string]bool{w: true}, has: map[string]bool{}})
		}
		return out, true
	case n.Has != nil:
		out := make([]branch, 0, len(n.Has))
		for _, h := range n.Has {
			out = append(out, branch{lists: map[string]bool{}, has: map[string]bool{h: true}})
		}
		return out, true
	case n.Any != nil:
		var out []branch
		for _, k := range n.Any {
			bs, ok := expand(k)
			if !ok || len(out)+len(bs) > maxBranches {
				return nil, false
			}
			out = append(out, bs...)
		}
		return out, true
	}
	out := []branch{{lists: map[string]bool{}, has: map[string]bool{}}}
	for _, k := range n.All {
		bs, ok := expand(k)
		if !ok || len(out)*len(bs) > maxBranches {
			return nil, false
		}
		next := make([]branch, 0, len(out)*len(bs))
		for _, a := range out {
			for _, b := range bs {
				next = append(next, merge(a, b))
			}
		}
		out = next
	}
	return out, true
}

func merge(a, b branch) branch {
	m := branch{lists: map[string]bool{}, has: map[string]bool{}}
	for k := range a.lists {
		m.lists[k] = true
	}
	for k := range b.lists {
		m.lists[k] = true
	}
	for k := range a.has {
		m.has[k] = true
	}
	for k := range b.has {
		m.has[k] = true
	}
	return m
}

// checkActing enforces the operator's combination rule on every way an
// acting rule can match, so an `any:` group cannot slip a weak branch past.
func (c *compiler) checkActing(rs RuleSpec, path []string) {
	branches, ok := expand(rs.When)
	if !ok {
		c.fail(path, "rule %q has more than %d ways to match; split it into smaller rules", rs.Name, maxBranches)
		return
	}
	for _, b := range branches {
		if len(b.lists) >= 2 {
			continue // a lure rule: keywords from two different word lists
		}
		if len(b.lists) == 1 && anyArtefact(b.has) {
			continue // a keyword and a link or contact detail
		}
		c.fail(path, "rule %q deletes, removes and bans, so every way it can match needs a keyword AND a link or "+
			"contact detail (%s), or keywords from two different word lists; it can match on %s alone. "+
			"Make it action: log, or add the missing condition",
			rs.Name, strings.Join(sortedKeys(artefacts), ", "), describe(b))
		return
	}
}

func anyArtefact(has map[string]bool) bool {
	for h := range has {
		if artefacts[h] {
			return true
		}
	}
	return false
}

func describe(b branch) string {
	var parts []string
	for _, l := range sortedKeys(b.lists) {
		parts = append(parts, "words from "+l)
	}
	parts = append(parts, sortedKeys(b.has)...)
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, " + ")
}

func (c *compiler) compileCommunities() {
	ruleNames := map[string]bool{}
	for _, r := range c.rs.spec.Rules {
		ruleNames[r.Name] = true
	}
	for _, cs := range c.rs.spec.Communities {
		path := []string{"communities", cs.ID}
		if len(cs.Groups) == 0 && !groupID.MatchString(cs.ID) {
			c.fail(path, "communities: %q is not a WhatsApp Community ID (a number ending in @g.us); a named set of standalone groups needs a groups: list", cs.ID)
		}
		for i, g := range cs.Groups {
			if !groupID.MatchString(g) {
				c.fail(append(append([]string{}, path...), "groups", strconv.Itoa(i)), "communities.%s: %q is not a group ID (a number ending in @g.us)", cs.ID, g)
				continue
			}
			if other, dup := c.rs.groupSet[g]; dup {
				c.fail(append(append([]string{}, path...), "groups", strconv.Itoa(i)), "communities.%s: group %s is already in %s", cs.ID, g, other)
			}
			c.rs.groupSet[g] = cs.ID
		}
		sc := &scope{id: cs.ID, mode: cs.Mode, disabled: map[string]bool{}, lists: map[string]*wordList{}}
		if sc.mode == "" {
			sc.mode = c.rs.spec.Mode
		}
		for i, r := range cs.DisableRules {
			if !ruleNames[r] {
				c.fail(append(append([]string{}, path...), "disable_rules", strconv.Itoa(i)), "communities.%s: disable_rules names %q, which is not a rule", cs.ID, r)
			}
			sc.disabled[r] = true
		}
		for name, wl := range c.rs.global.lists {
			sc.lists[name] = wl
		}
		for _, name := range sortedKeys(cs.WordLists) {
			g, ok := c.rs.global.lists[name]
			if !ok {
				c.fail(append(append([]string{}, path...), "word_lists", name), "communities.%s: %q is not a global word list (a community can only add words to existing lists)", cs.ID, name)
				continue
			}
			merged := &wordList{name: name, leet: g.leet, patterns: append([]normalise.Pattern{}, g.patterns...)}
			c.addWords(merged, cs.WordLists[name], []string{"communities", cs.ID, "word_lists", name})
			sc.lists[name] = merged
		}
		c.rs.scopes[cs.ID] = sc
	}
}

func isBuiltin(h string) bool {
	for _, b := range Builtins {
		if b == h {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hashSpec is the sha256 of the spec's canonical JSON (map keys sorted by
// encoding/json), so formatting, comments and key order never change it.
func hashSpec(s Spec) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("rules hash: %v", err)) // a plain struct always marshals
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Hash is the sha256 of the compiled spec (64 hex digits).
func (rs *Ruleset) Hash() string { return rs.hash }

// Spec returns the spec the ruleset was compiled from (defaults filled).
func (rs *Ruleset) Spec() Spec { return rs.spec }

// CommunityOf returns the configured community a group belongs to: its
// standalone set, or its parent community when that is configured. "" means
// the group is not moderated.
func (rs *Ruleset) CommunityOf(group, parent string) string {
	if set, ok := rs.groupSet[group]; ok {
		return set
	}
	if _, ok := rs.scopes[parent]; ok && parent != "" {
		return parent
	}
	return ""
}

// Communities lists every configured community, sorted.
func (rs *Ruleset) Communities() []string { return sortedKeys(rs.scopes) }

// SetGroups lists the groups of a configured standalone set, sorted; nil for
// a WhatsApp Community, whose groups are discovered from WhatsApp.
func (rs *Ruleset) SetGroups(community string) []string {
	var out []string
	for g, set := range rs.groupSet {
		if set == community {
			out = append(out, g)
		}
	}
	sort.Strings(out)
	return out
}

// ModeFor returns a configured community's mode; ok is false when the
// community is not configured (any more).
func (rs *Ruleset) ModeFor(community string) (mode Mode, ok bool) {
	sc, ok := rs.scopes[community]
	if !ok {
		return "", false
	}
	return sc.mode, true
}

// BanScope says whether a ban covers every community or only its own.
func (rs *Ruleset) BanScope() BanScope { return rs.banScope }

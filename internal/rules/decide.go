package rules

import (
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/normalise"
)

// Input is one message to decide on.
type Input struct {
	// Community is the configured community of the message's group
	// (Ruleset.CommunityOf); "" uses the global settings.
	Community string
	Fields    []client.Field
	PushName  string
	// SenderIsAdmin: the sender is a current admin in Community (of it or of
	// a group that counts towards it). Admins are reported, never actioned (a
	// hijacked admin still shows up in the report); an admin only elsewhere is
	// actioned like anyone.
	SenderIsAdmin bool
	// FromMetaAI: the sender is WhatsApp's Meta AI participant.
	FromMetaAI bool
}

// Exemption says why a matched sender was only reported.
type Exemption string

const (
	ExemptAdmin  Exemption = "admin"
	ExemptMetaAI Exemption = "meta_ai"
)

// Match is one rule that matched.
type Match struct {
	Rule   string `json:"rule"`
	Action Action `json:"action"`
	// Enforced: an acting rule, confirmed, in a community in enforce mode.
	Enforced bool `json:"enforced"`
}

// Decision is what happens to one message.
type Decision struct {
	// Action: ActionNone, Log or DeleteRemoveBan.
	Action Action `json:"action"`
	// Rule decided the action ("" when nothing matched).
	Rule string `json:"rule,omitempty"`
	// WouldHaveActed: an acting rule matched but is watch-only (shadow mode
	// or not yet confirmed), so the post is only reported as "would have acted".
	WouldHaveActed bool      `json:"would_have_acted,omitempty"`
	Exempt         Exemption `json:"exempt,omitempty"`
	// BanIn lists the communities the ban covers (DeleteRemoveBan only).
	BanIn   []string `json:"ban_in,omitempty"`
	Matches []Match  `json:"matches,omitempty"`
	// Lists are the word lists with a keyword in the message, for the daily
	// summary of keyword-only posts no rule acted on.
	Lists []string `json:"lists,omitempty"`
}

// prepared is one input (the message, or the push name) ready for matching.
type prepared struct {
	texts   []normalise.Text
	fields  []client.Field
	signals signals
	allowed map[string]bool
	hits    map[string]bool // word list → found (memoised)
}

func (rs *Ruleset) prepare(fields []client.Field) *prepared {
	p := &prepared{fields: fields, allowed: rs.allowed, hits: map[string]bool{}}
	for _, f := range fields {
		text := f.Match
		if text == "" {
			text = f.Text
		}
		p.texts = append(p.texts, normalise.Prepare(text, rs.never))
	}
	return p
}

func (p *prepared) list(wl *wordList) bool {
	if hit, ok := p.hits[wl.name]; ok {
		return hit
	}
	hit := p.listAvoiding(wl, nil)
	p.hits[wl.name] = hit
	return hit
}

// listAvoiding reports whether a keyword of wl occurs in a field using none
// of that field's characters in avoid (nil avoids nothing).
func (p *prepared) listAvoiding(wl *wordList, avoid []map[int]bool) bool {
	for _, pat := range wl.patterns {
		for i, t := range p.texts {
			var a map[int]bool
			if avoid != nil {
				a = avoid[i]
			}
			if pat.MatchAvoiding(t, wl.leet, a) {
				return true
			}
		}
	}
	return false
}

// words is, per field, every character of the original text a keyword of wl
// matches in any reading (plain, look-alike letters, digit swaps, spaced
// letters joined).
func (p *prepared) words(wl *wordList) []map[int]bool {
	used := make([]map[int]bool, len(p.texts))
	for i, t := range p.texts {
		used[i] = map[int]bool{}
		for _, pat := range wl.patterns {
			pat.Words(t, wl.leet, used[i])
		}
	}
	return used
}

// acts reports whether an acting rule matches: one of the ways it can match
// holds, and a way without a link or contact detail (a lure: keywords from
// two word lists) has those keywords in different words of the post. One
// keyword that two lists share, or that a wildcard of each list matches,
// never deletes, removes and bans on its own.
func (p *prepared) acts(r *rule, sc *scope) bool {
	for _, b := range r.branches {
		if p.branchHolds(b, sc) && (anyArtefact(b.has) || p.differentWords(b.lists, sc)) {
			return true
		}
	}
	return false
}

func (p *prepared) branchHolds(b branch, sc *scope) bool {
	for h := range b.has {
		if !p.has(h) {
			return false
		}
	}
	for l := range b.lists {
		if wl, ok := sc.lists[l]; !ok || !p.list(wl) {
			return false
		}
	}
	return true
}

// differentWords reports whether two of lists match different words: one
// list matches on characters of the post that another list matches in no
// reading. A keyword read one way by one list and another way by the other
// (DOGECOIN, d0gecoin) is the same characters, so it is one word. A "*"
// reaches only as far as a break between words that some reading shows
// (normalise.go stands), so "profit|bitcoin" is two words although the
// look-alike reading joins them. A phrase of one list overlapping a phrase of
// another is not counted, which can only leave a post unactioned, never
// action one.
func (p *prepared) differentWords(lists map[string]bool, sc *scope) bool {
	names := sortedKeys(lists)
	for _, a := range names {
		used := p.words(sc.lists[a])
		for _, b := range names {
			if b != a && p.listAvoiding(sc.lists[b], used) {
				return true
			}
		}
	}
	return false
}

func (p *prepared) has(name string) bool {
	if p.signals == nil {
		p.signals = detect(p.fields, p.allowed)
	}
	return p.signals[name]
}

// holds reports whether condition n is true for this input.
func (p *prepared) holds(n Node, sc *scope) bool {
	switch {
	case n.Words != nil:
		for _, w := range n.Words {
			if wl, ok := sc.lists[w]; ok && p.list(wl) {
				return true
			}
		}
		return false
	case n.Has != nil:
		for _, h := range n.Has {
			if p.has(h) {
				return true
			}
		}
		return false
	case n.Any != nil:
		for _, k := range n.Any {
			if p.holds(k, sc) {
				return true
			}
		}
		return false
	}
	for _, k := range n.All {
		if !p.holds(k, sc) {
			return false
		}
	}
	return true
}

func (rs *Ruleset) scopeFor(community string) *scope {
	if sc, ok := rs.scopes[community]; ok {
		return sc
	}
	return rs.global
}

// matchAll evaluates every rule enabled in sc. The push name is its own
// input: a rule reads either the message or the push name, never both.
func (rs *Ruleset) matchAll(in Input, sc *scope) ([]Match, []string) {
	msg := rs.prepare(in.Fields)
	var name *prepared
	if in.PushName != "" {
		name = rs.prepare([]client.Field{{Name: "push_name", Text: in.PushName}})
	}
	var matches []Match
	for _, r := range rs.rules {
		if sc.disabled[r.name] {
			continue
		}
		input := msg
		if r.on == OnPushName {
			if name == nil {
				continue
			}
			input = name
		}
		var matched bool
		if r.action == DeleteRemoveBan {
			matched = input.acts(r, sc)
		} else {
			matched = input.holds(r.when, sc)
		}
		if matched {
			matches = append(matches, Match{
				Rule: r.name, Action: r.action,
				Enforced: r.action == DeleteRemoveBan && r.confirmed && sc.mode == Enforce,
			})
		}
	}
	var lists []string
	for _, name := range sortedKeys(sc.lists) {
		if msg.list(sc.lists[name]) {
			lists = append(lists, name)
		}
	}
	return matches, lists
}

// Decide applies every rule to one message. There is one decision for every
// member: a match of an enforced rule deletes the post and removes and bans
// the sender wherever the ban scope reaches, on the first post, however long
// they have been a member. Admins and Meta AI are reported instead.
func (rs *Ruleset) Decide(in Input) Decision {
	sc := rs.scopeFor(in.Community)
	matches, lists := rs.matchAll(in, sc)
	d := Decision{Matches: matches, Lists: lists}
	var acting, logged *Match
	for i := range matches {
		m := &matches[i]
		switch {
		case m.Enforced:
			if d.Action != DeleteRemoveBan {
				d.Action, d.Rule = DeleteRemoveBan, m.Rule
			}
		case m.Action == DeleteRemoveBan && acting == nil:
			acting = m
		case m.Action == Log && logged == nil:
			logged = m
		}
	}
	// An admin or Meta AI is exempt from every acting rule, enforced or
	// watch-only: a watch-only match on them is reported as exempt, never as
	// one the bot would have acted on.
	var exempt Exemption
	switch {
	case in.FromMetaAI:
		exempt = ExemptMetaAI
	case in.SenderIsAdmin:
		exempt = ExemptAdmin
	}
	switch {
	case d.Action == DeleteRemoveBan && exempt != "":
		d.Action, d.Exempt = Log, exempt
	case d.Action == DeleteRemoveBan:
		d.BanIn = rs.BanTargets(in.Community)
	case acting != nil && exempt != "":
		d.Action, d.Rule, d.Exempt = Log, acting.Rule, exempt
	case acting != nil:
		d.Action, d.Rule, d.WouldHaveActed = Log, acting.Rule, true
	case logged != nil:
		d.Action, d.Rule = Log, logged.Rule
	}
	return d
}

// BanTargets is every configured community for all_communities, else the
// message's own community.
func (rs *Ruleset) BanTargets(community string) []string {
	if rs.banScope == PerCommunity {
		if community == "" {
			return nil
		}
		return []string{community}
	}
	return rs.Communities()
}

// ActsOn is the corpus test's view of a message, as if every rule were
// confirmed and enforced: the matching rules that would delete it, and every
// matching rule.
func (rs *Ruleset) ActsOn(in Input) (acting, matched []string) {
	matches, _ := rs.matchAll(in, rs.scopeFor(in.Community))
	for _, m := range matches {
		matched = append(matched, m.Rule)
		if m.Action == DeleteRemoveBan {
			acting = append(acting, m.Rule)
		}
	}
	return acting, matched
}

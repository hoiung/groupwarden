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
	// SenderIsAdmin: the sender is a current admin of ANY moderated group or
	// community. Admins are reported, never actioned (a hijacked admin still
	// shows up in the report).
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
	hit := false
	for _, pat := range wl.patterns {
		for _, t := range p.texts {
			if pat.MatchIn(t, wl.leet) {
				hit = true
				break
			}
		}
		if hit {
			break
		}
	}
	p.hits[wl.name] = hit
	return hit
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
		if input.holds(r.when, sc) {
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
	switch {
	case d.Action == DeleteRemoveBan && in.FromMetaAI:
		d.Action, d.Exempt = Log, ExemptMetaAI
	case d.Action == DeleteRemoveBan && in.SenderIsAdmin:
		d.Action, d.Exempt = Log, ExemptAdmin
	case d.Action == DeleteRemoveBan:
		d.BanIn = rs.BanTargets(in.Community)
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

// ActsOn reports whether any acting rule matches as if every rule were
// confirmed and enforced (the corpus test's view), and which rules matched.
func (rs *Ruleset) ActsOn(in Input) (bool, []string) {
	matches, _ := rs.matchAll(in, rs.scopeFor(in.Community))
	var names []string
	acts := false
	for _, m := range matches {
		names = append(names, m.Rule)
		if m.Action == DeleteRemoveBan {
			acts = true
		}
	}
	return acts, names
}

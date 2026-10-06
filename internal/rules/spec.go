// Package rules compiles the operator's word lists and rules and decides
// what happens to each message.
//
// There is one decision for everyone (operator rule S3-12): a post matching
// an enforced rule is deleted and its sender removed and banned in every
// group the ban covers, on the first post, whoever they are. Only current
// admins and WhatsApp's Meta AI participant are exempt, and they are reported.
package rules

import (
	"encoding/json"
	"fmt"
)

// Mode says whether a community's rules act or only report.
type Mode string

const (
	Shadow  Mode = "shadow"  // report what it would do
	Enforce Mode = "enforce" // act
)

// Action is what a rule does when it matches.
type Action string

const (
	// ActionNone: nothing matched.
	ActionNone Action = ""
	// Log reports only.
	Log Action = "log"
	// DeleteRemoveBan deletes the post, removes the sender and bans them.
	DeleteRemoveBan Action = "delete_remove_ban"
)

// BanScope says which communities a ban covers.
type BanScope string

const (
	AllCommunities BanScope = "all_communities"
	PerCommunity   BanScope = "per_community"
)

// Input names what a rule reads.
const (
	OnMessage  = "message"   // every field the sender wrote
	OnPushName = "push_name" // the sender's display name, never combined with the message
)

// Names is one name or a list of names ("crypto" or [crypto, stocks]).
type Names []string

// UnmarshalJSON accepts a string or an array of strings.
func (n *Names) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*n = Names{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("expected a name or a list of names")
	}
	*n = many
	return nil
}

// Node is one condition. Exactly one field is set:
//   - All: every child matches
//   - Any: at least one child matches
//   - Words: a keyword from any of these word lists
//   - Has: any of these built-in conditions (any_link, invite_link, ...)
type Node struct {
	All   []Node `json:"all,omitempty"`
	Any   []Node `json:"any,omitempty"`
	Words Names  `json:"words,omitempty"`
	Has   Names  `json:"has,omitempty"`
}

// RuleSpec is one rule as written in the config.
type RuleSpec struct {
	Name   string `json:"name"`
	Action Action `json:"action"`
	// Confirmed: an operator has confirmed the rule. Unconfirmed rules only
	// report what they would have done, whatever the community's mode.
	Confirmed bool   `json:"confirmed"`
	On        string `json:"on"` // OnMessage (default) or OnPushName
	When      Node   `json:"when"`
}

// CommunitySpec overrides the global settings for one configured community:
// a WhatsApp Community (ID = its "…@g.us" ID) or a named set of standalone
// groups (Groups listed; ID = the set's name).
type CommunitySpec struct {
	ID           string   `json:"id"`
	Groups       []string `json:"groups,omitempty"`
	Mode         Mode     `json:"mode,omitempty"`
	DisableRules []string `json:"disable_rules,omitempty"`
	// WordLists adds entries to global word lists of the same name.
	WordLists map[string][]string `json:"word_lists,omitempty"`
}

// Spec is everything the rules compile from. Error paths use the config
// file's own keys (word_lists, rules.list, communities, ...).
type Spec struct {
	Mode           Mode                `json:"mode"`
	MinWordLength  int                 `json:"min_word_length"`
	WordLists      map[string][]string `json:"word_lists"`
	LeetWordLists  []string            `json:"leet_word_lists,omitempty"`
	NeverMatch     []string            `json:"never_match,omitempty"`
	AllowedDomains []string            `json:"allowed_domains,omitempty"`
	Rules          []RuleSpec          `json:"rules"`
	Communities    []CommunitySpec     `json:"communities,omitempty"`
	BanScope       BanScope            `json:"ban_scope"`
}

// Error is one problem found while compiling, at a config path.
type Error struct {
	Path []string
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Errors is every problem found while compiling.
type Errors []*Error

func (es Errors) Error() string {
	s := ""
	for i, e := range es {
		if i > 0 {
			s += "\n"
		}
		s += e.Msg
	}
	return s
}

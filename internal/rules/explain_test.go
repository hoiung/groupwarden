package rules

import (
	"reflect"
	"testing"
)

// TestClosestRule: for a message no acting rule catches, Closest names the
// acting rule meeting the most of its conditions (fewest failed on a tie,
// then config order) and describes each condition it failed; a push-name
// rule with no push name fails as such; a disabled rule or a log rule is
// never named; no acting rule at all is reported as not ok.
func TestClosestRule(t *testing.T) {
	spec := testSpec()
	spec.Rules = append(spec.Rules,
		RuleSpec{Name: "name-pitch", Action: DeleteRemoveBan, Confirmed: true, On: OnPushName,
			When: Node{All: []Node{{Words: Names{"crypto"}}, {Words: Names{"lures"}}}}},
		RuleSpec{Name: "watch", Action: Log, When: Node{Words: Names{"crypto"}}})
	rs := compile(t, spec)
	pitchHas := "has: " + artefactNode.Has[0]
	for _, h := range artefactNode.Has[1:] {
		pitchHas += " or " + h
	}
	cases := []struct {
		name string
		in   Input
		want RuleCheck
	}{
		{"a crypto word alone: a tie on one condition met, the first rule wins",
			Input{Fields: body("bitcoin to the moon")},
			RuleCheck{Rule: "crypto-or-stocks-pitch", Met: 1, Failed: []string{pitchHas}}},
		{"a lure alone: only the lure rule meets a condition",
			Input{Fields: body("guaranteed returns, inbox me")},
			RuleCheck{Rule: "crypto-lure", Met: 1, Failed: []string{"words: crypto or stocks"}}},
		{"nothing at all: every rule fails everything, the first is named",
			Input{Fields: body("see you on Sunday")},
			RuleCheck{Rule: "crypto-or-stocks-pitch", Met: 0, Failed: []string{"words: crypto or stocks", pitchHas}}},
		{"a push name meeting both conditions is not a miss of the message rules",
			Input{Fields: body("hello"), PushName: "bitcoin dm me"},
			RuleCheck{Rule: "name-pitch", Met: 2}},
	}
	for _, c := range cases {
		got, ok := rs.Closest(c.in)
		if !ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v (ok %v), want %+v", c.name, got, ok, c.want)
		}
	}
	// No push name: the push-name rule fails as such and is never closest
	// over a rule meeting one condition.
	if got, _ := rs.Closest(Input{Fields: body("hello")}); got.Rule == "name-pitch" {
		t.Errorf("a push-name rule was named for a sample with no push name: %+v", got)
	}
	only := testSpec()
	only.Rules = []RuleSpec{{Name: "name-pitch", Action: DeleteRemoveBan, Confirmed: true, On: OnPushName,
		When: Node{All: []Node{{Words: Names{"crypto"}}, {Words: Names{"lures"}}}}}}
	got, ok := compile(t, only).Closest(Input{Fields: body("bitcoin dm me")})
	if !ok || got.Rule != "name-pitch" || !reflect.DeepEqual(got.Failed, []string{"the sample has no push name"}) {
		t.Errorf("push-name rule without a push name: %+v %v", got, ok)
	}
	// A tie on conditions met goes to the rule that failed fewer, ahead of
	// one listed earlier.
	tie := testSpec()
	tie.Rules = append([]RuleSpec{{Name: "three-way", Action: DeleteRemoveBan, Confirmed: true,
		When: Node{All: []Node{{Words: Names{"crypto"}}, artefactNode, {Words: Names{"lures"}}}}}}, tie.Rules...)
	if got, _ := compile(t, tie).Closest(Input{Fields: body("bitcoin to the moon")}); got.Rule != "crypto-or-stocks-pitch" {
		t.Errorf("tie on met: got %+v, want crypto-or-stocks-pitch (1 failed) over three-way (2 failed)", got)
	}
	// A rule disabled in the community is skipped; none left → not ok.
	off := testSpec()
	off.Communities[0].DisableRules = []string{"crypto-or-stocks-pitch", "crypto-lure"}
	if got, ok := compile(t, off).Closest(Input{Community: communityA, Fields: body("bitcoin")}); ok {
		t.Errorf("disabled rules named: %+v", got)
	}
	if got := Describe(Node{Any: []Node{{Words: Names{"crypto"}}, {All: []Node{{Has: Names{AnyLink}}, {Words: Names{"lures"}}}}}}); got !=
		"any of (words: crypto; all of (has: any_link; words: lures))" {
		t.Errorf("Describe nested: %q", got)
	}
}

package config

import (
	"strings"
	"testing"
)

// Groups of no configured community (listed in .secret-pii-allowlist); the
// second has an older group's ID (creator's number, "-", creation time).
const (
	loneGroup = "99999000000333@g.us"
	oldGroup  = "447700900123-1500000111@g.us"
)

// TestWithEnabledMergesTheList: an /enable community joins the communities
// with the global settings, a single group becomes a set of one (an older
// group's ID too), and what the file already lists keeps the file's settings;
// the version stays the file's and the file-only config is left as it was.
func TestWithEnabledMergesTheList(t *testing.T) {
	l := parse(t, lists+"communities:\n  \""+communityA+"\":\n    name: from the file\n    mode: shadow\n"+
		"  set-b:\n    groups: [\""+groupSet+"\"]\n")
	m, err := l.WithEnabled([]Enabled{
		{ID: communityB, Name: "book club"},
		{ID: loneGroup, Group: true, Name: "public chat"},
		{ID: oldGroup, Group: true, Name: "old chat"},
		{ID: communityA, Name: "renamed"},
		{ID: groupSet, Group: true, Name: "again"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mode, ok := m.Rules.ModeFor(communityB); !ok || mode != m.Config.Mode {
		t.Fatalf("community B: mode %q on %v, want the global %q", mode, ok, m.Config.Mode)
	}
	for _, g := range []string{loneGroup, oldGroup} {
		if got := m.Rules.CommunityOf(g, communityB); got != g {
			t.Fatalf("the single group %s belongs to %q, want its own set", g, got)
		}
	}
	if got := m.Rules.CommunityOf(groupSet, ""); got != "set-b" {
		t.Fatalf("the file's set group moved to %q", got)
	}
	if mode, _ := m.Rules.ModeFor(communityA); mode != "shadow" || m.Config.Communities[communityA].Name != "from the file" {
		t.Fatalf("the file's community lost its settings: mode %q name %q", mode, m.Config.Communities[communityA].Name)
	}
	if m.Config.Communities[communityB].Name != "book club" {
		t.Fatalf("community B name %q", m.Config.Communities[communityB].Name)
	}
	for id, want := range map[string]bool{communityA: true, "set-b": true, groupSet: true, communityB: false, loneGroup: false} {
		if m.FromFile(id) != want {
			t.Errorf("FromFile(%s) = %v, want %v", id, m.FromFile(id), want)
		}
	}
	if m.Hash != l.Hash {
		t.Fatalf("the merged config is v%s, the file v%s: check would print another version than the bot posts", m.Hash, l.Hash)
	}
	if _, ok := l.Rules.ModeFor(communityB); ok {
		t.Fatal("WithEnabled changed the config it was called on")
	}
	again, err := l.WithEnabled(nil)
	if err != nil || again.Hash != l.Hash {
		t.Fatalf("an empty list: %v, version %s, want %s", err, again.Hash, l.Hash)
	}
}

// TestOwnSettings names each setting a community gives itself, in the order
// the reply lists them; an entry with only a name has none.
func TestOwnSettings(t *testing.T) {
	days := 7
	full := Community{Name: "x", Mode: "shadow", DisableRules: []string{"r"}, WordLists: map[string][]string{"w": {"a"}},
		Retention: &RetentionOverride{EvidenceDays: &days}}
	if got := strings.Join(full.OwnSettings(), ","); got != "mode,disable_rules,word_lists,retention" {
		t.Fatalf("own settings %q", got)
	}
	if got := (Community{Name: "x", Groups: []string{loneGroup}}).OwnSettings(); len(got) != 0 {
		t.Fatalf("a name and groups only: %v", got)
	}
}

// TestOlderGroupIDInASet: a set in the file may list an older group's ID; a
// community key may not take that form.
func TestOlderGroupIDInASet(t *testing.T) {
	l := parse(t, lists+"communities:\n  set-old:\n    groups: [\""+oldGroup+"\"]\n")
	if got := l.Rules.CommunityOf(oldGroup, ""); got != "set-old" {
		t.Fatalf("the older group belongs to %q", got)
	}
	refused(t, lists+"communities:\n  \""+oldGroup+"\":\n    name: x\n", "is not a WhatsApp Community ID")
}

// TestWithEnabledNoCommunitiesInTheFile: a file with no communities: key
// (it is optional) takes /enable entries too.
func TestWithEnabledNoCommunitiesInTheFile(t *testing.T) {
	l := parse(t, lists)
	if len(l.Config.Communities) != 0 {
		t.Fatalf("the fixture lists communities: %v", l.Config.Communities)
	}
	m, err := l.WithEnabled([]Enabled{{ID: communityB, Name: "book club"}, {ID: loneGroup, Group: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Rules.ModeFor(communityB); !ok || m.Rules.CommunityOf(loneGroup, "") != loneGroup {
		t.Fatalf("communities %v", m.Rules.Communities())
	}
}

// TestSetEnabledSurvivesReload: the holder keeps the list, so a reload of
// the file merges it again; an empty list switches it off.
func TestSetEnabledSurvivesReload(t *testing.T) {
	_, path, good := reloadFixture(t)
	h, rejected, err := Boot(path)
	if err != nil || rejected != nil {
		t.Fatalf("boot: %v %v", err, rejected)
	}
	if _, err := h.SetEnabled([]Enabled{{ID: communityB, Name: "book club"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Current().Rules.ModeFor(communityB); !ok {
		t.Fatal("SetEnabled did not switch the community on")
	}
	writeConfig(t, path, good+"mode: enforce\n")
	if _, err := h.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Current().Rules.ModeFor(communityB); !ok || h.Current().Config.Mode != "enforce" {
		t.Fatalf("after a reload: on %v, mode %q", ok, h.Current().Config.Mode)
	}
	if _, err := h.SetEnabled(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Current().Rules.ModeFor(communityB); ok {
		t.Fatal("an empty list left the community on")
	}
}

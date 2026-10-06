package corpus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/rules"
)

func ruleset(t *testing.T) *rules.Ruleset {
	t.Helper()
	rs, err := rules.Compile(rules.Spec{
		Mode: rules.Shadow, BanScope: rules.AllCommunities, MinWordLength: 3,
		WordLists: map[string][]string{"crypto": {"crypto", "bitcoin"}, "lures": {"inbox me"}},
		Rules: []rules.RuleSpec{
			{Name: "pitch", Action: rules.DeleteRemoveBan, When: rules.Node{All: []rules.Node{
				{Words: rules.Names{"crypto"}}, {Has: rules.Names{rules.AnyLink, rules.PhoneNumber}}}}},
			{Name: "lure", Action: rules.DeleteRemoveBan, When: rules.Node{All: []rules.Node{
				{Words: rules.Names{"crypto"}}, {Words: rules.Names{"lures"}}}}},
			{Name: "name-watch", Action: rules.Log, On: rules.OnPushName, When: rules.Node{Words: rules.Names{"crypto"}}},
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return rs
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func names(fs []client.Field) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

// TestFieldsPerType: each sample type yields the fields the WhatsApp adapter
// extracts from that kind of message; the quoted message is never a field and
// a listed mention is masked.
func TestFieldsPerType(t *testing.T) {
	cases := []struct {
		s    Sample
		want string
	}{
		{Sample{Type: TypeText, Text: "hi", Quoted: "bitcoin t.me/x"}, "body"},
		{Sample{Type: TypeImageCaption, Text: "hi"}, "caption"},
		{Sample{Type: TypePoll, Text: "q\na\nb"}, "poll.question poll.option poll.option"},
		{Sample{Type: TypeContact, Text: "Desk\n+44 7700 900456"}, "contact.name contact.number"},
		{Sample{Type: TypeInvite, Text: "join us"}, "invite.caption invite.link"},
		{Sample{Type: TypeEvent, Text: "Talk\nabout it\nhttps://example.test/j"}, "event.name event.description event.join_link"},
	}
	for _, c := range cases {
		fs := c.s.Fields()
		if got := strings.Join(names(fs), " "); got != c.want {
			t.Errorf("%s: fields %q, want %q", c.s.Type, got, c.want)
		}
		for _, f := range fs {
			if strings.Contains(f.Text, "bitcoin") {
				t.Errorf("%s: quoted text became a field", c.s.Type)
			}
		}
	}
	s := Sample{Type: TypeText, Text: "@447700900123 thanks", Mentions: []string{"447700900123"}}
	f := s.Fields()[0]
	if strings.Contains(f.Match, "447700900123") || !strings.Contains(f.Match, client.MentionMarker) || f.Text != s.Text {
		t.Errorf("mention: text %q match %q", f.Text, f.Match)
	}
}

// TestLoadReadsLabelsAndClasses: spam/ and legit/<class>/ set the label and
// class; a legit sample outside a class folder is "general".
func TestLoadReadsLabelsAndClasses(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "spam/a/one.yaml", "text: \"bitcoin t.me/x\"\n")
	write(t, dir, "legit/in-stock/two.yaml", "type: image-caption\npush_name: \"Dave\"\ntext: \"still in stock\"\n")
	write(t, dir, "legit/three.yaml", "text: \"hello\"\n")
	write(t, dir, "legit/notes.txt", "not a sample")
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, s := range got {
		seen[filepath.Base(s.Path)] = s.Label + "/" + s.Class + "/" + s.Type
	}
	want := map[string]string{"one.yaml": "spam/a/text", "two.yaml": "legit/in-stock/image-caption", "three.yaml": "legit/general/text"}
	if len(seen) != len(want) {
		t.Fatalf("loaded %v", seen)
	}
	for k, v := range want {
		if seen[k] != v {
			t.Errorf("%s = %q, want %q", k, seen[k], v)
		}
	}
}

// TestReadSampleRefusesBadFiles: an unknown key, an unknown type and empty
// text each fail with the file named.
func TestReadSampleRefusesBadFiles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"key.yaml":   "text: \"x\"\nsender: \"y\"\n",
		"type.yaml":  "type: sticker\ntext: \"x\"\n",
		"empty.yaml": "text: \"  \"\n",
	} {
		write(t, dir, name, body)
		if _, err := ReadSample(filepath.Join(dir, name)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := Load(filepath.Join(dir, "missing")); err != nil {
		t.Errorf("a corpus with no spam/ or legit/ folder: %v", err)
	}
}

// TestRunCountsAndErr: every acting rule is evaluated as if enforced; a
// log-only rule is counted but never makes a sample "acted on"; Err fails on
// a legit hit, a missed spam, or an empty label.
func TestRunCountsAndErr(t *testing.T) {
	rs := ruleset(t)
	dir := t.TempDir()
	write(t, dir, "spam/1.yaml", "text: \"bitcoin here t.me/x\"\n")
	write(t, dir, "spam/2.yaml", "text: \"crypto, inbox me\"\n")
	write(t, dir, "legit/talk/1.yaml", "push_name: \"Crypto Dave\"\ntext: \"great crypto talk\"\n")
	r, err := Run(rs, dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Spam != 2 || r.Legit != 1 || r.LegitHits != 0 || r.SpamMissed != 0 || r.Err() != nil {
		t.Fatalf("result %+v err %v", r, r.Err())
	}
	if r.RuleHits["pitch"] != 1 || r.RuleHits["lure"] != 1 || r.RuleHits["name-watch"] != 1 {
		t.Errorf("rule hits %v", r.RuleHits)
	}
	if len(r.Classes) != 1 || r.Classes[0] != (ClassResult{Class: "talk", Samples: 1}) {
		t.Errorf("classes %+v", r.Classes)
	}

	write(t, dir, "legit/talk/2.yaml", "text: \"crypto meetup t.me/x\"\n")
	write(t, dir, "spam/3.yaml", "text: \"bitcoin to the moon\"\n")
	r, _ = Run(rs, dir)
	err = r.Err()
	if r.LegitHits != 1 || r.SpamMissed != 1 || err == nil ||
		!strings.Contains(err.Error(), "1 legit sample(s) would be deleted") || !strings.Contains(err.Error(), "1 spam sample(s) are not caught") {
		t.Fatalf("result %+v err %v", r, err)
	}

	for _, label := range []string{"spam", "legit"} {
		only := t.TempDir()
		write(t, only, label+"/1.yaml", "text: \"hello\"\n")
		r, _ := Run(rs, only)
		other := map[string]string{"spam": "legit", "legit": "spam"}[label]
		if err := r.Err(); err == nil || !strings.Contains(err.Error(), "no "+other+" samples") {
			t.Errorf("only %s samples: err = %v", label, err)
		}
	}
}

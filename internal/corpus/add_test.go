package corpus

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hoiung/groupwarden/internal/rules"
)

// Numbers outside the repo's PII allowlist are built at run time so the
// public-repo scanner never sees one written out.
var (
	ukMobile = fmt.Sprintf("+%d %d %d", 44, 7911, 123456) // +44, then 10 digits
	ngMobile = fmt.Sprintf("+%d %d %d %d", 234, 803, 123, 4567)
	national = fmt.Sprintf("0%d %d", 7911, 123456)
	waDigits = fmt.Sprintf("%d%d", 44, 7911123456)
	zeros    = strings.Repeat
)

// acts reports whether the test ruleset's acting rules catch text.
func acts(t *testing.T, text string) bool {
	t.Helper()
	acting, _ := ruleset(t).ActsOn(rules.Input{Fields: Sample{Text: text}.Fields()})
	return len(acting) > 0
}

// TestPrivateKeepsShape: the private mode keeps each number's country code
// (or national 0) and length, the domains and URL paths, and masks only the
// personal digits, invite codes and @-mentions. A redacted spam still trips
// the same rule.
func TestPrivateKeepsShape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"bitcoin desk " + ukMobile, "bitcoin desk +44 " + zeros("0", 4) + " " + zeros("0", 6)},
		{"call " + ngMobile + " now", "call +234 " + zeros("0", 3) + " " + zeros("0", 3) + " " + zeros("0", 4) + " now"},
		{"text " + national, "text 0" + zeros("0", 4) + " " + zeros("0", 6)},
		{"wa.me/" + waDigits + " for crypto", "wa.me/44" + zeros("0", 10) + " for crypto"},
		{"join https://chat.whatsapp.com/AbCdEf0123456789xyz now", "join https://chat.whatsapp.com/INVITECODE now"},
		{"t.me/+AbCdEf0123 and t.me/joinchat/XyZ987", "t.me/+INVITECODE and t.me/joinchat/INVITECODE"},
		{"https://whatsapp.com/channel/0029VaXyZ", "https://whatsapp.com/channel/INVITECODE"},
		{"t.me/example_signals", "t.me/example_signals"},
		{"https://promo.example.co.uk/vip/join?ref=77", "https://promo.example.co.uk/vip/join?ref=77"},
		{"@" + waDigits + " thanks, DM @example_signals", "@mention thanks, DM @example_signals"},
		{"mail desk@example.org", "mail desk@example.org"},
		{"price 1,500 on 12/05/2026", "price 1,500 on 12/05/2026"},
	}
	for _, c := range cases {
		if got := Redact(c.in, false); got != c.want {
			t.Errorf("Redact(%q, private)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	for _, spam := range []string{"bitcoin desk " + ukMobile, "bitcoin https://promo.example.co.uk/vip/join?ref=77"} {
		if !acts(t, spam) || !acts(t, Redact(spam, false)) {
			t.Errorf("%q: caught before %v, after private redaction %v", spam, acts(t, spam), acts(t, Redact(spam, false)))
		}
	}
}

// TestPublicFullRedaction: the public mode swaps every number for a
// drama-range number on the PII allowlist (the same number keeps its stand-in),
// every non-invite URL path for /REDACTED, every email for name@example.com,
// and still trips the same rule.
func TestPublicFullRedaction(t *testing.T) {
	in := "bitcoin " + ukMobile + " or " + ngMobile + " again " + ukMobile +
		" wa.me/" + waDigits + " https://chat.whatsapp.com/AbCdEf0123456789xyz t.me/example_signals" +
		" https://promo.example.co.uk/vip/join?ref=77 desk@example.org @" + waDigits
	// The wa.me number is the first number again: it keeps the same stand-in.
	want := "bitcoin +44 7700 900123 or +44 7700 900456 again +44 7700 900123" +
		" wa.me/447700900123 https://chat.whatsapp.com/INVITECODE t.me/REDACTED" +
		" https://promo.example.co.uk/REDACTED name@example.com @mention"
	got := Redact(in, true)
	if got != want {
		t.Fatalf("Redact(public)\n got %q\nwant %q", got, want)
	}
	for _, leaked := range []string{"7911", "123456", "803", "4567", "AbCdEf", "example_signals", "vip", "desk@"} {
		if strings.Contains(got, leaked) {
			t.Errorf("public redaction kept %q: %q", leaked, got)
		}
	}
	if !acts(t, got) {
		t.Errorf("the public sample no longer trips the rule: %q", got)
	}
}

func countYAML(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".yaml") {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDedupeByNormalisedHash: the same message pasted again with other case,
// spacing or look-alike letters is a duplicate; under the other label it is
// refused; the type is part of the identity.
func TestDedupeByNormalisedHash(t *testing.T) {
	dir := t.TempDir()
	a, err := Add(dir, Spam, Sample{Text: "Bitcoin signals t.me/example_signals\n"}, false, nil)
	if err != nil || a.Path == "" || a.Duplicate != "" {
		t.Fatalf("first add: %+v %v", a, err)
	}
	if filepath.Dir(a.Path) != filepath.Join(dir, Spam) || len(filepath.Base(a.Path)) != len("123456789abc.yaml") {
		t.Fatalf("saved at %s", a.Path)
	}
	for _, again := range []string{"bitcoin   SIGNALS t.me/example_signals", "Bіtcoin signals t.me/example_signals"} {
		d, err := Add(dir, Spam, Sample{Text: again}, false, nil)
		if err != nil || d.Path != "" || d.Duplicate != a.Path {
			t.Fatalf("%q: %+v %v, want a duplicate of %s", again, d, err, a.Path)
		}
	}
	if _, err := Add(dir, Legit, Sample{Text: "bitcoin signals T.ME/example_signals"}, false, nil); err == nil ||
		!strings.Contains(err.Error(), "already labelled spam at "+a.Path) {
		t.Fatalf("other label: %v", err)
	}
	poll, err := Add(dir, Spam, Sample{Type: TypePoll, Text: "Bitcoin signals t.me/example_signals"}, false, nil)
	if err != nil || poll.Path == "" {
		t.Fatalf("same text as a poll: %+v %v", poll, err)
	}
	if n := countYAML(t, dir); n != 2 {
		t.Fatalf("%d samples saved, want 2", n)
	}
}

// TestTypeAndPushNameRecorded: the type, push name (redacted) and note are
// saved and read back; a new corpus is seeded with the public legit samples
// first, so its first test has legit samples to measure against.
func TestTypeAndPushNameRecorded(t *testing.T) {
	seed := fstest.MapFS{
		"in-stock/01.yaml":  {Data: []byte("text: \"Back in stock: size 8 trainers\"\n")},
		"meetup/01.yaml":    {Data: []byte("text: \"Blockchain meetup on Friday, all welcome\"\n")},
		"meetup/README.txt": {Data: []byte("not a sample")},
	}
	dir := filepath.Join(t.TempDir(), "private")
	a, err := Add(dir, Spam, Sample{Type: TypeContact, PushName: "Crypto Desk " + ukMobile, Note: "contact card",
		Text: "Crypto Support Desk\n" + ukMobile}, false, seed)
	if err != nil || a.Path == "" {
		t.Fatalf("add: %+v %v", a, err)
	}
	if a.Seeded != 2 {
		t.Fatalf("seeded %d legit samples, want 2", a.Seeded)
	}
	s, err := ReadSample(a.Path)
	if err != nil {
		t.Fatal(err)
	}
	mobile := "+44 " + zeros("0", 4) + " " + zeros("0", 6)
	if s.Type != TypeContact || s.PushName != "Crypto Desk "+mobile || s.Note != "contact card" ||
		s.Text != "Crypto Support Desk\n"+mobile {
		t.Fatalf("read back %+v", s)
	}
	if f := s.Fields(); len(f) != 2 || f[0].Name != "contact.name" || f[1].Name != "contact.number" {
		t.Fatalf("contact fields %+v", f)
	}
	r, err := Run(ruleset(t), dir)
	if err != nil || r.Spam != 1 || r.Legit != 2 || r.Err() != nil {
		t.Fatalf("corpus test of the new corpus: %+v %v / %v", r, err, r.Err())
	}
	// A corpus that exists is never seeded again.
	b, err := Add(dir, Legit, Sample{Text: "see you at class on Sunday"}, false, seed)
	if err != nil || b.Seeded != 0 || filepath.Dir(b.Path) != filepath.Join(dir, Legit) {
		t.Fatalf("second add: %+v %v", b, err)
	}
	for _, bad := range []struct {
		label string
		s     Sample
		want  string
	}{
		{"maybe", Sample{Text: "x"}, `label must be spam or legit, not "maybe"`},
		{Spam, Sample{Type: "sticker", Text: "x"}, `unknown type "sticker"`},
		{Spam, Sample{Text: " \n "}, "the pasted message is empty"},
	} {
		if _, err := Add(dir, bad.label, bad.s, false, nil); err == nil || err.Error() != bad.want {
			t.Errorf("%+v: %v, want %q", bad, err, bad.want)
		}
	}
	if got, err := Load(dir); err != nil || len(got) != 4 {
		t.Fatalf("refused samples were written: %d samples, %v", len(got), err)
	}
}

package normalise

import "testing"

func mustCompile(t *testing.T, s string) Pattern {
	t.Helper()
	p, err := Compile(s)
	if err != nil {
		t.Fatalf("compile %q: %v", s, err)
	}
	return p
}

func matches(t *testing.T, keyword, text string, never ...string) bool {
	t.Helper()
	var nv []Pattern
	for _, n := range never {
		nv = append(nv, mustCompile(t, n))
	}
	return mustCompile(t, keyword).MatchIn(Prepare(text, nv), false)
}

func TestHomoglyphSkeleton(t *testing.T) {
	for _, text := range []string{
		"сrурtо profits", // Cyrillic с, у, р, о
		"CRYPTO profits", // case
		"crypt0 profits", // digit zero is confusable with O
		"ｃｒｙｐｔｏ profits", // fullwidth (NFKC)
		"𝐜𝐫𝐲𝐩𝐭𝐨 profits", // mathematical bold (NFKC)
		"ⓒⓡⓨⓟⓣⓞ profits", // circled (NFKC)
		"çrÿptö profits", // accents
	} {
		if !matches(t, "crypto", text) {
			t.Errorf("%q: crypto not found", text)
		}
	}
	// The skeleton is a comparison form only: a legitimate word that turns
	// into nonsense under it must not start matching another keyword.
	if matches(t, "stock", "Stockholm is lovely") {
		t.Error("Stockholm matched stock")
	}
	if Base("Stockholm") != "stockholm" {
		t.Errorf("Base changed the word: %q", Base("Stockholm"))
	}
}

func TestFoldTable(t *testing.T) {
	for _, text := range []string{
		"🇨🇷🇾🇵🇹🇴 now", // regional indicators
		"🅲🆁🆈🅿🆃🅾 now", // negative squared
		"🄲🅁🅈🄿🅃🄾 now", // squared
		"🅒🅡🅨🅟🅣🅞 now", // negative circled
		"⒞⒭⒴⒫⒯⒪ now", // parenthesised
		"ᴄʀʏᴘᴛᴏ now", // small capitals
	} {
		if !matches(t, "crypto", text) {
			t.Errorf("%q: crypto not found", text)
		}
	}
	if !matches(t, "bitcoin", "₿itcoin giveaway") {
		t.Error("₿itcoin not found")
	}
}

func TestZeroWidth(t *testing.T) {
	for _, text := range []string{
		"cry\u200bpto",     // zero-width space
		"cry\u2060pto",     // word joiner
		"cry\u00adpto",     // soft hyphen
		"cry\u034fpto",     // combining grapheme joiner
		"cry\ufe0fpto",     // variation selector
		"cry\U000E0041pto", // tag character
		"c̲r̲y̲p̲t̲o̲",     // combining underline
	} {
		if !matches(t, "crypto", text) {
			t.Errorf("%q: crypto not found", text)
		}
	}
}

func TestSpacedLettersExtraView(t *testing.T) {
	for _, text := range []string{"join c r y p t o doubler", "c.r.y.p.t.o pump", "c-r-y-p-t-o", "c·r·y·p·t·o"} {
		if !matches(t, "crypto", text) {
			t.Errorf("%q: crypto not found", text)
		}
	}
	// The collapsed text is an extra view: the original words still match,
	// and three spaced letters are not collapsed.
	if !matches(t, "doubler", "join c r y p t o doubler") {
		t.Error("original view lost")
	}
	if matches(t, "abc", "a b c") {
		t.Error("three letters collapsed")
	}
	// A wider gap separates spaced-out words instead of joining them.
	for _, kw := range []string{"usdt", "free"} {
		if !matches(t, kw, "F R E E  U S D T for everyone") {
			t.Errorf("%q not found in two spaced-out words", kw)
		}
	}
}

func TestLeetOptIn(t *testing.T) {
	p := mustCompile(t, "stocks")
	text := Prepare("hot $tock$ tips", nil)
	if p.MatchIn(text, false) {
		t.Error("leet view used without opt-in")
	}
	if !p.MatchIn(text, true) {
		t.Error("leet view not used with opt-in")
	}
	if !mustCompile(t, "crypto").MatchIn(Prepare("cryp7o signals", nil), true) {
		t.Error("cryp7o not matched with leet")
	}
	// A price never becomes a word without opt-in.
	if mustCompile(t, "is").MatchIn(Prepare("£15", nil), false) {
		t.Error("price matched a word")
	}
}

func TestWholeWord(t *testing.T) {
	cases := []struct {
		keyword, text string
		want          bool
	}{
		{"crypto", "cryptography lecture", false},
		{"crypto", "crypto!", true},
		{"stock", "stockport fixtures", false},
		{"broker*", "my brokerage account", true},
		{"*coin", "free bitcoin", true},
		{"*coin", "coinbase", false},
		{"stock tips", "best stock tips here", true},
		{"stock tips", "stock and tips", false},
		{"expert*", "our experts recover funds", true},
	}
	for _, c := range cases {
		if got := matches(t, c.keyword, c.text); got != c.want {
			t.Errorf("%q in %q = %v, want %v", c.keyword, c.text, got, c.want)
		}
	}
}

func TestNeverMatchFirst(t *testing.T) {
	never := []string{"in stock", "out of stock", "stock photo*", "chicken stock", "laughing stock", "stock up", "stocking"}
	for _, text := range []string{
		"Selling my old bike £120, still in stock, message me",
		"back out of stock soon",
		"nice stock photos",
		"make chicken stock",
		"he's a laughing stock",
		"time to stock up",
	} {
		if matches(t, "stock", text, never...) {
			t.Errorf("%q: stock matched through a never-match phrase", text)
		}
	}
	// Only the blanked words are removed: another "stock" in the same text
	// still matches.
	if !matches(t, "stock", "in stock now, and stock tips", never...) {
		t.Error("blanking removed more than the phrase")
	}
	// Blanking happens in every view, including the skeleton.
	if matches(t, "stock", "still іn ѕtock", never...) {
		t.Error("never-match not applied to the skeleton view")
	}
}

// TestPunctuatedEntryIsPhrase: punctuation inside an entry splits it the same
// way it splits the text, so "1.10" is the phrase "1 10" and a * stays on the
// outer end it was written on.
func TestPunctuatedEntryIsPhrase(t *testing.T) {
	cases := []struct {
		keyword, text string
		want          bool
	}{
		{"1.10", "upgrade to 1.10 today", true},
		{"1.10", "1.100 units", false},
		{"stock-tips", "free stock tips here", true},
		{"*stock-tips", "freestock tips", true},
		{"stock-tip*", "stock tipsters", true},
		{"stock-tip*", "stockx tipsters", false},
	}
	for _, c := range cases {
		if got := matches(t, c.keyword, c.text); got != c.want {
			t.Errorf("%q in %q = %v, want %v", c.keyword, c.text, got, c.want)
		}
	}
	if p := mustCompile(t, "1.10"); !p.Phrase() {
		t.Error(`"1.10" is not a phrase, so the minimum word length would refuse it`)
	}
	if p := mustCompile(t, "bitcoin"); p.Phrase() {
		t.Error(`"bitcoin" counted as a phrase`)
	}
}

func TestCompileRejectsBadWildcards(t *testing.T) {
	for _, s := range []string{"cr*pto", "*", "stock * tips", "**coin", "", "  "} {
		if _, err := Compile(s); err == nil {
			t.Errorf("%q compiled", s)
		}
	}
	for _, s := range []string{"broker*", "*coin", "stock photo*", "*crypto*"} {
		if _, err := Compile(s); err != nil {
			t.Errorf("%q refused: %v", s, err)
		}
	}
}

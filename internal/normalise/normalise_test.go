package normalise

import (
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"
)

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

// collapseWhole is the spaced-letter join applied to a whole string: the
// reference the traced views are checked against.
func collapseWhole(s string) (string, bool) {
	idx := spaced.FindAllStringSubmatchIndex(s, -1)
	if len(idx) == 0 {
		return s, false
	}
	var b strings.Builder
	last := 0
	for _, m := range idx {
		b.WriteString(s[last:m[2]])
		b.WriteString(spacer.ReplaceAllString(s[m[2]:m[3]], ""))
		last = m[3]
	}
	b.WriteString(s[last:])
	return b.String(), true
}

// wholeTextSamples are texts the traced readings are checked against the
// whole text with: look-alike and folded letters, invisible characters,
// spaced letters, digit swaps, and letters a normal form merges across an
// invisible character or a dropped accent.
var wholeTextSamples = []string{
	"сrурtо profits", "CRYPTO profits", "ｃｒｙｐｔｏ profits", "çrÿptö profits",
	"🇨🇷🇾🇵🇹🇴 now", "ᴄʀʏᴘᴛᴏ now", "₿itcoin giveaway", "cry\u200bpto", "c̲r̲y̲p̲t̲o̲",
	"c r y p t o dm me", "c.r.y.p.t.o", "F R E E  U S D T", "b u y\tb t c",
	"$tocks risk free", "e@sy m0ney", "b1tcoin d0gecoin", "anyone still holding DOGECOIN?",
	"price\u00a0list 10\u3000days", "bit¨coin", "  leading and trailing  ", "", "Stockholm is lovely",
	"\u1100\u00ad\u1161", "\u1100\u200b\u1161 coin", "\u3260\u1161", "\u3131\u200b\u1161",
	"\u1100 \u1161 \u1100 \u1161", "\u1100\u0301\u1161", "\u110f\u00ad\u1169\uc778 dm me",
	"crypto\u01c3dm me", "=\u0338 \u2260", "\u09c7\u200b\u09be", "e\u0301 \u00e9", "\ufb01nance \u2460\u2461",
	"z" + strings.Repeat("\u0301", 45) + "\u0323x crypto",
}

// wholeText is every reading the whole text gives: base, look-alike letters
// and digit swaps of the text, and the same of its spaced-letter join.
func wholeText(s string) map[string]bool {
	want := map[string]bool{
		fmt.Sprint(tokens(base(s))): true, fmt.Sprint(tokens(skeleton(s))): true,
		fmt.Sprint(tokens(leet.Replace(base(s)))): true,
	}
	if c, ok := collapseWhole(base(s)); ok {
		want[fmt.Sprint(tokens(c))] = true
		want[fmt.Sprint(tokens(skeleton(c)))] = true
		want[fmt.Sprint(tokens(leet.Replace(c)))] = true
	}
	return want
}

// readsWholeText reports in t unless the readings Prepare builds, tracing
// each character, are exactly the readings of the whole text.
func readsWholeText(t *testing.T, s string) {
	t.Helper()
	var source traced
	for i, c := range []rune(s) {
		source.runes, source.src = append(source.runes, c), append(source.src, i)
	}
	r := newReader(len(source.runes))
	if got := string(r.base(source).runes); got != base(s) {
		t.Errorf("%+q: base reading %+q, the whole text gives %+q", s, got, base(s))
	}
	if got := string(r.skeleton(source).runes); got != skeleton(s) {
		t.Errorf("%+q: look-alike reading %+q, the whole text gives %+q", s, got, skeleton(s))
	}
	want := wholeText(s)
	got := map[string]bool{}
	for _, v := range Prepare(s, nil).views {
		got[fmt.Sprint(v.words)] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("%+q: no reading gives %+q", s, w)
		}
	}
	for w := range got {
		if !want[w] {
			t.Errorf("%+q: a reading gives %+q, the whole text does not", s, w)
		}
	}
}

// TestViewsReadTheWholeText: building every reading character by character
// (to know which characters each match came from) reads exactly the words the
// whole text gives, so no keyword matches more or less than before.
func TestViewsReadTheWholeText(t *testing.T) {
	for _, s := range wholeTextSamples {
		readsWholeText(t, s)
	}
}

// TestComposedLettersReadAsTheWholeText: every letter Unicode composes from
// two parts (every one below U+30000; Hangul: every syllable without a final
// consonant and every final consonant once), written as its parts with
// nothing, an invisible character or an accent the matcher drops between
// them, reads exactly as the whole text does.
func TestComposedLettersReadAsTheWholeText(t *testing.T) {
	var pairs [][2]string
	for c := rune(0); c < 0x30000; c++ {
		if c >= 0xD800 && c < 0xE000 || c > 0xAC1B && c <= 0xD7A3 && (c-0xAC00)%28 != 0 {
			continue
		}
		d := []rune(norm.NFD.String(string(c)))
		if len(d) < 2 || norm.NFC.String(string(d)) != string(c) {
			continue
		}
		pairs = append(pairs, [2]string{norm.NFC.String(string(d[:len(d)-1])), string(d[len(d)-1])})
	}
	if len(pairs) < 1000 {
		t.Fatalf("premise: only %d composed letters found", len(pairs))
	}
	// compatibility letters NFKC turns into a part that composes
	pairs = append(pairs, [2]string{"\u3260", "\u1161"}, [2]string{"\u3131", "\u1161"}, [2]string{"\uffa1", "\u1161"})
	for _, p := range pairs {
		for _, between := range []string{"", "\u00ad", "\u200b", "\u034f", "\u2060", "\ufeff", "\u0301"} {
			readsWholeText(t, p[0]+between+p[1])
		}
		if t.Failed() {
			return
		}
	}
}

// FuzzViewsReadTheWholeText runs the samples under go test; go test -fuzz
// looks for a text whose traced readings differ from the whole text's.
func FuzzViewsReadTheWholeText(f *testing.F) {
	for _, s := range wholeTextSamples {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		readsWholeText(t, s)
	})
}

// TestMergedLettersAreOneCharacter: characters a normal form merges ("ᄀ"
// and "ᅡ" into "가", here with a soft hyphen between them) count as one
// character of the text in every reading.
func TestMergedLettersAreOneCharacter(t *testing.T) {
	text := Prepare("\u1100\u00ad\u1161 \u1100", nil)
	if text.one[0] != text.one[1] || text.one[0] != text.one[2] {
		t.Fatalf("merged characters %v are not one", text.one[:3])
	}
	if text.one[3] == text.one[0] || text.one[4] == text.one[0] {
		t.Fatalf("characters after the merged letter joined it: %v", text.one)
	}
}

// usedRuns is the text's characters a Words set holds, as the runs they
// make in the original text.
func usedRuns(text Text, s string, used map[int]bool) []string {
	var runs []string
	run := ""
	for i, c := range []rune(s) {
		if used[text.one[i]] {
			run += string(c)
			continue
		}
		if run != "" {
			runs, run = append(runs, run), ""
		}
	}
	if run != "" {
		runs = append(runs, run)
	}
	return runs
}

// TestWordsAcrossReadings: a keyword uses the same characters of the text
// whichever reading it matched in, so a second keyword on the same word is
// never counted as a different word, while words split only by punctuation,
// or by a letter the look-alike reading turns into punctuation, stay
// different words.
func TestWordsAcrossReadings(t *testing.T) {
	cases := []struct {
		text, first, second string
		leet                bool
		used                []string
		secondElsewhere     bool
	}{
		// capitals: the look-alike reading of DOGECOIN is "dogecoln"
		{"anyone still holding DOGECOIN?", "*coin", "doge*", false, []string{"DOGECOIN"}, false},
		{"anyone still holding dogecoin?", "*coin", "doge*", false, []string{"dogecoin"}, false},
		// a digit swap read only by the second list
		{"anyone still holding b1tcoin?", "*coin", "bitcoin", true, []string{"b1tcoin"}, false},
		// spaced letters span every letter and space they came from
		{"c r y p t o dm me", "crypto", "dm me", false, []string{"c r y p t o"}, true},
		// a joined word overlaps every letter it was made from, not just the first
		{"b i t c o i n", "c", "bitcoin", false, []string{"c"}, false},
		{"cryp7o dm me", "crypto", "dm me", true, []string{"cryp7o"}, true},
		{"$tocks risk free", "stocks", "risk free", true, []string{"$tocks"}, true},
		{"сrурtо crypto", "crypto", "crypto", false, []string{"сrурtо", "crypto"}, false},
		// punctuation with no space still separates two words
		{"crypto,dm me", "crypto", "dm me", false, []string{"crypto"}, true},
		{"bitcoin/guaranteed", "bitcoin", "guaranteed", false, []string{"bitcoin"}, true},
		// a "*" stops at a break the plain reading shows, though the look-alike reading reads "|" as "l"
		{"daily profit|bitcoin", "daily profit*", "*coin", false, []string{"daily", "profit"}, true},
		{"daily profitxbitcoin", "daily profit*", "*coin", false, []string{"daily", "profitxbitcoin"}, false},
		// a "*" at both ends reaches both ways, up to the break
		{"crypto|bitcoin", "*ypt*", "*coin", false, []string{"crypto"}, true},
		{"cryptocoin", "*ypt*", "*coin", false, []string{"cryptocoin"}, false},
		// the look-alike reading of the click letter is "!", so it splits words there
		{"crypto\u01c3dm me", "crypto", "dm me", false, []string{"crypto"}, true},
		// letters merged across a soft hyphen are one word, read as the whole text reads it
		{"\u110f\u00ad\u1169\uc778 dm me", "\ucf54\uc778", "\ucf54\uc778", false, []string{"\u110f\u00ad\u1169\uc778"}, false},
	}
	for _, c := range cases {
		text := Prepare(c.text, nil)
		used := map[int]bool{}
		mustCompile(t, c.first).Words(text, c.leet, used)
		if got := usedRuns(text, c.text, used); !reflect.DeepEqual(got, c.used) {
			t.Errorf("%+q: %+q uses %+q, want %+q", c.text, c.first, got, c.used)
		}
		second := mustCompile(t, c.second)
		if !second.MatchIn(text, c.leet) {
			t.Errorf("%+q: %+q does not match at all", c.text, c.second)
		}
		if got := second.MatchAvoiding(text, c.leet, used); got != c.secondElsewhere {
			t.Errorf("%+q: %+q away from %+q's words = %v, want %v", c.text, c.second, c.first, got, c.secondElsewhere)
		}
	}
}

// apart is the rules package's lure check on one text: one pattern matches
// characters the other matches in no reading.
func apart(text Text, a, b Pattern) bool {
	for _, x := range [][2]Pattern{{a, b}, {b, a}} {
		used := map[int]bool{}
		x[0].Words(text, false, used)
		if x[1].MatchAvoiding(text, false, used) {
			return true
		}
	}
	return false
}

// TestWildcardsStopAtABreak: with a "*" in each pattern, a character put
// between the two words makes them different words exactly when the whole
// text, read plainly or with look-alike letters, shows it as a space or
// punctuation there. So a real lure ("daily profit|bitcoin") acts, and one
// word ("dogecoin" with a letter in between) never does. Checked for every
// character below U+30000 the two readings disagree on ("|" is punctuation
// plainly and "l" with look-alike letters; U+01C3 the other way round), and
// for every 101st other character as a control.
func TestWildcardsStopAtABreak(t *testing.T) {
	shapes := []struct{ left, right, a, b string }{
		{"daily profit", "bitcoin", "daily profit*", "*coin"},
		{"doge", "coin", "doge*", "*coin"},
	}
	const end, chunk = 0x30000, 0x2000
	for _, sh := range shapes {
		a, b := mustCompile(t, sh.a), mustCompile(t, sh.b)
		l, r := []rune(sh.left), []rune(sh.right)
		var disagree, controls atomic.Int64
		t.Run(sh.left+sh.right, func(t *testing.T) {
			for lo := rune(1); lo < end; lo += chunk {
				t.Run(fmt.Sprintf("U+%05X", lo), func(t *testing.T) {
					t.Parallel()
					for c := lo; c < min(lo+chunk, end); c++ {
						if c >= 0xD800 && c < 0xE000 {
							continue
						}
						// only the letters either side can join with c in a reading
						around := string([]rune{l[len(l)-1], c, r[0]})
						plainSplit, lookSplit := len(tokens(base(around))) > 1, len(tokens(skeleton(around))) > 1
						if plainSplit != lookSplit {
							disagree.Add(1)
						} else if c%101 != 0 {
							continue
						} else {
							controls.Add(1)
						}
						s := sh.left + string(c) + sh.right
						text := Prepare(s, nil)
						want := (plainSplit || lookSplit) && a.MatchIn(text, false) && b.MatchIn(text, false)
						if got := apart(text, a, b); got != want {
							t.Fatalf("%+q: %q and %q on different words = %v, want %v", s, sh.a, sh.b, got, want)
						}
					}
				})
			}
		})
		// premise: the readings disagree on hundreds of characters ("|", U+01C3)
		if disagree.Load() < 500 || controls.Load() < 1000 {
			t.Fatalf("premise: %q: %d characters the readings disagree on, %d controls", sh.left+sh.right, disagree.Load(), controls.Load())
		}
		t.Logf("%q: %d characters the readings disagree on, %d controls", sh.left+sh.right, disagree.Load(), controls.Load())
	}
}

// TestLongWordStaysFast: a word that repeats the letters a "*" entry names
// ("*coin*" on "coincoincoin...") costs about its length, not repeats times
// length, so one long post cannot hold up every message behind it. Matching
// must take less time than reading the post (Prepare, which is linear), so a
// slow or busy machine slows both alike. Measured on this 64,013-character
// post over ten runs: reading 180-400 ms, matching 26-66 ms (with -race 1.7-2.3 s
// and 133-268 ms); walking each repeat out to the break again took 4.4 s, and
// the code before the fix took minutes.
func TestLongWordStaysFast(t *testing.T) {
	coin, lure := mustCompile(t, "*coin*"), mustCompile(t, "daily profit*")
	text := "daily profit " + strings.Repeat("coin", 16000)
	start := time.Now()
	tx := Prepare(text, nil)
	read := time.Since(start)
	done := make(chan bool, 1)
	start = time.Now()
	go func() {
		used := map[int]bool{}
		coin.Words(tx, false, used)
		done <- lure.MatchAvoiding(tx, false, used)
	}()
	select {
	case apart := <-done:
		took := time.Since(start)
		if !apart {
			t.Fatal(`"daily profit" and the long "coin" word read as one word`)
		}
		if took > read {
			t.Fatalf("matching a %d-character post took %v, longer than reading it (%v)", len(text), took, read)
		}
		t.Logf("read a %d-character post in %v, matched it in %v", len(text), read, took)
	case <-time.After(time.Minute):
		t.Fatalf("matching a %d-character post took over a minute", len(text))
	}
}

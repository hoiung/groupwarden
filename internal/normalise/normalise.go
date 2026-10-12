// Package normalise prepares message text and keywords for matching.
//
// It produces matching VIEWS only. The original text is never changed: it is
// kept for evidence and display, because some views (the UTS #39 skeleton
// above all) turn ordinary words into nonsense ("Stockholm" → "stockholrn").
// A keyword matches when its own view of the same kind occurs, as whole
// words, in any view of the text.
package normalise

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/eskriett/confusables"
	"golang.org/x/text/cases"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// fold maps letter look-alikes that NFKC and the confusables skeleton leave
// alone: regional-indicator, squared, negative-squared, negative-circled and
// parenthesised letters, small capitals, and the bitcoin sign.
func fold(r rune) rune {
	switch {
	case r >= 0x1F1E6 && r <= 0x1F1FF: // regional indicator symbols a-z
		return 'a' + (r - 0x1F1E6)
	case r >= 0x1F130 && r <= 0x1F149: // squared latin capitals
		return 'a' + (r - 0x1F130)
	case r >= 0x1F150 && r <= 0x1F169: // negative circled latin capitals
		return 'a' + (r - 0x1F150)
	case r >= 0x1F170 && r <= 0x1F189: // negative squared latin capitals
		return 'a' + (r - 0x1F170)
	case r >= 0x249C && r <= 0x24B5: // parenthesised small letters
		return 'a' + (r - 0x249C)
	case r == 0x20BF: // bitcoin sign
		return 'b'
	}
	if f, ok := smallCaps[r]; ok {
		return f
	}
	return r
}

var smallCaps = map[rune]rune{
	'ᴀ': 'a', 'ʙ': 'b', 'ᴄ': 'c', 'ᴅ': 'd', 'ᴇ': 'e', 'ꜰ': 'f', 'ɢ': 'g', 'ʜ': 'h', 'ɪ': 'i',
	'ᴊ': 'j', 'ᴋ': 'k', 'ʟ': 'l', 'ᴍ': 'm', 'ɴ': 'n', 'ᴏ': 'o', 'ᴘ': 'p', 'ꞯ': 'q', 'ʀ': 'r',
	'ꜱ': 's', 'ᴛ': 't', 'ᴜ': 'u', 'ᴠ': 'v', 'ᴡ': 'w', 'ʏ': 'y', 'ᴢ': 'z',
}

// defaultIgnorable is Unicode's Default_Ignorable_Code_Point property
// (DerivedCoreProperties.txt, Unicode 17.0): zero-width characters, the soft
// hyphen, the combining grapheme joiner, bidi controls, variation selectors
// and tag characters. They are invisible, so they are removed before matching.
var defaultIgnorable = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x00AD, Hi: 0x00AD, Stride: 1},
		{Lo: 0x034F, Hi: 0x034F, Stride: 1},
		{Lo: 0x061C, Hi: 0x061C, Stride: 1},
		{Lo: 0x115F, Hi: 0x1160, Stride: 1},
		{Lo: 0x17B4, Hi: 0x17B5, Stride: 1},
		{Lo: 0x180B, Hi: 0x180F, Stride: 1},
		{Lo: 0x200B, Hi: 0x200F, Stride: 1},
		{Lo: 0x202A, Hi: 0x202E, Stride: 1},
		{Lo: 0x2060, Hi: 0x206F, Stride: 1},
		{Lo: 0x3164, Hi: 0x3164, Stride: 1},
		{Lo: 0xFE00, Hi: 0xFE0F, Stride: 1},
		{Lo: 0xFEFF, Hi: 0xFEFF, Stride: 1},
		{Lo: 0xFFA0, Hi: 0xFFA0, Stride: 1},
		{Lo: 0xFFF0, Hi: 0xFFF8, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x1BCA0, Hi: 0x1BCA3, Stride: 1},
		{Lo: 0x1D173, Hi: 0x1D17A, Stride: 1},
		{Lo: 0xE0000, Hi: 0xE0FFF, Stride: 1},
	},
}

// leet is the opt-in symbol and digit map (per word list): it raises false
// hits on prices and times, so it never applies unless a list asks for it.
var leet = strings.NewReplacer("0", "o", "1", "i", "3", "e", "4", "a", "5", "s", "7", "t", "8", "b",
	"$", "s", "@", "a", "!", "i", "|", "l")

// spaced finds runs of four or more single letters each split by ONE space or
// . - _ * · ("c r y p t o", "c.r.y.p.t.o"). A wider gap ends the run, because
// that is how spaced-out words are kept apart: "F R E E  U S D T" is two words.
var spaced = regexp.MustCompile(`(?:^|[^\p{L}\p{N}])((?:\p{L}[\s.\-_*·]){3,}\p{L})(?:$|[^\p{L}\p{N}])`)

var spacer = regexp.MustCompile(`[\s.\-_*·]+`)

// base is the main matching view: fold table, NFKC, invisible characters
// removed, accents removed, case folded, and the ideographic full stops that
// NFKC keeps (。 ｡) turned into "." so hidden links are found.
func base(s string) string {
	s = strings.Map(fold, s)
	s = norm.NFKC.String(s)
	s = strings.Map(dropIgnorable, s)
	s = stripMarks(s)
	s = cases.Fold().String(s)
	return fullStops.Replace(s)
}

var fullStops = strings.NewReplacer("。", ".", "｡", ".")

// skeleton is the UTS #39 skeleton of the folded text (invisible characters
// removed first, NFD after mapping, as the algorithm defines), then case
// folded and stripped of marks so it compares like base.
func skeleton(s string) string {
	s = strings.Map(fold, s)
	s = norm.NFKC.String(s)
	s = strings.Map(dropIgnorable, s)
	s = norm.NFD.String(confusables.ToSkeleton(s))
	return cases.Fold().String(stripMarks(s))
}

func dropIgnorable(r rune) rune {
	if unicode.Is(defaultIgnorable, r) {
		return -1
	}
	return r
}

func stripMarks(s string) string {
	out, _, err := transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), s)
	if err != nil {
		return s // the transformers never fail on valid UTF-8 strings
	}
	return out
}

// tokens splits a view into words: runs of letters and digits.
func tokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// Base returns the main matching view of s (used to find links, phone
// numbers and other artefacts).
func Base(s string) string { return base(s) }

// Skeleton returns s with look-alike letters mapped to one form (the corpus
// uses it to spot the same message pasted twice).
func Skeleton(s string) string { return skeleton(s) }

// kind tells which form of a pattern a view is compared with.
type kind int

const (
	plain kind = iota
	skel
)

type view struct {
	kind  kind
	leet  bool
	words []string
	src   []span // the characters of the original text each word came from
	blank []bool // words inside a never-match phrase
}

// span is the range of characters of the original text (rune indexes) one
// word of a view came from. Every reading of a word maps back to the same
// characters, so two keywords can be told apart whichever reading each
// matched in, and "crypto,dm me" is still three words.
type span struct{ lo, hi int }

// Text is one piece of text prepared for matching. one maps each character
// of the original text to the character it became one with in a reading
// (see reader.form); characters with the same entry count as one.
type Text struct {
	views []view
	one   []int
}

// traced is a reading under construction: each rune with the index of the
// character of the original text it came from.
type traced struct {
	runes []rune
	src   []int
}

// reader builds the readings of one text, each the same steps as base or
// skeleton, so a reading has exactly the runes base or skeleton gives for the
// whole text. A step either converts each rune on its own (each) or is a
// Unicode normal form (form).
type reader struct {
	joined []int // characters a normal form made one: union-find parents
	fold   cases.Caser
}

func newReader(n int) *reader {
	r := &reader{joined: make([]int, n), fold: cases.Fold()}
	for i := range r.joined {
		r.joined[i] = i
	}
	return r
}

func (r *reader) root(i int) int {
	for r.joined[i] != i {
		r.joined[i] = r.joined[r.joined[i]]
		i = r.joined[i]
	}
	return i
}

func (r *reader) join(a, b int) {
	a, b = r.root(a), r.root(b)
	r.joined[max(a, b)] = min(a, b)
}

// mapped is f as strings.Map applies it: a negative result drops the rune.
func mapped(f func(rune) rune) func(rune) string {
	return func(c rune) string {
		if m := f(c); m >= 0 {
			return string(m)
		}
		return ""
	}
}

// each converts every rune on its own, keeping where it came from.
func each(t traced, conv func(rune) string) traced {
	var out traced
	for i, c := range t.runes {
		for _, o := range conv(c) {
			out.runes = append(out.runes, o)
			out.src = append(out.src, t.src[i])
		}
	}
	return out
}

// form applies a Unicode normal form one normalisation segment at a time, as
// the standard defines it, so the result is f of the whole reading. A segment
// can merge characters ("ᄀ" and "ᅡ" become "가", also when an invisible
// character stood between them), so the characters of a segment are joined.
func (r *reader) form(t traced, f norm.Form) traced {
	s := string(t.runes)
	starts := make([]int, 0, len(t.runes))
	for off := range s {
		starts = append(starts, off)
	}
	var out traced
	var it norm.Iter
	it.InitString(f, s)
	k := 0
	for !it.Done() {
		seg := string(it.Next())
		first := k
		for k < len(starts) && starts[k] < it.Pos() {
			r.join(t.src[first], t.src[k])
			k++
		}
		if len(t.src) == 0 {
			continue
		}
		// first is past the end only if a segment consumed nothing; its output
		// then goes with the last rune.
		id := t.src[min(first, len(t.src)-1)]
		for _, o := range seg {
			out.runes = append(out.runes, o)
			out.src = append(out.src, id)
		}
	}
	return out
}

func (r *reader) stripMarks(t traced) traced {
	t = r.form(t, norm.NFD)
	t = each(t, mapped(func(c rune) rune {
		if unicode.Is(unicode.Mn, c) {
			return -1
		}
		return c
	}))
	return r.form(t, norm.NFC)
}

func (r *reader) caseFold(c rune) string { return r.fold.String(string(c)) }

// base is the traced base: the same steps in the same order.
func (r *reader) base(t traced) traced {
	t = each(t, mapped(fold))
	t = r.form(t, norm.NFKC)
	t = each(t, mapped(dropIgnorable))
	t = r.stripMarks(t)
	t = each(t, r.caseFold)
	return each(t, func(c rune) string { return fullStops.Replace(string(c)) })
}

// skeleton is the traced skeleton: the same steps in the same order
// (confusables.ToSkeleton is NFD, then each rune mapped on its own).
func (r *reader) skeleton(t traced) traced {
	t = each(t, mapped(fold))
	t = r.form(t, norm.NFKC)
	t = each(t, mapped(dropIgnorable))
	t = r.form(t, norm.NFD)
	t = each(t, func(c rune) string { return confusables.ToSkeleton(string(c)) })
	t = r.form(t, norm.NFD)
	t = r.stripMarks(t)
	return each(t, r.caseFold)
}

// collapseTraced joins spaced-out single letters, keeping where each letter
// came from: "c r y p t o" becomes one word spanning every letter and space
// it was made from. ok is false when there were none.
func collapseTraced(t traced) (traced, bool) {
	s := string(t.runes)
	idx := spaced.FindAllStringSubmatchIndex(s, -1)
	if len(idx) == 0 {
		return t, false
	}
	var out traced
	r, m := 0, 0
	for off, c := range s {
		inRun := m < len(idx) && off >= idx[m][2] && off < idx[m][3]
		if !inRun || !spacer.MatchString(string(c)) {
			out.runes = append(out.runes, c)
			out.src = append(out.src, t.src[r])
		}
		r++
		if m < len(idx) && off+utf8.RuneLen(c) >= idx[m][3] {
			m++
		}
	}
	return out, true
}

// Prepare builds every matching view of s and blanks the never-match phrases
// in each, before any keyword is looked at.
func Prepare(s string, never []Pattern) Text {
	var source traced
	for _, c := range s {
		source.runes = append(source.runes, c)
		source.src = append(source.src, len(source.src))
	}
	r := newReader(len(source.runes))
	leetRune := func(c rune) string { return leet.Replace(string(c)) }
	b := r.base(source)
	var t Text
	seen := map[string]bool{}
	add := func(k kind, leetView bool, v traced) {
		key := fmt.Sprint(k, leetView, string(v.runes))
		if seen[key] {
			return
		}
		seen[key] = true
		vw := view{kind: k, leet: leetView}
		vw.words, vw.src = tokensTraced(v)
		vw.blank = make([]bool, len(vw.words))
		for _, p := range never {
			p.mark(&vw)
		}
		t.views = append(t.views, vw)
	}
	add(plain, false, b)
	add(skel, false, r.skeleton(source))
	add(plain, true, each(b, leetRune))
	if c, ok := collapseTraced(b); ok {
		add(plain, false, c)
		add(skel, false, r.skeleton(c))
		add(plain, true, each(c, leetRune))
	}
	t.one = make([]int, len(source.runes))
	for i := range t.one {
		t.one[i] = r.root(i)
	}
	return t
}

// tokensTraced is tokens on a traced view, with the characters of the
// original text each word came from.
func tokensTraced(t traced) (words []string, src []span) {
	for i := 0; i < len(t.runes); {
		if !isWordRune(t.runes[i]) {
			i++
			continue
		}
		j, sp := i, span{lo: t.src[i], hi: t.src[i]}
		for j < len(t.runes) && isWordRune(t.runes[j]) {
			sp.lo, sp.hi = min(sp.lo, t.src[j]), max(sp.hi, t.src[j])
			j++
		}
		words, src = append(words, string(t.runes[i:j])), append(src, sp)
		i = j
	}
	return words, src
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// part is one word of a pattern; Prefix/Suffix come from a "*" at its end or
// start ("broker*" matches "brokers", "*coin" matches "bitcoin").
type part struct {
	word         string
	prefix, suff bool
}

func (p part) matches(w string) bool {
	switch {
	case p.prefix && p.suff:
		return strings.Contains(w, p.word)
	case p.prefix:
		return strings.HasPrefix(w, p.word)
	case p.suff:
		return strings.HasSuffix(w, p.word)
	}
	return w == p.word
}

// Pattern is a compiled keyword, phrase or never-match entry.
type Pattern struct {
	Raw   string
	forms [2][]part // [plain], [skel]
}

// Compile checks an entry and builds its matching forms. A "*" is allowed
// only at the start or end of a word; it is never a whole word.
func Compile(entry string) (Pattern, error) {
	p := Pattern{Raw: entry}
	fields := strings.Fields(entry)
	if len(fields) == 0 {
		return p, fmt.Errorf("%q is empty", entry)
	}
	for _, f := range fields {
		core := strings.Trim(f, "*")
		if core == "" {
			return p, fmt.Errorf("%q: a * must be attached to a word", entry)
		}
		if strings.Contains(core, "*") {
			return p, fmt.Errorf("%q: a * may only be at the start or end of a word", entry)
		}
		if strings.HasPrefix(f, "**") || strings.HasSuffix(f, "**") {
			return p, fmt.Errorf("%q: use a single * at a word end", entry)
		}
		pre, suf := strings.HasSuffix(f, "*"), strings.HasPrefix(f, "*")
		for k, conv := range []func(string) string{base, skeleton} {
			ws := tokens(conv(core))
			if len(ws) == 0 {
				return p, fmt.Errorf("%q: %q has no letters or digits", entry, core)
			}
			// Punctuation splits a field the same way it splits the text, so
			// "1.10" or "stock-tips" become consecutive words of a phrase. A
			// leading * belongs to the first of them, a trailing * to the last.
			for i, w := range ws {
				p.forms[k] = append(p.forms[k], part{word: w, prefix: pre && i == len(ws)-1, suff: suf && i == 0})
			}
		}
	}
	return p, nil
}

// Phrase reports whether the entry has more than one word.
func (p Pattern) Phrase() bool { return len(p.forms[plain]) > 1 }

// Length is the number of letters and digits in a one-word entry.
func (p Pattern) Length() int {
	n := 0
	for _, pt := range p.forms[plain] {
		n += len([]rune(pt.word))
	}
	return n
}

// at reports whether the pattern's words match v starting at word i, none of
// them blanked.
func (p Pattern) at(v *view, i int) bool {
	parts := p.forms[v.kind]
	if i+len(parts) > len(v.words) {
		return false
	}
	for j, pt := range parts {
		if v.blank[i+j] || !pt.matches(v.words[i+j]) {
			return false
		}
	}
	return true
}

func (p Pattern) mark(v *view) {
	n := len(p.forms[v.kind])
	for i := range v.words {
		if p.at(v, i) {
			for j := i; j < i+n; j++ {
				v.blank[j] = true
			}
		}
	}
}

// MatchIn reports whether the pattern occurs as whole words in any view of t.
// Leet views are searched only when useLeet is set (an opt-in word list).
func (p Pattern) MatchIn(t Text, useLeet bool) bool {
	return p.MatchAvoiding(t, useLeet, nil)
}

// MatchAvoiding is MatchIn using none of the characters in avoid (as Words
// records them).
func (p Pattern) MatchAvoiding(t Text, useLeet bool, avoid map[int]bool) bool {
	for i := range t.views {
		v := &t.views[i]
		if v.leet && !useLeet {
			continue
		}
		n := len(p.forms[v.kind])
		for w := range v.words {
			if p.at(v, w) && !t.uses(v, w, w+n, avoid) {
				return true
			}
		}
	}
	return false
}

// Words adds to used every character of the original text the pattern
// matches in any view of t it may search.
func (p Pattern) Words(t Text, useLeet bool, used map[int]bool) {
	for i := range t.views {
		v := &t.views[i]
		if v.leet && !useLeet {
			continue
		}
		n := len(p.forms[v.kind])
		for w := range v.words {
			if !p.at(v, w) {
				continue
			}
			for _, sp := range v.src[w : w+n] {
				for s := sp.lo; s <= sp.hi; s++ {
					used[t.one[s]] = true
				}
			}
		}
	}
}

// uses reports whether words [from, to) of v came from any character in set.
func (t Text) uses(v *view, from, to int, set map[int]bool) bool {
	for _, sp := range v.src[from:to] {
		for s := sp.lo; s <= sp.hi; s++ {
			if set[t.one[s]] {
				return true
			}
		}
	}
	return false
}

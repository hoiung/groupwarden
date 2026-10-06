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
	return strings.NewReplacer("。", ".", "｡", ".").Replace(s)
}

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

// collapse joins spaced-out single letters; ok is false when there were none.
func collapse(s string) (string, bool) {
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

// tokens splits a view into words: runs of letters and digits.
func tokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// Base returns the main matching view of s (used to find links, phone
// numbers and other artefacts).
func Base(s string) string { return base(s) }

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
	blank []bool // words inside a never-match phrase
}

// Text is one piece of text prepared for matching.
type Text struct {
	views []view
}

// Prepare builds every matching view of s and blanks the never-match phrases
// in each, before any keyword is looked at.
func Prepare(s string, never []Pattern) Text {
	b := base(s)
	var t Text
	seen := map[string]bool{}
	add := func(k kind, leetView bool, v string) {
		key := fmt.Sprint(k, leetView, v)
		if seen[key] {
			return
		}
		seen[key] = true
		vw := view{kind: k, leet: leetView, words: tokens(v)}
		vw.blank = make([]bool, len(vw.words))
		for _, p := range never {
			p.mark(&vw)
		}
		t.views = append(t.views, vw)
	}
	add(plain, false, b)
	add(skel, false, skeleton(s))
	add(plain, true, leet.Replace(b))
	if c, ok := collapse(b); ok {
		add(plain, false, c)
		add(skel, false, skeleton(c))
		add(plain, true, leet.Replace(c))
	}
	return t
}

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
	for i := range t.views {
		v := &t.views[i]
		if v.leet && !useLeet {
			continue
		}
		for w := range v.words {
			if p.at(v, w) {
				return true
			}
		}
	}
	return false
}

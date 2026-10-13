package rules

import (
	"bufio"
	"bytes"
	_ "embed" // the bundled shortener list
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"
	"mvdan.cc/xurls/v2"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/normalise"
)

// Built-in conditions a rule can name under `has:`.
const (
	AnyLink     = "any_link"
	InviteLink  = "invite_link"
	Handle      = "handle"
	Shortener   = "shortener"
	PhoneNumber = "phone_number"
	ContactCard = "contact_card"
	MoneyAmount = "money_amount"
)

// Builtins lists every built-in condition.
var Builtins = []string{AnyLink, InviteLink, Handle, Shortener, PhoneNumber, ContactCard, MoneyAmount}

// artefacts are the link and contact conditions an acting rule needs beside
// a keyword ("keywords and those adding links combination"). money_amount
// is a signal, not an artefact.
var artefacts = map[string]bool{AnyLink: true, InviteLink: true, Shortener: true, PhoneNumber: true, ContactCard: true, Handle: true}

//go:embed data/shorteners.txt
var shortenerList []byte

var shorteners = func() map[string]bool {
	m := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(shortenerList))
	for sc.Scan() {
		if d := strings.TrimSpace(sc.Text()); d != "" && !strings.HasPrefix(d, "#") {
			m[strings.ToLower(d)] = true
		}
	}
	return m
}()

// Links are found with or without a scheme ("t.me/x", "bit.ly/x").
var (
	linkRe = xurls.Relaxed()
	tgRe   = regexp.MustCompile(`\btg://\S+`)
	// inviteRe: an invite host (inviteHosts), for a field with more
	// link-shaped matches than detect reads.
	inviteRe = regexp.MustCompile(`\b(?:chat\.whatsapp\.com|api\.whatsapp\.com|wa\.me|t\.me|telegram\.me|telegram\.dog)\b`)
	// handleRe: an "@username" not inside a word or an email address.
	handleRe = regexp.MustCompile(`(?:^|[^\p{L}\p{N}_@.])@(\p{L}[\p{L}\p{N}_.]{2,31})`)
	// phoneRe: a digit run with phone separators (group 1); IsPhone checks it.
	phoneRe = regexp.MustCompile(`(?:^|[^\p{L}\p{N}])(\+?\d[\d \t().\-]{7,24}\d)`)
	moneyRe = regexp.MustCompile(`[$£€¥₹₦₽]\s?\d[\d,]*(?:\.\d+)?|\b\d[\d,]*(?:\.\d+)?\s?(?:k|m)?\s?(?:usd|usdt|usdc|gbp|eur|dollars?|pounds?|euros?|btc|eth)\b`)
	// dateRe: a date written with dots, dashes or slashes ("07.11.2025",
	// "01-12-25"), perhaps followed by a time ("18:00", "19.30").
	dateRe = regexp.MustCompile(`\b\d{1,2}[./-]\d{1,2}[./-](?:\d{4}|\d{2})\b(?:[ \t]+\d{1,2}[:.]\d{2})?`)
	// decimalRe: a number that starts "0." or "0," ("0.00012345 BTC",
	// "+0.12345678%"); no phone number starts that way.
	decimalRe = regexp.MustCompile(`(?:^|[^\d.,])([+-]?0[.,]\d+)`)
)

// bareEndings are the domain endings a name written with nothing else
// ("cryptoprofits.com": no scheme, "www." or path) counts as a link with.
// Most of the 1,400 endings are also words, file endings or stock exchange
// codes: "Bitcoin is down again.Is it time to sell?", "5pm.So", "notes.zip"
// and "SAP.DE" are text, and the case-folded view cannot tell "again.Me"
// from "earnbig.me", so a bare name counts only with one of these, endings
// spam uses that are not words a sentence often starts with. Anything with
// a scheme, "www.", a path, a port or a query is a link whatever its ending.
var bareEndings = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(`com net org biz io co cc ly xyz top site online shop store icu cyou buzz bid loan website`) {
		m[e] = true
	}
	return m
}()

const (
	// linkWindow bounds how far one search for a link reads. The link
	// pattern keeps a search alive to the end of a run with no spaces
	// ("1.1.1.1…"), so searching the whole post once per link made one
	// 64,000-character post take over ten minutes. No host is longer than
	// 253 characters, so a link that starts in a window is recognised there;
	// one that runs past the window's end is taken to the next space.
	linkWindow  = 1024
	linkOverlap = 256
	// maxLinks is how many link-shaped matches one field is read for:
	// links, email addresses, and text such as "again.Is" or "1.1.1.1",
	// each of which costs a search. A field with more counts as having a
	// link (detect): no chat message holds that many.
	maxLinks = 64
)

// LinkSpans finds the links in s, and how many link-shaped matches it
// read: at most limit (limit < 0: all), the plain-text ones (notALink:
// "again.Is", "notes.zip") included.
func LinkSpans(s string, fileName bool, limit int) (spans [][2]int, read int) {
	pos := 0
	for pos < len(s) && (limit < 0 || read < limit) {
		end := windowEnd(s, pos)
		m := linkRe.FindStringIndex(s[pos:end])
		if m == nil {
			if end == len(s) {
				return spans, read
			}
			next := max(end-linkOverlap, pos+1)
			for next < end && !utf8.RuneStart(s[next]) {
				next++
			}
			pos = next
			continue
		}
		a, b := pos+m[0], pos+m[1]
		if cutShort(s, b, end) {
			if a > pos {
				// The window's end may have cut the link short ("https://chat"
				// of "https://chat.whatsapp.com/…" when its "." is the
				// window's last byte) or made one up ("dead.th" of "dead.The
				// end"): search again from its start, with a whole window
				// after it.
				pos = a
				continue
			}
			// A link longer than a window: take it to the next space, less
			// the punctuation a link never ends on.
			if i := strings.IndexFunc(s[b:], unicode.IsSpace); i >= 0 {
				b += i
			} else {
				b = len(s)
			}
			b = max(a+1, a+len(strings.TrimRight(s[a:b], `.,:;!?'"`)))
		}
		read++
		if !notALink(s[a:b], s[b:], fileName) {
			spans = append(spans, [2]int{a, b})
		}
		pos = b
	}
	return spans, read
}

// windowEnd is where a search from i stops: linkWindow bytes on, moved back
// to the start of a character (not moved in invalid UTF-8 with none).
func windowEnd(s string, i int) int {
	end := min(len(s), i+linkWindow)
	for e := end; e > i; e-- {
		if e == len(s) || utf8.RuneStart(s[e]) {
			return e
		}
	}
	return end
}

// cutShort reports whether a match ending at b may have been cut by a
// window ending at end: it ends at most a few bytes (the punctuation a link
// never ends on) before the window's end, with no space between.
func cutShort(s string, b, end int) bool {
	return end < len(s) && end-b <= 16 && strings.IndexFunc(s[b:end], unicode.IsSpace) < 0
}

// notALink reports whether a found link is plain text: written with no
// scheme, "www.", user name, port, path, fragment or query (the text going
// on with one, as in "again.is?ref=1", counts: the matcher stops before a
// query it cannot tell from a question mark), not a shortener or an invite
// host or a name under one ("x.t.me"), and either in a document's file name or
// ending in neither one of bareEndings nor a secondLevels ending such as
// "co.in".
func notALink(raw, after string, fileName bool) bool {
	low := strings.ToLower(raw)
	if strings.Contains(low, "://") || strings.HasPrefix(low, "www.") || strings.ContainsAny(low, "/?#:@") {
		return false
	}
	if len(after) > 0 && (after[0] == '/' || after[0] == '#') ||
		len(after) > 1 && (after[0] == '?' && isASCIIAlnum(after[1]) || after[0] == ':' && after[1] >= '0' && after[1] <= '9') {
		return false
	}
	host := strings.TrimSuffix(low, ".")
	if inviteHosts[host] || shorteners[host] || inviteHosts[registrable(host)] {
		return false
	}
	if fileName {
		return true
	}
	labels := strings.Split(host, ".")
	n := len(labels)
	last := labels[n-1]
	return !bareEndings[last] && (n < 3 || len(last) != 2 || !secondLevels[labels[n-2]])
}

// secondLevels make a two-part ending with a country code ("promo.co.in",
// "deals.com.my"), which no missing space after a full stop produces.
var secondLevels = map[string]bool{"co": true, "com": true, "net": true, "org": true}

// PhoneSpans finds the phone numbers in s: digit runs IsPhone accepts (from
// a later group starting "0" when the run's first digits are something else,
// as in "Room 4 0470 …") that are more than dates (onlyDates). A
// decimal that starts "0." ("0.00012345 BTC") is never part of one.
func PhoneSpans(s string) [][2]int {
	masked := []byte(s)
	for _, m := range decimalRe.FindAllStringSubmatchIndex(s, -1) {
		blank(masked, m[2], m[3])
	}
	dated := []byte(s)
	for _, m := range dateRe.FindAllStringIndex(s, -1) {
		// "06.12.34.56.78" goes on past the "date": a number written in pairs.
		if rest := s[m[1]:]; len(rest) < 2 || !strings.ContainsRune("./-", rune(rest[0])) || rest[1] < '0' || rest[1] > '9' {
			blank(dated, m[0], m[1])
		}
	}
	var spans [][2]int
	for _, m := range phoneRe.FindAllStringSubmatchIndex(string(masked), -1) {
		i, j := m[2], m[3]
		for i < j && !IsPhone(s[i:j]) {
			i = nextGroup(s, i, j)
		}
		if i < j && !onlyDates(dated[i:j]) {
			spans = append(spans, [2]int{i, j})
		}
	}
	return spans
}

// nextGroup is where the next digit group starting "0" after i begins in
// s[:j] (j when there is none).
func nextGroup(s string, i, j int) int {
	for k := i + 1; k < j; k++ {
		if s[k] == '0' && strings.IndexByte(" \t(", s[k-1]) >= 0 {
			return k
		}
	}
	return j
}

// onlyDates reports whether a digit run, with its dates blanked out, has
// too little left to be a phone number: no "+", no group of three or more
// digits and fewer than five digits (an event's text leaves at most a time
// or a day and month: "07.11.2025 18:00",
// "01.12.2025 - 05.12.2025", "08.30 07.11.2025"; a colon always ends a run,
// so "18:00 07.11.2025" leaves only "00"). A number written with its last
// digits like a date ("0470 12.11.23") keeps its "0470".
func onlyDates(run []byte) bool {
	digits, group, longest := 0, 0, 0
	for _, c := range run {
		switch {
		case c == '+':
			return false
		case c >= '0' && c <= '9':
			digits++
			group++
			longest = max(longest, group)
		default:
			group = 0
		}
	}
	return longest < 3 && digits < 5
}

func isASCIIAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// blank turns b[i:j] (ASCII digits and separators) into a letter, which no
// phone number crosses.
func blank(b []byte, i, j int) {
	for k := i; k < j; k++ {
		b[k] = 'x'
	}
}

// inviteHosts are WhatsApp and Telegram join or chat links.
var inviteHosts = map[string]bool{
	"chat.whatsapp.com": true, "wa.me": true, "api.whatsapp.com": true,
	"t.me": true, "telegram.me": true, "telegram.dog": true,
}

// signals is which built-in conditions one input shows.
type signals map[string]bool

// detect finds the built-in conditions in the fields' matching views.
// allowed holds registrable domains whose links never count.
func detect(fields []client.Field, allowed map[string]bool) signals {
	s := signals{}
	for _, f := range fields {
		if strings.HasPrefix(f.Name, "contact.") {
			s[ContactCard] = true
		}
		text := f.Match
		if text == "" {
			text = f.Text
		}
		v := normalise.Base(text)
		// The other detectors read the text with every link blanked out:
		// the digits of a job or event ID in a URL are not a phone number.
		rest := v
		links, read := LinkSpans(v, f.Name == "document.file_name", maxLinks+1)
		if read > maxLinks {
			// More than are read: the field has a link, and an invite host
			// anywhere in it still counts.
			s[AnyLink] = true
			if inviteRe.MatchString(v) {
				s[InviteLink] = true
			}
			links = links[:min(len(links), maxLinks)]
		}
		if len(links) > 0 {
			var b strings.Builder
			last := 0
			for _, l := range links {
				linkSignals(v[l[0]:l[1]], allowed, s)
				b.WriteString(v[last:l[0]])
				b.WriteByte(' ')
				last = l[1]
			}
			b.WriteString(v[last:])
			rest = b.String()
		}
		if tgRe.MatchString(v) {
			s[AnyLink], s[InviteLink] = true, true
		}
		// An in-app group invite or an event's join link is an invite even
		// when its text is not a URL the matcher recognises.
		if (f.Name == "invite.link" || f.Name == "event.join_link") && !allowedText(v, allowed) {
			s[AnyLink], s[InviteLink] = true, true
		}
		for _, m := range handleRe.FindAllStringSubmatch(rest, -1) {
			if !isMention(m[1]) {
				s[Handle] = true
			}
		}
		if len(PhoneSpans(rest)) > 0 {
			s[PhoneNumber] = true
		}
		if moneyRe.MatchString(rest) {
			s[MoneyAmount] = true
		}
	}
	return s
}

// isMention reports whether a handle match is the mention marker, perhaps
// followed by a full stop ("thanks @mention.", "@mention.See you"):
// handleRe's class takes a dot, and a sentence ending after a mention is not
// a handle.
func isMention(handle string) bool {
	rest, ok := strings.CutPrefix("@"+handle, client.MentionMarker)
	return ok && (rest == "" || rest[0] == '.')
}

// linkSignals classifies one found link. A link the URL parser rejects (a bad
// %-escape or port) still counts, by the host written before its path: the
// matcher found it, so it is never invisible.
func linkSignals(raw string, allowed map[string]bool, s signals) {
	if isEmail(raw) {
		return
	}
	host, path, ok := hostOf(raw)
	if !ok {
		host, path = textHost(raw)
	}
	reg := registrable(host)
	// whatsapp.net hosts serve media downloads, never a link a person posted.
	if reg == "whatsapp.net" || allowed[reg] {
		return
	}
	s[AnyLink] = true
	h := strings.TrimPrefix(host, "www.")
	if inviteHosts[h] || (reg == "whatsapp.com" && strings.HasPrefix(path, "/channel")) {
		s[InviteLink] = true
	}
	if shorteners[h] || shorteners[reg] {
		s[Shortener] = true
	}
}

// allowedText reports whether a URL-looking text belongs to an allowed domain.
func allowedText(v string, allowed map[string]bool) bool {
	host, _, ok := hostOf(strings.TrimSpace(v))
	return ok && allowed[registrable(host)]
}

// isEmail reports whether a found link is an email address: no "://", and an
// "@" before any path, query or fragment ("name@example.com"). An "@" after
// them ("youtube.com/@channel", "bitcoin:bc1q…?label=a@b") is part of a link;
// the matcher keeps a "?" or "#" right after the host only for a scheme
// without "//", such as "bitcoin:".
func isEmail(raw string) bool {
	if strings.Contains(raw, "://") {
		return false
	}
	authority, _, _ := strings.Cut(raw, "/")
	authority, _, _ = strings.Cut(authority, "?")
	authority, _, _ = strings.Cut(authority, "#")
	return strings.Contains(authority, "@")
}

// hostOf parses a link with the URL parser (ok false when it refuses).
func hostOf(raw string) (host, path string, ok bool) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", "", false
	}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), u.EscapedPath(), true
}

// textHost reads the host of a link the URL parser refused from its text:
// what follows any scheme and user name, up to the port or path.
func textHost(raw string) (host, path string) {
	s := raw
	if _, after, ok := strings.Cut(s, "://"); ok {
		s = after
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s, path = s[:i], s[i:]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	s, _, _ = strings.Cut(s, ":")
	return strings.ToLower(strings.TrimSuffix(s, ".")), path
}

// registrable is the domain a person registers ("evil.example.co.uk" →
// "example.co.uk"), by the Public Suffix List.
func registrable(host string) string {
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}

// IsPhone accepts 9-15 digits starting with + or 0, or one unbroken run of
// 10-15 digits ("447700900123"). PhoneSpans sets dates and "0." decimals
// aside first, which this digit count alone cannot tell from a number.
func IsPhone(s string) bool {
	digits := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	if digits < 9 || digits > 15 {
		return false
	}
	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "0") {
		return true
	}
	return digits >= 10 && digits == utf8.RuneCountInString(s)
}

// RegistrableDomain is the Public Suffix List view of an allowed_domains entry.
func RegistrableDomain(entry string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(entry))
	if strings.Contains(e, "@") {
		return "", false
	}
	host, _, ok := hostOf(e)
	if !ok || !strings.Contains(host, ".") {
		return "", false
	}
	return registrable(host), true
}

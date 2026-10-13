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

// sentenceTLDs are domain endings that are also everyday words or file
// endings. "Bitcoin is down again.Is it time to sell?" is a missing space
// after a full stop, not a link to again.is, so a match written with no
// scheme, "www." or path that ends in one of these is not a link.
var sentenceTLDs = map[string]bool{
	"am": true, "as": true, "at": true, "be": true, "by": true, "do": true, "go": true, "in": true,
	"is": true, "it": true, "me": true, "my": true, "no": true, "so": true, "to": true, "us": true,
	"now": true, "today": true, "love": true, "chat": true,
	"md": true, "mov": true, "zip": true,
}

const (
	// linkWindow bounds how far one search for a link reads. The link
	// pattern keeps a search alive to the end of a run with no spaces
	// ("1.1.1.1…"), so searching the whole post once per link made one
	// 64,000-character post take over ten minutes. No host is longer than
	// 253 characters, so a link that starts in a window is recognised there;
	// one that runs past the window's end is taken to the next space.
	linkWindow  = 1024
	linkOverlap = 256
	// maxLinks is how many links one field is searched for. A post with
	// more is spam already; the text after the last one searched is not
	// read for phone numbers or handles either.
	maxLinks = 64
)

// LinkSpans finds the links in s, at most limit of them (limit < 0: no
// limit), and how far s was searched. A sentence break such as "again.Is"
// and, in a file name, a bare "notes.zip" are not links.
func LinkSpans(s string, fileName bool, limit int) (spans [][2]int, searched int) {
	pos := 0
	for pos < len(s) && (limit < 0 || len(spans) < limit) {
		end := min(len(s), pos+linkWindow)
		for end < len(s) && end > pos && !utf8.RuneStart(s[end]) {
			end--
		}
		m := linkRe.FindStringIndex(s[pos:end])
		if m == nil {
			if end == len(s) {
				return spans, len(s)
			}
			next := end - linkOverlap
			for next < len(s) && !utf8.RuneStart(s[next]) {
				next++
			}
			pos = max(next, pos+1)
			continue
		}
		a, b := pos+m[0], pos+m[1]
		if b == end && end < len(s) {
			if i := strings.IndexFunc(s[b:], unicode.IsSpace); i >= 0 {
				b += i
			} else {
				b = len(s)
			}
		}
		if !notALink(s[a:b], s[b:], fileName) {
			spans = append(spans, [2]int{a, b})
		}
		pos = b
	}
	return spans, pos
}

// notALink reports whether a found link is a sentence break or a file name:
// written with no scheme, "www." or path, and ending in a sentenceTLD (or,
// in a file name, ending in anything). Invite hosts and shorteners stay
// links, and so does one the text goes on from with a port, path, fragment
// or query ("again.is?ref=1": the matcher stops before a query it cannot
// tell from a question mark).
func notALink(raw, after string, fileName bool) bool {
	low := strings.ToLower(raw)
	if strings.Contains(low, "://") || strings.HasPrefix(low, "www.") || strings.ContainsAny(low, "/?#:@") {
		return false
	}
	if len(after) > 0 && strings.IndexByte("/:#", after[0]) >= 0 {
		return false
	}
	if len(after) > 1 && after[0] == '?' && isASCIIAlnum(after[1]) {
		return false
	}
	host := strings.TrimSuffix(low, ".")
	if inviteHosts[host] || shorteners[host] {
		return false
	}
	return fileName || sentenceTLDs[host[strings.LastIndexByte(host, '.')+1:]]
}

// PhoneSpans finds the phone numbers in s: the digit runs IsPhone accepts,
// after dates, times and decimals that start "0." are set aside, so
// "07.11.2025 18:00" and "0.00012345 BTC" are not phone numbers.
func PhoneSpans(s string) [][2]int {
	masked := []byte(s)
	for _, m := range dateRe.FindAllStringIndex(s, -1) {
		// "06.12.34.56.78" goes on past the "date": it is a phone number.
		if rest := s[m[1]:]; len(rest) > 1 && strings.ContainsRune("./-", rune(rest[0])) && rest[1] >= '0' && rest[1] <= '9' {
			continue
		}
		blank(masked, m[0], m[1])
	}
	for _, m := range decimalRe.FindAllStringSubmatchIndex(s, -1) {
		blank(masked, m[2], m[3])
	}
	var spans [][2]int
	for _, m := range phoneRe.FindAllStringSubmatchIndex(string(masked), -1) {
		if IsPhone(s[m[2]:m[3]]) {
			spans = append(spans, [2]int{m[2], m[3]})
		}
	}
	return spans
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
		if links, searched := LinkSpans(v, f.Name == "document.file_name", maxLinks); len(links) > 0 || searched < len(v) {
			var b strings.Builder
			last := 0
			for _, l := range links {
				linkSignals(v[l[0]:l[1]], allowed, s)
				b.WriteString(v[last:l[0]])
				b.WriteByte(' ')
				last = l[1]
			}
			b.WriteString(v[last:searched])
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

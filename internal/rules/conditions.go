package rules

import (
	"bufio"
	"bytes"
	_ "embed" // the bundled shortener list
	"net/url"
	"regexp"
	"strings"
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
	// PhoneRe: a digit run with phone separators (group 1); IsPhone checks it.
	PhoneRe = regexp.MustCompile(`(?:^|[^\p{L}\p{N}])(\+?\d[\d \t().\-]{7,24}\d)`)
	moneyRe = regexp.MustCompile(`[$£€¥₹₦₽]\s?\d[\d,]*(?:\.\d+)?|\b\d[\d,]*(?:\.\d+)?\s?(?:k|m)?\s?(?:usd|usdt|usdc|gbp|eur|dollars?|pounds?|euros?|btc|eth)\b`)
)

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
		if links := linkRe.FindAllStringIndex(v, -1); len(links) > 0 {
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
		for _, m := range PhoneRe.FindAllStringSubmatch(rest, -1) {
			if IsPhone(m[1]) {
				s[PhoneNumber] = true
			}
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
// 10-15 digits ("447700900123"), so dates and prices do not count.
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

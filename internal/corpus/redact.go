package corpus

import (
	"regexp"
	"strings"

	"mvdan.cc/xurls/v2"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/rules"
)

// Redaction of a pasted message before it is saved.
//
// Private (the default, for the deployer's private corpus) keeps the
// pattern's shape and masks only what identifies a person: a phone number
// keeps its country code (or national 0) and every later digit becomes 0;
// an invite code becomes INVITECODE; an @-mention of a number becomes
// @mention. Domains and URL paths stay as they were.
//
// Public (for promotion into tests/corpus) also replaces every phone number
// with an Ofcom drama-range number on the repo's PII allowlist, every URL
// path other than an invite with /REDACTED, and every email address with
// name@example.com. Domains stay: they are the pattern a rule learns.
//
// Masked digits stay digits, so a redacted sample still shows the phone
// number a rule looks for.

// dramaNumbers are the public fixtures' numbers (.secret-pii-allowlist); a
// fourth distinct number in one message reuses the first.
var dramaNumbers = []string{"447700900123", "447700900456", "447700900789"}

var (
	linkRe = xurls.Relaxed()
	// mentionRe: an @-mention of a member by number, as WhatsApp copies it.
	mentionRe = regexp.MustCompile(`@\d{6,15}\b`)
)

// oneDigitCodes and twoDigitCodes are the ITU country calling codes of one
// and two digits; every other code has three.
var (
	oneDigitCodes = map[string]bool{"1": true, "7": true}
	twoDigitCodes = map[string]bool{
		"20": true, "27": true, "30": true, "31": true, "32": true, "33": true, "34": true, "36": true, "39": true,
		"40": true, "41": true, "43": true, "44": true, "45": true, "46": true, "47": true, "48": true, "49": true,
		"51": true, "52": true, "53": true, "54": true, "55": true, "56": true, "57": true, "58": true,
		"60": true, "61": true, "62": true, "63": true, "64": true, "65": true, "66": true,
		"81": true, "82": true, "84": true, "86": true, "90": true, "91": true, "92": true, "93": true, "94": true,
		"95": true, "98": true,
	}
)

// countryCodeLen is how many leading digits of an international number are
// its country code.
func countryCodeLen(digits string) int {
	switch {
	case len(digits) >= 1 && oneDigitCodes[digits[:1]]:
		return 1
	case len(digits) >= 2 && twoDigitCodes[digits[:2]]:
		return 2
	}
	return 3
}

// redactor redacts one message; the same number gets the same stand-in.
type redactor struct {
	public bool
	drama  map[string]string
}

// Redact returns text with the personal parts masked (see the package's
// redaction notes).
func Redact(text string, public bool) string {
	r := &redactor{public: public, drama: map[string]string{}}
	return r.text(text)
}

func (r *redactor) text(s string) string {
	s = mentionRe.ReplaceAllString(s, client.MentionMarker)
	var b strings.Builder
	last := 0
	for _, l := range linkRe.FindAllStringIndex(s, -1) {
		b.WriteString(r.phones(s[last:l[0]]))
		b.WriteString(r.link(s[l[0]:l[1]]))
		last = l[1]
	}
	b.WriteString(r.phones(s[last:]))
	return b.String()
}

// phones redacts every number in s (text outside links) that the rule
// engine reads as a phone number.
func (r *redactor) phones(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range rules.PhoneRe.FindAllStringSubmatchIndex(s, -1) {
		if p := s[m[2]:m[3]]; rules.IsPhone(p) {
			b.WriteString(s[last:m[2]])
			b.WriteString(r.phone(p))
			last = m[3]
		}
	}
	b.WriteString(s[last:])
	return b.String()
}

// phone redacts one number: private keeps the country code (or the national
// 0) and zeroes the rest; public swaps in a drama-range number.
func (r *redactor) phone(p string) string {
	digits := onlyDigits(p)
	if r.public {
		d, ok := r.drama[digits]
		if !ok {
			d = dramaNumbers[len(r.drama)%len(dramaNumbers)]
			r.drama[digits] = d
		}
		if p == digits { // one unbroken run, as in a wa.me link
			return d
		}
		return "+" + d[:2] + " " + d[2:6] + " " + d[6:]
	}
	keep := 0 // a national number: its leading 0 stays a 0 anyway
	if !strings.HasPrefix(p, "0") {
		keep = countryCodeLen(digits)
	}
	var b strings.Builder
	seen := 0
	for _, c := range p {
		if c >= '0' && c <= '9' {
			seen++
			if seen > keep {
				c = '0'
			}
		}
		b.WriteRune(c)
	}
	return b.String()
}

// link redacts one found link (or email address).
func (r *redactor) link(raw string) string {
	scheme, rest := "", raw
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme, rest = raw[:i+3], raw[i+3:]
	}
	if scheme == "" && strings.Contains(rest, "@") {
		if r.public {
			return "name@example.com"
		}
		return raw
	}
	host, path := rest, ""
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		host, path = rest[:j], rest[j:]
	}
	h := strings.TrimPrefix(strings.ToLower(host), "www.")
	switch {
	case path == "" || path == "/":
	case h == "chat.whatsapp.com":
		path = "/INVITECODE"
	case h == "whatsapp.com" && strings.HasPrefix(path, "/channel/"):
		path = "/channel/INVITECODE"
	case (h == "t.me" || h == "telegram.me" || h == "telegram.dog") && strings.HasPrefix(path, "/+"):
		path = "/+INVITECODE"
	case (h == "t.me" || h == "telegram.me" || h == "telegram.dog") && strings.HasPrefix(path, "/joinchat/"):
		path = "/joinchat/INVITECODE"
	case h == "wa.me" || h == "api.whatsapp.com":
		path = digitRunRe.ReplaceAllStringFunc(path, func(d string) string {
			if rules.IsPhone(d) {
				return r.phone(d)
			}
			return d
		})
	case r.public:
		path = "/REDACTED"
	}
	return scheme + host + path
}

// digitRunRe: a number written into a link (wa.me/<number>, ?phone=<number>).
var digitRunRe = regexp.MustCompile(`\d{9,15}`)

func onlyDigits(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

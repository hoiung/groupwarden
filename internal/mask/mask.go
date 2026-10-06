// Package mask hides WhatsApp identifiers before they reach logs or reports:
// only the last 4 digits of a number, LID or group ID stay visible.
package mask

import (
	"regexp"
	"strings"
)

// keep is how many trailing digits stay visible.
const keep = 4

// jidRe matches a WhatsApp ID: digits, an optional ":device", "@", a server.
var jidRe = regexp.MustCompile(`\+?([0-9]{5,})(?::[0-9]+)?@(s\.whatsapp\.net|c\.us|g\.us|lid|newsletter|broadcast)`)

// runRe matches any other digit run long enough to identify someone (a bare
// phone number, a group ID written without its server). A run that touches a
// letter is part of a word or a hex hash ("config v55786e8be51a"), not an ID,
// and is left alone; a hash made only of digits still reads as a number and is
// masked, because hiding a hash costs less than showing a phone number.
var runRe = regexp.MustCompile(`\+?[0-9]{5,}`)

// kinds names each server, so a masked ID still says what it was.
var kinds = map[string]string{
	"s.whatsapp.net": "phone", "c.us": "phone", "g.us": "group", "lid": "lid",
	"newsletter": "channel", "broadcast": "broadcast",
}

// IDs masks every identifier in s — one JID, a bare phone number, or a whole
// log line or error string that mentions several of them:
// "99999000000444@lid" → "lid…0444", "+447700900456" → "…0456".
// The masked form never looks like an email address or a number.
func IDs(s string) string {
	s = jidRe.ReplaceAllStringFunc(s, func(j string) string {
		m := jidRe.FindStringSubmatch(j)
		return kinds[m[2]] + "…" + last(m[1])
	})
	var b strings.Builder
	prev := 0
	for _, m := range runRe.FindAllStringIndex(s, -1) {
		if letterAt(s, m[0]-1) || letterAt(s, m[1]) {
			continue
		}
		b.WriteString(s[prev:m[0]])
		b.WriteString("…" + last(strings.TrimPrefix(s[m[0]:m[1]], "+")))
		prev = m[1]
	}
	b.WriteString(s[prev:])
	return b.String()
}

func letterAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i] | 0x20 // ASCII lower case
	return c >= 'a' && c <= 'z'
}

func last(digits string) string { return digits[len(digits)-keep:] }

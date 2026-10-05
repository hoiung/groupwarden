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
// phone number, a group ID written without its server).
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
	return runRe.ReplaceAllStringFunc(s, func(run string) string {
		return "…" + last(strings.TrimPrefix(run, "+"))
	})
}

func last(digits string) string { return digits[len(digits)-keep:] }

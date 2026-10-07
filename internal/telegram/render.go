package telegram

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/store"
)

// maxUnits is Telegram's message length limit, in UTF-16 code units.
const maxUnits = 4096

// removedNote replaces a member's message text once it is removed.
const removedNote = "(message text removed)"

// nameMax bounds the attachment's file name in a report's header (the name
// is in full under "Message:", as the sender wrote it).
const nameMax = 200

// rendered is a report as it goes to the chat: the report message, then any
// follow-ups the message text did not fit in. quotes[i] is where the member's
// text sits in message i; stripped[i] is what message i becomes once that
// text is removed ("" when it never carried any).
type rendered struct {
	parts    []string
	quotes   []quote
	stripped []string
}

// quote is the span of a message that is the member's text, in UTF-16 units
// (zero length: none).
type quote struct{ offset, length int }

// actionLine says what the bot did, by report kind (a report's own Action
// replaces it).
var actionLine = map[string]string{
	ledger.KindAction:         "deleted for everyone; sender removed and banned",
	ledger.KindWouldHaveActed: "none (watch-only rule, or the message was too old to act on)",
	ledger.KindExempt:         "none (an admin or Meta AI posted it)",
	ledger.KindUnaddressable:  "none (the bot cannot act on the sender's address)",
	ledger.KindLog:            "none (log rule)",
	ledger.KindNotDone:        "not done: an admin must do it by hand",
}

// render builds a report. With evidence it carries the group, the sender's
// display name and the last 4 digits of their number (of the LID when no
// number is known), the rule, the action, the config version, any attachment
// and every field the sender wrote, in full.
func render(r store.Report, ev *store.Evidence, groupName string) rendered {
	if ev == nil {
		parts, _ := split(r.Text, "")
		return rendered{parts: parts, quotes: make([]quote, len(parts)), stripped: make([]string, len(parts))}
	}
	head := header(r, ev, groupName, true)
	parts, starts := split(head+"\n"+messageText(ev), head)
	member := units(head + "\n") // where the member's text starts
	out := rendered{parts: parts, quotes: make([]quote, len(parts)), stripped: make([]string, len(parts))}
	for i, p := range parts {
		if lo, hi := max(starts[i], member), starts[i]+units(p); hi > lo {
			out.quotes[i] = quote{offset: lo - starts[i], length: hi - lo}
		}
		out.stripped[i] = removedNote
	}
	out.stripped[0] = clip(header(r, ev, groupName, false)+" "+removedNote, maxUnits)
	return out
}

// header is a report's text before the member's message: the report line,
// the group, the sender, the rule, the action, the config version and any
// attachment. Without full it leaves out what the sender wrote (their display
// name, the attachment's file name): that is what stays once their text is
// removed, in the chat and in the database.
func header(r store.Report, ev *store.Evidence, groupName string, full bool) string {
	var h strings.Builder
	h.WriteString(r.Text)
	h.WriteString("\n\nGroup: ")
	if groupName != "" {
		h.WriteString(groupName + " ")
	}
	h.WriteString("(" + mask.IDs(ev.Chat) + ")")
	h.WriteString("\nSender: " + sender(ev, full))
	if ev.Rule != "" {
		h.WriteString("\nRule: " + ev.Rule)
	}
	if a, ok := actionLine[r.Kind]; ok || r.Action != "" {
		if r.Action != "" {
			a = r.Action // what was done when it is not the kind's usual (a pause holds it)
		}
		h.WriteString("\nAction: " + a)
	}
	if ev.ConfigHash != "" {
		h.WriteString("\nConfig: v" + ev.ConfigHash)
	}
	if note := attachmentNote(ev, full); note != "" {
		h.WriteString("\nAttachment: " + note)
	}
	h.WriteString("\nMessage:")
	return h.String()
}

// sender names who posted: display name (unless named is false), then the
// masked number or LID.
func sender(ev *store.Evidence, named bool) string {
	id := ev.Sender
	for _, j := range []string{ev.Sender, ev.SenderAlt} {
		if client.JID(j).Server() == "s.whatsapp.net" {
			id = j
			break
		}
	}
	name := strings.TrimSpace(ev.PushName)
	switch {
	case !named:
		name = "(display name removed)"
	case name == "":
		name = "(no display name)"
	}
	return name + " (" + mask.IDs(id) + ")"
}

// messageText is every field the sender wrote, exactly as sent.
func messageText(ev *store.Evidence) string {
	var fields []client.Field
	if err := json.Unmarshal([]byte(ev.Fields), &fields); err != nil {
		return "(the evidence copy's text could not be read: " + err.Error() + ")"
	}
	if len(fields) == 0 {
		return "(no text)"
	}
	var b strings.Builder
	for i, f := range fields {
		if i > 0 {
			b.WriteString("\n")
		}
		if len(fields) > 1 || f.Name != "body" {
			b.WriteString(f.Name + ": ")
		}
		b.WriteString(f.Text)
	}
	return b.String()
}

// attachmentNote describes the deleted post's attachment, if it had one, with
// its file name (clipped) when named, and its size when known.
func attachmentNote(ev *store.Evidence, named bool) string {
	if ev.MediaKind == "" {
		return ""
	}
	desc := ev.MediaKind
	if name := strings.TrimSpace(ev.MediaName); named && name != "" {
		desc += " " + clip(name, nameMax)
	}
	if ev.MediaSize > 0 {
		desc += fmt.Sprintf(" (%.1f MB)", float64(ev.MediaSize)/(1<<20))
	}
	switch ev.MediaState {
	case store.MediaSaved:
		return desc + ", posted below"
	case store.MediaTooLarge:
		return desc + ", too large to keep"
	case store.MediaFailed:
		return desc + ", not available: " + ev.MediaError
	}
	return desc + ", being saved"
}

// split cuts s into messages within Telegram's limit, never inside a
// character, and returns each with where it starts in s (UTF-16 units). The
// first part always holds keep (the report's header) whole when it fits. A
// part is trimmed of the white space at its ends, as Telegram trims it, and a
// part that is white space alone is dropped: Telegram refuses it as empty.
func split(s, keep string) (parts []string, starts []int) {
	rest := []rune(s)
	pos := 0 // units of s before rest
	keepRunes := len([]rune(keep))
	first := true
	for len(rest) > 0 {
		n, used := 0, 0
		for n < len(rest) {
			u := runeUnits(rest[n])
			if used+u > maxUnits {
				break
			}
			used += u
			n++
		}
		cut := n
		// Prefer a line break in the last quarter of the message.
		if n < len(rest) {
			for i := n - 1; i > n*3/4; i-- {
				if rest[i] == '\n' {
					cut = i + 1
					break
				}
			}
		}
		if first && cut < keepRunes && n >= keepRunes {
			cut = n
		}
		chunk := rest[:cut]
		lead, end := 0, len(chunk)
		for lead < end && unicode.IsSpace(chunk[lead]) {
			lead++
		}
		for end > lead && unicode.IsSpace(chunk[end-1]) {
			end--
		}
		if end > lead {
			parts = append(parts, string(chunk[lead:end]))
			starts = append(starts, pos+units(string(chunk[:lead])))
		}
		pos += units(string(chunk))
		rest = rest[cut:]
		first = false
	}
	return parts, starts
}

// clip shortens s to at most n UTF-16 units, ending in "…" when cut.
func clip(s string, n int) string {
	if units(s) <= n {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		if used+runeUnits(r) > n-1 {
			break
		}
		b.WriteRune(r)
		used += runeUnits(r)
	}
	return b.String() + "…"
}

// units is s's length as Telegram counts it.
func units(s string) int {
	n := 0
	for _, r := range s {
		n += runeUnits(r)
	}
	return n
}

// runeUnits is r's length in UTF-16 code units (an invalid rune counts 1).
func runeUnits(r rune) int {
	if u := utf16.RuneLen(r); u > 0 {
		return u
	}
	return 1
}

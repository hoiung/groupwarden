package telegram

import (
	"encoding/json"
	"fmt"
	"strings"
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

// rendered is a report as it goes to the chat: the report message, then any
// follow-ups the message text did not fit in. stripped[i] is what message i
// becomes once the member's text is removed ("" when it never carried any).
type rendered struct {
	parts    []string
	stripped []string
}

// actionLine says what the bot did, by report kind.
var actionLine = map[string]string{
	ledger.KindAction:         "deleted for everyone; sender removed and banned",
	ledger.KindWouldHaveActed: "none (watch-only rule, or the message was too old to act on)",
	ledger.KindExempt:         "none (an admin or Meta AI posted it)",
	ledger.KindLog:            "none (log rule)",
}

// render builds a report. With evidence it carries the group, the sender's
// display name and the last 4 digits of their number (of the LID when no
// number is known), the rule, the action, the config version, any attachment
// and every field the sender wrote, in full.
func render(r store.Report, ev *store.Evidence, groupName string) rendered {
	if ev == nil {
		parts := split(r.Text, "")
		return rendered{parts: parts, stripped: make([]string, len(parts))}
	}
	var h strings.Builder
	h.WriteString(r.Text)
	h.WriteString("\n\nGroup: ")
	if groupName != "" {
		h.WriteString(groupName + " ")
	}
	h.WriteString("(" + mask.IDs(ev.Chat) + ")")
	h.WriteString("\nSender: " + sender(ev))
	if ev.Rule != "" {
		h.WriteString("\nRule: " + ev.Rule)
	}
	if a, ok := actionLine[r.Kind]; ok {
		h.WriteString("\nAction: " + a)
	}
	if ev.ConfigHash != "" {
		h.WriteString("\nConfig: v" + ev.ConfigHash)
	}
	if note := attachmentNote(ev); note != "" {
		h.WriteString("\nAttachment: " + note)
	}
	header := h.String() + "\nMessage:"
	text := messageText(ev)
	parts := split(header+"\n"+text, header)
	out := rendered{parts: parts, stripped: make([]string, len(parts))}
	out.stripped[0] = header + " " + removedNote
	for i := 1; i < len(parts); i++ {
		out.stripped[i] = removedNote
	}
	return out
}

// sender names who posted: display name, then the masked number or LID.
func sender(ev *store.Evidence) string {
	id := ev.Sender
	for _, j := range []string{ev.Sender, ev.SenderAlt} {
		if client.JID(j).Server() == "s.whatsapp.net" {
			id = j
			break
		}
	}
	name := strings.TrimSpace(ev.PushName)
	if name == "" {
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

// attachmentNote describes the deleted post's attachment, if it had one.
func attachmentNote(ev *store.Evidence) string {
	if ev.MediaKind == "" {
		return ""
	}
	desc := strings.TrimSpace(ev.MediaKind + " " + ev.MediaName)
	size := fmt.Sprintf("%.1f MB", float64(ev.MediaSize)/(1<<20))
	switch ev.MediaState {
	case store.MediaSaved:
		return desc + " (" + size + "), posted below"
	case store.MediaTooLarge:
		return desc + " (" + size + "), too large to keep"
	case store.MediaFailed:
		return desc + " (" + size + "), download failed: " + ev.MediaError
	}
	return desc + " (" + size + "), being saved"
}

// split cuts s into messages within Telegram's limit, never inside a
// character. The first part always holds keep (the report's header) whole
// when it fits.
func split(s, keep string) []string {
	if units(s) <= maxUnits {
		return []string{s}
	}
	var parts []string
	rest := []rune(s)
	first := true
	for len(rest) > 0 {
		n, used := 0, 0
		for n < len(rest) {
			u := utf16.RuneLen(rest[n])
			if u < 0 {
				u = 1
			}
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
		if first && cut < len([]rune(keep)) && n >= len([]rune(keep)) {
			cut = n
		}
		parts = append(parts, string(rest[:cut]))
		rest = rest[cut:]
		first = false
	}
	return parts
}

// units is s's length as Telegram counts it.
func units(s string) int {
	n := 0
	for _, r := range s {
		u := utf16.RuneLen(r)
		if u < 0 {
			u = 1
		}
		n += u
	}
	return n
}

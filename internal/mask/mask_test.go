package mask

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestIDsKeepsLastFourDigits(t *testing.T) {
	cases := map[string]string{
		"447700900123@s.whatsapp.net":                         "phone…0123",
		"99999000000444@lid":                                  "lid…0444",
		"99999000000444:12@lid":                               "lid…0444",
		"99999000000111@g.us":                                 "group…0111",
		"+447700900456":                                       "…0456",
		"removed 99999000000555@lid from 99999000000111@g.us": "removed lid…0555 from group…0111",
		"no ids here, code 404":                               "no ids here, code 404",
		"(447700900123), +447700900789.":                      "(…0123), …0789.",
		"phone:447700900456;":                                 "phone:…0456;",
		"config v55786e8be51a loaded":                         "config v55786e8be51a loaded",
		"ok 55786e8be51a: no change":                          "ok 55786e8be51a: no change",
		"commit e8be51a1234567 and ab12345cd":                 "commit e8be51a1234567 and ab12345cd",
		"digits-only hash 123456789012":                       "digits-only hash …9012",
		"Banned (#123456). To undo: /unban #123456":           "Banned (#123456). To undo: /unban #123456",
		"#12345 about 447700900123":                           "#12345 about …0123",
	}
	for in, want := range cases {
		if got := IDs(in); got != want {
			t.Errorf("IDs(%q) = %q, want %q", in, got, want)
		}
	}
}

// jid is a named string type, as client.JID is: slog holds it as an Any.
type jid string

// TestJSONLoggerMasksEveryAttribute: whatever a call site passes — the
// message, a string, an error, a named string type, a slice, attributes added
// with With or inside a group — no identifier reaches the log unmasked, while
// numbers, durations and ID-free values keep their type.
func TestJSONLoggerMasksEveryAttribute(t *testing.T) {
	var buf bytes.Buffer
	log := JSONLogger(&buf, slog.LevelDebug).With("chat", "99999000000111@g.us")
	log.WithGroup("g").Debug("removed 447700900123 from the group",
		"member", "99999000000444@lid",
		"err", errors.New("server refused 99999000000555@lid in 99999000000222@g.us"),
		"jid", jid("99999000000666@lid"),
		"ids", []string{"99999000000777@lid"},
		"plain", []string{"ok"},
		"n", 12345678,
		"took", time.Second,
		"report", "#12345")
	raw := buf.String()
	for _, leak := range []string{"99999000000", "447700900"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("an unmasked ID (%s) in %s", leak, raw)
		}
	}
	var f map[string]any
	if err := json.Unmarshal(buf.Bytes(), &f); err != nil {
		t.Fatalf("not one JSON line (%v): %s", err, raw)
	}
	g, _ := f["g"].(map[string]any)
	want := map[string]any{"member": "lid…0444", "err": "server refused lid…0555 in group…0222", "jid": "lid…0666",
		"ids": "[lid…0777]", "n": float64(12345678), "took": float64(time.Second), "report": "#12345"}
	if f["msg"] != "removed …0123 from the group" || f["chat"] != "group…0111" || g == nil {
		t.Fatalf("message or With attribute not masked: %s", raw)
	}
	for k, v := range want {
		if g[k] != v {
			t.Errorf("%s = %#v, want %#v", k, g[k], v)
		}
	}
	if plain, ok := g["plain"].([]any); !ok || len(plain) != 1 || plain[0] != "ok" {
		t.Errorf("an ID-free slice changed type: %#v", g["plain"])
	}
	buf.Reset()
	JSONLogger(&buf, nil).Debug("dropped")
	if buf.Len() != 0 {
		t.Fatalf("a nil level logs debug: %s", buf.String())
	}
}

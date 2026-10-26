package main

import (
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/client"
)

// TestParseMemberReadsNumbersAsBanDoes: the ban and member commands read a
// phone number as /ban <number> does (client.PhoneJID): a "(0)" after the
// country code is dropped, and a number with a leading 0 or fewer than 8
// digits is refused. Built at run time from the synthetic number listed in
// .secret-pii-allowlist.
func TestParseMemberReadsNumbersAsBanDoes(t *testing.T) {
	const number = "+44 7700 900123"
	want := client.JID(strings.TrimPrefix(strings.ReplaceAll(number, " ", ""), "+") + "@s.whatsapp.net")
	for _, s := range []string{number, strings.Replace(number, " 7700 ", " (0)7700 ", 1), string(want)} {
		if m, err := parseMember(s); err != nil || m.Phone != want || m.LID != "" {
			t.Errorf("parseMember(%q) = %+v, %v; want phone %s", s, m, err, want)
		}
	}
	digits := strings.TrimSuffix(string(want), "@s.whatsapp.net")
	for _, s := range []string{"0" + digits[2:], "+" + digits[:7]} {
		if m, err := parseMember(s); err == nil {
			t.Errorf("parseMember(%q) = %+v, want refused", s, m)
		}
	}
}

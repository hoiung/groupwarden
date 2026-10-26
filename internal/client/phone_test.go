package client

import (
	"strings"
	"testing"
)

// TestPhoneJID: a number written with its country code, however it is
// spaced or bracketed, is one phone JID; a "(0)" after the country code is
// dropped, one anywhere else is a digit; too few or too many digits, a
// leading 0 (no country code) or letters are not a phone number. The forms
// are built at run time from the synthetic number listed in
// .secret-pii-allowlist.
func TestPhoneJID(t *testing.T) {
	const number = "+44 7700 900123"
	want := JID(strings.TrimPrefix(strings.ReplaceAll(number, " ", ""), "+") + "@s.whatsapp.net")
	digits := strings.TrimSuffix(string(want), "@s.whatsapp.net")
	for name, s := range map[string]string{
		"spaces":      number,
		"dashes":      strings.ReplaceAll(number, " ", "-"),
		"brackets":    strings.Replace(number, " 7700 ", " (7700) ", 1),
		"digits-only": digits,
		"trunk-zero":  strings.Replace(number, " 7700 ", " (0)7700 ", 1),
	} {
		if got, ok := PhoneJID(s); !ok || got != want {
			t.Errorf("%s: PhoneJID(%q) = %q, %v; want %q", name, s, got, ok, want)
		}
	}
	if got, ok := PhoneJID(strings.Replace(number, "900123", "90(0)123", 1)); !ok || got != want {
		t.Errorf("bracketed zero inside the number: %q, %v; want %q", got, ok, want)
	}
	for name, s := range map[string]string{
		"too-short":    "+" + digits[:7],
		"too-long":     "+" + digits + "4567",
		"leading-zero": "0" + digits[2:],
		"letters":      strings.Replace(number, "900123", "9001AB", 1),
		"empty":        "",
	} {
		if got, ok := PhoneJID(s); ok {
			t.Errorf("%s: PhoneJID(%q) = %q, want not a phone number", name, s, got)
		}
	}
}

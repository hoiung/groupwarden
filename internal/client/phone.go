package client

import (
	"regexp"
	"strings"
	"unicode"
)

// A phone number a person writes (/ban <number>, `groupwarden ban add
// <phone>`) has its country code and, once its spaces, dashes, brackets and
// leading "+" are taken out, PhoneMinDigits to PhoneMaxDigits digits (E.164).
// A shorter run of digits is a report number in the admin chat.
const (
	PhoneMinDigits = 8
	PhoneMaxDigits = 15
)

// trunkZero is a "(0)" written straight after a country code (one to three
// digits): the national 0, which is not dialled from abroad.
var trunkZero = regexp.MustCompile(`^(\d{1,3})\s*\(0\)`)

// PhoneDigits takes out what people write around and between a phone
// number's digits (spaces, dashes, brackets, a leading "+", and a "(0)" after
// the country code) and reports whether only digits are left.
func PhoneDigits(s string) (string, bool) {
	s = trunkZero.ReplaceAllString(strings.TrimPrefix(strings.TrimSpace(s), "+"), "$1")
	bad := false
	d := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9':
			return r
		case unicode.IsSpace(r), r == '-', r == '(', r == ')':
			return -1
		}
		bad = true
		return -1
	}, s)
	return d, !bad && d != ""
}

// PhoneJID is the WhatsApp phone JID of a number written with its country
// code (ok false when s is not one: too short or long, or starting with 0,
// which no country code does, so it is a number written without one).
func PhoneJID(s string) (JID, bool) {
	d, ok := PhoneDigits(s)
	if !ok || len(d) < PhoneMinDigits || len(d) > PhoneMaxDigits || d[0] == '0' {
		return "", false
	}
	return JID(d + "@s.whatsapp.net"), true
}

package mask

import "testing"

func TestIDsKeepsLastFourDigits(t *testing.T) {
	cases := map[string]string{
		"447700900123@s.whatsapp.net":                         "phone…0123",
		"99999000000444@lid":                                  "lid…0444",
		"99999000000444:12@lid":                               "lid…0444",
		"99999000000111@g.us":                                 "group…0111",
		"+447700900456":                                       "…0456",
		"removed 99999000000555@lid from 99999000000111@g.us": "removed lid…0555 from group…0111",
		"no ids here, code 404":                               "no ids here, code 404",
	}
	for in, want := range cases {
		if got := IDs(in); got != want {
			t.Errorf("IDs(%q) = %q, want %q", in, got, want)
		}
	}
}

package config

import (
	"math"
	"strings"
	"testing"
)

// TestResolveCoreSchema: a plain scalar is typed by the YAML 1.2 core
// schema, so the YAML 1.1 words (yes, no, on, off) stay text; a quoted one is
// always text.
func TestResolveCoreSchema(t *testing.T) {
	for _, c := range []struct {
		in   string
		want any
	}{
		{"", nil}, {"~", nil}, {"null", nil}, {"Null", nil}, {"NULL", nil},
		{"true", true}, {"True", true}, {"TRUE", true}, {"false", false}, {"False", false}, {"FALSE", false},
		{"42", int64(42)}, {"-7", int64(-7)}, {"+3", int64(3)}, {"0777", int64(777)},
		{"0o17", int64(15)}, {"0x1F", int64(31)}, {"0xff", int64(255)},
		{"1.5", 1.5}, {".5", 0.5}, {"-2.", -2.0}, {"1e3", 1000.0}, {"6.02E+2", 602.0},
		{"yes", "yes"}, {"no", "no"}, {"on", "on"}, {"off", "off"}, {"nULL", "nULL"}, {"tRUE", "tRUE"},
		{"0o8", "0o8"}, {"0X1F", "0X1F"}, {"1_000", "1_000"}, {"12:30", "12:30"}, {"0b101", "0b101"},
	} {
		got, err := resolve(c.in, false)
		if err != nil || got != c.want {
			t.Errorf("resolve(%q) = %#v, %v; want %#v", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"", "null", "true", "42", "0x1F", "1.5", ".inf"} {
		if got, err := resolve(in, true); err != nil || got != in {
			t.Errorf("quoted %q = %#v, %v", in, got, err)
		}
	}
}

// TestResolveRefusesUnusableNumbers: a number too large for an int64, and
// infinities and NaN, are refused rather than read as something else.
func TestResolveRefusesUnusableNumbers(t *testing.T) {
	for in, want := range map[string]string{
		"9223372036854775808":      "9223372036854775808 is too large a number",
		"-9223372036854775809":     "-9223372036854775809 is too large a number",
		"0o7777777777777777777777": "0o7777777777777777777777 is too large a number",
		"0x8000000000000000":       "0x8000000000000000 is too large a number",
		"1e999":                    "1e999 is not a usable number",
		".inf":                     ".inf is not a usable number",
		"-.Inf":                    "-.Inf is not a usable number",
		".NaN":                     ".NaN is not a usable number",
	} {
		got, err := resolve(in, false)
		if err == nil || err.Error() != want {
			t.Errorf("resolve(%q) = %#v, %v; want %q", in, got, err, want)
		}
		if f, ok := got.(float64); ok && !math.IsNaN(f) {
			t.Errorf("resolve(%q) returned the number %v", in, f)
		}
	}
	if got, err := resolve("9223372036854775807", false); err != nil || got != int64(math.MaxInt64) {
		t.Errorf("the largest int64: %#v, %v", got, err)
	}
}

// TestParseYAMLRefuses: the YAML features the config does not support are
// refused at their line, so a value never comes from somewhere else in the
// file.
func TestParseYAMLRefuses(t *testing.T) {
	for _, c := range []struct {
		name, body, want string
	}{
		{"empty", "", "the config file is empty"},
		{"comments only", "# nothing\n", "the config file is empty"},
		{"two documents", "mode: enforce\n---\nmode: shadow\n", "line 2: only one YAML document is allowed (remove the extra ---)"},
		{"anchor", "word_lists:\n  a: &x [\"one\"]\n  b: *x\n", "line 2: anchors (&) and aliases (*) are not supported; write the value out"},
		{"tag", "mode: !!str enforce\n", "line 1: tags like !!str are not supported; use quotes for text (column 7)"},
		{"merge key", "rate:\n  <<: {burst: 1}\n", "line 2: merge keys (<<) are not supported; write the values out (column 3)"},
		{"complex key", "? [a, b]\n: 1\n", "line 1: a key must be plain text (column 3)"},
		{"syntax", "mode: [enforce\n", "line 1: did not find expected ',' or ']'"},
	} {
		_, err := parseYAML([]byte(c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want %q", c.name, err, c.want)
		}
	}
	// A quoted merge key is an ordinary key.
	tr, err := parseYAML([]byte("\"<<\": 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := tr.root.(map[string]any); !ok || m["<<"] != int64(1) {
		t.Fatalf("quoted <<: %#v", tr.root)
	}
}

// TestProblemString: a problem names its line and column when it has them.
func TestProblemString(t *testing.T) {
	ps := Problems{{Line: 4, Column: 2, Msg: "b"}, {Msg: "c"}, {Line: 3, Msg: "a"}}.sorted()
	if got := ps.Error(); got != "c\nline 3: a\nline 4: b (column 2)" {
		t.Fatalf("%q", got)
	}
}

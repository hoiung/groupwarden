package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// botToken is a token of BotFather's shape, built at run time so the secret
// scanner never sees one written out.
func botToken() string { return strings.Repeat("7", 9) + ":" + strings.Repeat("Ab_-", 9) }

func writeSecrets(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // past the umask
		t.Fatal(err)
	}
	return p
}

// TestReadSecretsFile: KEY=value lines, with blank lines and comments
// skipped, spaces around the key and value trimmed (a CRLF file included), a
// value's first "=" the only split and one pair of matching quotes removed.
func TestReadSecretsFile(t *testing.T) {
	tok := botToken()
	body := "# the admin chat\n\n" +
		KeyTelegramBot + " = \"" + tok + "\"\n" +
		"   " + KeyTelegramChatID + "='-100999'\r\n" +
		"EXTRA=a=b\n" +
		"MISMATCHED=\"x'\n" +
		"ONEQUOTE=\"\n" +
		"EMPTY=\n"
	got, err := ReadSecretsFile(writeSecrets(t, body, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{KeyTelegramBot: tok, KeyTelegramChatID: "-100999", "EXTRA": "a=b", "MISMATCHED": "\"x'",
		"ONEQUOTE": "\"", "EMPTY": ""}
	if len(got) != len(want) {
		t.Fatalf("read %d keys, want %d: %q", len(got), len(want), keys(got))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}

	// The owner-only file is also accepted read-only.
	if _, err := ReadSecretsFile(writeSecrets(t, "EXTRA=1\n", 0o400)); err != nil {
		t.Errorf("mode 0400: %v", err)
	}
}

// TestReadSecretsFileRefuses: a line without "=" (named by its number, its
// text never echoed), a file group or others can read, and a missing file.
func TestReadSecretsFileRefuses(t *testing.T) {
	_, err := ReadSecretsFile(writeSecrets(t, "# ok\n"+botToken()+"\n", 0o600))
	if err == nil || err.Error() != "secrets file line 2: expected KEY=value" {
		t.Errorf("a line without =: %v", err)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o660, 0o644} {
		p := writeSecrets(t, "EXTRA=1\n", mode)
		_, err := ReadSecretsFile(p)
		if err == nil || !strings.Contains(err.Error(), "is readable by others") || !strings.Contains(err.Error(), "run chmod 600") {
			t.Errorf("mode %04o: %v", mode, err)
		}
	}
	_, err = ReadSecretsFile(filepath.Join(t.TempDir(), "missing.env"))
	if !errors.Is(err, fs.ErrNotExist) || !strings.HasPrefix(err.Error(), "secrets file: ") {
		t.Errorf("a missing file: %v", err)
	}
}

// TestCheckTelegramToken passes only BotFather's shape, and no error repeats
// the value it was given.
func TestCheckTelegramToken(t *testing.T) {
	if err := CheckTelegramToken(botToken()); err != nil {
		t.Fatalf("a well-formed token: %v", err)
	}
	if err := CheckTelegramToken(""); err == nil || err.Error() != KeyTelegramBot+" is not set" {
		t.Errorf("empty: %v", err)
	}
	id, key := strings.Repeat("7", 9), strings.Repeat("Ab_-", 9)
	for name, v := range map[string]string{
		"no colon":       id + key,
		"short id":       "77777:" + key,
		"long id":        strings.Repeat("7", 13) + ":" + key,
		"letter in id":   "7777777a7:" + key,
		"short key":      id + ":" + key[:29],
		"space in key":   id + ":" + key[:20] + " " + key[20:],
		"trailing space": botToken() + " ",
		"newline":        botToken() + "\n",
	} {
		err := CheckTelegramToken(v)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), key[:20]) {
			t.Errorf("%s: the error repeats the value: %v", name, err)
		}
	}
}

// TestParseChatID: the admin chat's ID must be a negative (group) number.
func TestParseChatID(t *testing.T) {
	if id, err := ParseChatID("-1009999000000"); err != nil || id != -1009999000000 {
		t.Fatalf("a supergroup ID: %d %v", id, err)
	}
	if id, err := ParseChatID("-42"); err != nil || id != -42 {
		t.Fatalf("a basic group ID: %d %v", id, err)
	}
	for v, want := range map[string]string{
		"":     KeyTelegramChatID + " is not set",
		"abc":  KeyTelegramChatID + ` "abc" is not a number`,
		"-1.5": KeyTelegramChatID + ` "-1.5" is not a number`,
		"0":    KeyTelegramChatID + " 0 is not a group chat (group IDs are negative)",
		"4242": KeyTelegramChatID + " 4242 is not a group chat (group IDs are negative)",
	} {
		if _, err := ParseChatID(v); err == nil || err.Error() != want {
			t.Errorf("ParseChatID(%q) = %v, want %q", v, err, want)
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

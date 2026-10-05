package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Secrets-file keys. The file holds KEY=value lines and must be readable by
// its owner only.
const (
	KeyTelegramBot    = "TELEGRAM_TOKEN" // the bot token from BotFather
	KeyTelegramChatID = "TELEGRAM_CHAT_ID"
)

// ReadSecretsFile reads KEY=value lines (blank lines and # comments skipped,
// optional surrounding quotes removed). It refuses a file group or others can read.
func ReadSecretsFile(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("secrets file: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("secrets file %s is readable by others (mode %04o); run chmod 600", path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator names the secrets file
	if err != nil {
		return nil, fmt.Errorf("secrets file: %w", err)
	}
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("secrets file line %d: expected KEY=value", n)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, sc.Err()
}

// telegramTokenShape is a BotFather token: a numeric bot ID, a colon, a key.
var telegramTokenShape = regexp.MustCompile(`^[0-9]{6,12}:[A-Za-z0-9_-]{30,}$`)

// CheckTelegramToken checks a bot token's shape (never logs it).
func CheckTelegramToken(v string) error {
	if v == "" {
		return errors.New(KeyTelegramBot + " is not set")
	}
	if !telegramTokenShape.MatchString(v) {
		return errors.New(KeyTelegramBot + " does not look like a BotFather token")
	}
	return nil
}

// ParseChatID parses the admin chat's ID; a Telegram group's ID is negative.
func ParseChatID(v string) (int64, error) {
	if v == "" {
		return 0, errors.New(KeyTelegramChatID + " is not set")
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a number", KeyTelegramChatID, v)
	}
	if id >= 0 {
		return 0, fmt.Errorf("%s %d is not a group chat (group IDs are negative)", KeyTelegramChatID, id)
	}
	return id, nil
}

// Package configtest builds configs for tests in other packages.
package configtest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hoiung/groupwarden/internal/config"
)

// Base is the smallest valid config (paths under a temp dir are filled by
// Holder).
const Base = `data_dir: data
secrets_file: secrets.env
backup:
  target_dir: backups
`

// Static parses Base plus extra (relative paths under /nonexistent; nothing
// is read or written) and holds it. It panics on an invalid config.
func Static(extra string) *config.Holder {
	l, err := config.Parse([]byte(Base+extra), "/nonexistent")
	if err != nil {
		panic(err)
	}
	return config.NewHolder("", l)
}

// Holder writes Base plus extra to a config file in a temp dir, loads it and
// returns its holder and path.
func Holder(t testing.TB, extra string) (*config.Holder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(Base+extra), 0o600); err != nil {
		t.Fatal(err)
	}
	h, rejected, err := config.Boot(path)
	if err != nil || rejected != nil {
		t.Fatalf("config: %v %v", err, rejected)
	}
	return h, path
}

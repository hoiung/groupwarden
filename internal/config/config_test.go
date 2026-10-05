package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const base = `data_dir: /var/lib/groupwarden
secrets_file: /etc/groupwarden/secrets.env
backup:
  target_dir: /mnt/backup/groupwarden
`

func TestReplayWindowCappedAt47h(t *testing.T) {
	cfg, err := Parse([]byte(base), "/etc/groupwarden")
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if got := time.Duration(cfg.ActOnReplayMaxAge); got != 47*time.Hour {
		t.Fatalf("default act_on_replay_max_age = %s, want 47h", got)
	}
	if _, err := Parse([]byte(base+"act_on_replay_max_age: 47h\n"), "/x"); err != nil {
		t.Fatalf("47h refused: %v", err)
	}
	for _, v := range []string{"47h1m", "48h", "72h", "0s", "-1h"} {
		_, err := Parse([]byte(base+"act_on_replay_max_age: "+v+"\n"), "/x")
		if err == nil || !strings.Contains(err.Error(), "act_on_replay_max_age") {
			t.Errorf("act_on_replay_max_age %s: err = %v, want a refusal naming the key", v, err)
		}
	}
}

func TestLoadRefusesMissingFileAndUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "nope.yaml")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing file: err = %v", err)
	}
	if _, err := Parse([]byte(base+"surprise: 1\n"), dir); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("unknown key: err = %v", err)
	}
	if _, err := Parse([]byte(base+"data_dir: /other\n"), dir); err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Fatalf("duplicate key: err = %v", err)
	}
}

func TestRelativePathsResolveFromConfigDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	body := "data_dir: data\nsecrets_file: secrets.env\nbackup:\n  target_dir: /b\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != filepath.Join(dir, "data") || cfg.StoreDB() != filepath.Join(dir, "data", "groupwarden.db") {
		t.Fatalf("data_dir = %q", cfg.DataDir)
	}
	if cfg.Hash() == "" || len(cfg.Hash()) != 12 {
		t.Fatalf("hash = %q", cfg.Hash())
	}
}

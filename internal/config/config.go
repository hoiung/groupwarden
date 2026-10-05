// Package config loads and checks groupwarden's YAML configuration.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.yaml.in/yaml/v3"
)

// MaxReplayAge is the hard ceiling for act_on_replay_max_age: WhatsApp lets a
// group admin delete a message for everyone for about 2 days, so anything
// older can only be reported.
const MaxReplayAge = 47 * time.Hour

// Duration is a YAML duration written like "47h" or "90m".
type Duration time.Duration

// UnmarshalYAML parses a quoted or plain duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected a duration like \"47h\"", n.Line)
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration like \"47h\"", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes the duration as a string, for the config hash.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Reconcile holds the periodic sweep settings.
type Reconcile struct {
	IntervalMinutes int `yaml:"interval_minutes" json:"interval_minutes"`
}

// Backup holds where encrypted backups go.
type Backup struct {
	TargetDir    string `yaml:"target_dir" json:"target_dir"`
	AgeRecipient string `yaml:"age_recipient" json:"age_recipient"`
}

// Config is the loaded configuration.
type Config struct {
	DataDir                string    `yaml:"data_dir" json:"data_dir"`
	SecretsFile            string    `yaml:"secrets_file" json:"secrets_file"`
	DeployKeyFile          string    `yaml:"deploy_key_file" json:"deploy_key_file"`
	ActOnReplayMaxAge      Duration  `yaml:"act_on_replay_max_age" json:"act_on_replay_max_age"`
	DeafnessAlertHours     int       `yaml:"deafness_alert_hours" json:"deafness_alert_hours"`
	DisconnectAlertMinutes int       `yaml:"disconnect_alert_minutes" json:"disconnect_alert_minutes"`
	Reconcile              Reconcile `yaml:"reconcile" json:"reconcile"`
	Backup                 Backup    `yaml:"backup" json:"backup"`

	// Dir is the directory the config file was read from (not a key).
	Dir string `yaml:"-" json:"-"`
}

// defaults returns a Config holding every default; Load decodes over it.
func defaults() Config {
	return Config{
		ActOnReplayMaxAge:      Duration(MaxReplayAge),
		DeafnessAlertHours:     6,
		DisconnectAlertMinutes: 15,
		Reconcile:              Reconcile{IntervalMinutes: 60},
	}
}

// Load reads, decodes and checks the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator names the config file
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config file %s does not exist", path)
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw, filepath.Dir(path))
}

// Parse decodes and checks config bytes; dir resolves relative paths.
func Parse(raw []byte, dir string) (*Config, error) {
	cfg := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg.Dir = dir
	for _, p := range []*string{&cfg.DataDir, &cfg.SecretsFile, &cfg.DeployKeyFile, &cfg.Backup.TargetDir} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	if err := cfg.check(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) check() error {
	var errs []error
	if c.DataDir == "" {
		errs = append(errs, errors.New("data_dir is required"))
	}
	if c.SecretsFile == "" {
		errs = append(errs, errors.New("secrets_file is required"))
	}
	if c.Backup.TargetDir == "" {
		errs = append(errs, errors.New("backup.target_dir is required"))
	}
	age := time.Duration(c.ActOnReplayMaxAge)
	if age <= 0 || age > MaxReplayAge {
		errs = append(errs, fmt.Errorf("act_on_replay_max_age %s must be above 0 and at most %s (WhatsApp's admin delete window)", age, MaxReplayAge))
	}
	errs = append(errs,
		bound("deafness_alert_hours", c.DeafnessAlertHours, 1, 168),
		bound("disconnect_alert_minutes", c.DisconnectAlertMinutes, 1, 1440),
		bound("reconcile.interval_minutes", c.Reconcile.IntervalMinutes, 5, 1440),
	)
	return errors.Join(errs...)
}

func bound(key string, v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("%s %d must be between %d and %d", key, v, lo, hi)
	}
	return nil
}

// Hash is the short sha256 of the loaded configuration, shown as "config v<hash>".
func (c *Config) Hash() string {
	b, err := json.Marshal(c)
	if err != nil {
		panic(fmt.Sprintf("config hash: %v", err)) // a plain struct always marshals
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// WhatsmeowDB is the WhatsApp session store (never backed up).
func (c *Config) WhatsmeowDB() string { return filepath.Join(c.DataDir, "whatsmeow.db") }

// StoreDB is groupwarden's own database.
func (c *Config) StoreDB() string { return filepath.Join(c.DataDir, "groupwarden.db") }

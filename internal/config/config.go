// Package config loads, validates and reloads groupwarden's YAML config.
//
// A file is read as YAML 1.2 (duplicate keys, anchors and tags refused),
// checked against schema/config.schema.json (the only home of defaults and
// bounds), its words checked to be quoted, then compiled into a rules.Ruleset
// whose semantic checks (the combination rule, references, wildcards) run
// before anything is swapped in.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/hoiung/groupwarden/internal/rules"
)

// Duration is a config duration written like "47h" or "90m".
type Duration time.Duration

// UnmarshalJSON reads the schema-checked duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes the duration as a string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Retention holds the purge windows.
type Retention struct {
	EvidenceDays           int `json:"evidence_days"`
	ActionLogMonths        int `json:"action_log_months"`
	AnnouncementSecretDays int `json:"announcement_secret_days"`
}

// RetentionOverride is a community's retention; nil fields inherit.
type RetentionOverride struct {
	EvidenceDays           *int `json:"evidence_days,omitempty"`
	ActionLogMonths        *int `json:"action_log_months,omitempty"`
	AnnouncementSecretDays *int `json:"announcement_secret_days,omitempty"`
}

// Rate is the outbound WhatsApp action token bucket.
type Rate struct {
	PerMinute int `json:"per_minute"`
	Burst     int `json:"burst"`
}

// Breaker pauses removals and bans after too many in a window.
type Breaker struct {
	MaxActions    int `json:"max_actions"`
	WindowMinutes int `json:"window_minutes"`
}

// Reconcile holds the periodic sweep settings.
type Reconcile struct {
	IntervalMinutes int `json:"interval_minutes"`
}

// Backup holds where encrypted backups go.
type Backup struct {
	TargetDir    string `json:"target_dir"`
	AgeRecipient string `json:"age_recipient"`
	Keep         int    `json:"keep"`
}

// Evidence holds evidence-copy settings.
type Evidence struct {
	MaxAttachmentMB int `json:"max_attachment_mb"`
}

// Report holds admin-chat report settings.
type Report struct {
	AttachmentShowHours int `json:"attachment_show_hours"`
}

// Bans holds the ban list settings.
type Bans struct {
	Scope rules.BanScope `json:"scope"`
}

// RulesSection holds the rules and their word-length floor.
type RulesSection struct {
	MinWordLength int              `json:"min_word_length"`
	List          []rules.RuleSpec `json:"list"`
}

// Community is one configured community's overrides.
type Community struct {
	Name         string              `json:"name,omitempty"`
	Groups       []string            `json:"groups,omitempty"`
	Mode         rules.Mode          `json:"mode,omitempty"`
	DisableRules []string            `json:"disable_rules,omitempty"`
	WordLists    map[string][]string `json:"word_lists,omitempty"`
	Retention    *RetentionOverride  `json:"retention,omitempty"`
}

// Config is a loaded config with every default filled in.
type Config struct {
	DataDir                string               `json:"data_dir"`
	SecretsFile            string               `json:"secrets_file"`
	DeployKeyFile          string               `json:"deploy_key_file"`
	CorpusDir              string               `json:"corpus_dir"`
	ActOnReplayMaxAge      Duration             `json:"act_on_replay_max_age"`
	DeafnessAlertHours     int                  `json:"deafness_alert_hours"`
	DisconnectAlertMinutes int                  `json:"disconnect_alert_minutes"`
	ConfigSyncMinutes      int                  `json:"config_sync_minutes"`
	HeartbeatURL           string               `json:"heartbeat_url"`
	DailyCheckTime         string               `json:"daily_check_time"`
	Mode                   rules.Mode           `json:"mode"`
	Retention              Retention            `json:"retention"`
	Rate                   Rate                 `json:"rate"`
	Breaker                Breaker              `json:"breaker"`
	Reconcile              Reconcile            `json:"reconcile"`
	Backup                 Backup               `json:"backup"`
	Evidence               Evidence             `json:"evidence"`
	Report                 Report               `json:"report"`
	Bans                   Bans                 `json:"bans"`
	WordLists              map[string][]string  `json:"word_lists"`
	LeetWordLists          []string             `json:"leet_word_lists"`
	NeverMatch             []string             `json:"never_match"`
	AllowedDomains         []string             `json:"allowed_domains"`
	Rules                  RulesSection         `json:"rules"`
	Communities            map[string]Community `json:"communities"`

	// Dir is the directory the config file was read from (not a key).
	Dir string `json:"-"`
}

// Loaded is a config that passed every check, with its compiled rules.
type Loaded struct {
	Config *Config
	Rules  *rules.Ruleset
	// Hash identifies the compiled config ("config v<Hash>"): the sha256 of
	// the effective settings and the compiled ruleset, so comments, key
	// order and defaults written out do not change it.
	Hash string
	// Raw is the file as read (kept on disk as the last good copy).
	Raw []byte
}

// Load reads path, checks it fully (including the corpus when corpus_dir
// is set) and compiles it.
func Load(path string) (*Loaded, error) {
	l, err := Read(path)
	if err != nil {
		return nil, err
	}
	if err := checkCorpus(l); err != nil {
		return nil, err
	}
	return l, nil
}

// Read reads, checks and compiles path without the corpus test (which
// `corpus test` runs itself, to print every result).
func Read(path string) (*Loaded, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator names the config file
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config file %s does not exist", path)
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(raw, filepath.Dir(path))
}

// Parse checks config bytes and compiles them; dir resolves relative paths.
// It reads no other file.
func Parse(raw []byte, dir string) (*Loaded, error) {
	if _, _, err := configSchema(); err != nil {
		return nil, err
	}
	t, err := parseYAML(raw)
	if err != nil {
		return nil, err
	}
	if t.root == nil {
		return nil, Problems{{Msg: "the config file is empty"}}
	}
	_, doc, _ := configSchema()
	applyDefaults(t.root, doc)
	probs := append(validate(t), unquotedWords(t)...)
	if len(probs) > 0 {
		return nil, dedupe(probs).sorted()
	}
	cfg, err := decode(t.root)
	if err != nil {
		return nil, err
	}
	cfg.Dir = dir
	for _, p := range []*string{&cfg.DataDir, &cfg.SecretsFile, &cfg.DeployKeyFile, &cfg.CorpusDir, &cfg.Backup.TargetDir} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	if probs := cfg.checkRetention(t); len(probs) > 0 {
		return nil, probs.sorted()
	}
	rs, err := rules.Compile(cfg.Spec())
	if err != nil {
		var es rules.Errors
		if errors.As(err, &es) {
			return nil, ruleProblems(t, es).sorted()
		}
		return nil, err
	}
	return &Loaded{Config: cfg, Rules: rs, Hash: hashOf(cfg, rs), Raw: raw}, nil
}

func decode(root any) (*Config, error) {
	b, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

func dedupe(ps Problems) Problems {
	seen := map[string]bool{}
	var out Problems
	for _, p := range ps {
		if k := p.String(); !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}

// checkRetention enforces announcement_secret_days >= evidence_days, globally
// and for each community's effective values (a cross-key rule the schema
// cannot express).
func (c *Config) checkRetention(t *tree) Problems {
	var probs Problems
	check := func(r Retention, path []string, label string) {
		if r.AnnouncementSecretDays < r.EvidenceDays {
			pos := t.where(append(path, "announcement_secret_days"))
			probs = append(probs, Problem{Line: pos.line, Column: pos.col, Msg: fmt.Sprintf(
				"%sretention.announcement_secret_days (%d) must be at least retention.evidence_days (%d)",
				label, r.AnnouncementSecretDays, r.EvidenceDays)})
		}
	}
	check(c.Retention, []string{"retention"}, "")
	for _, id := range sortedKeys(c.Communities) {
		check(c.RetentionFor(id), []string{"communities", id, "retention"}, "communities."+id+": ")
	}
	return probs
}

// RetentionFor returns a community's effective retention.
func (c *Config) RetentionFor(community string) Retention {
	r := c.Retention
	if o := c.Communities[community].Retention; o != nil {
		if o.EvidenceDays != nil {
			r.EvidenceDays = *o.EvidenceDays
		}
		if o.ActionLogMonths != nil {
			r.ActionLogMonths = *o.ActionLogMonths
		}
		if o.AnnouncementSecretDays != nil {
			r.AnnouncementSecretDays = *o.AnnouncementSecretDays
		}
	}
	return r
}

// LongestRetention is the longest of each window across the global settings
// and every community: one purge serves all, so it keeps the longest.
func (c *Config) LongestRetention() Retention {
	r := c.Retention
	for id := range c.Communities {
		e := c.RetentionFor(id)
		r.EvidenceDays = max(r.EvidenceDays, e.EvidenceDays)
		r.ActionLogMonths = max(r.ActionLogMonths, e.ActionLogMonths)
		r.AnnouncementSecretDays = max(r.AnnouncementSecretDays, e.AnnouncementSecretDays)
	}
	return r
}

// Spec is the rules' view of the config.
func (c *Config) Spec() rules.Spec {
	s := rules.Spec{
		Mode: c.Mode, MinWordLength: c.Rules.MinWordLength, WordLists: c.WordLists,
		LeetWordLists: c.LeetWordLists, NeverMatch: c.NeverMatch, AllowedDomains: c.AllowedDomains,
		Rules: c.Rules.List, BanScope: c.Bans.Scope,
	}
	for _, id := range sortedKeys(c.Communities) {
		cm := c.Communities[id]
		s.Communities = append(s.Communities, rules.CommunitySpec{
			ID: id, Groups: cm.Groups, Mode: cm.Mode, DisableRules: cm.DisableRules, WordLists: cm.WordLists,
		})
	}
	return s
}

// ruleProblems places each compile error at its line in the file.
func ruleProblems(t *tree, es rules.Errors) Problems {
	var probs Problems
	for _, e := range es {
		pos := t.where(e.Path)
		if len(e.Path) >= 2 && e.Path[0] == "communities" && len(e.Path) == 2 {
			pos = t.keyAt(e.Path)
		}
		probs = append(probs, Problem{Line: pos.line, Column: pos.col, Msg: e.Msg})
	}
	return probs
}

func hashOf(c *Config, rs *rules.Ruleset) string {
	b, err := json.Marshal(struct {
		Config *Config `json:"config"`
		Rules  string  `json:"rules"`
	}{c, rs.Hash()})
	if err != nil {
		panic(fmt.Sprintf("config hash: %v", err)) // a plain struct always marshals
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// WhatsmeowDB is the WhatsApp session store (never backed up).
func (c *Config) WhatsmeowDB() string { return filepath.Join(c.DataDir, "whatsmeow.db") }

// StoreDB is groupwarden's own database.
func (c *Config) StoreDB() string { return filepath.Join(c.DataDir, "groupwarden.db") }

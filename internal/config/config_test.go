package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/rules"
)

const base = `data_dir: /var/lib/groupwarden
secrets_file: /etc/groupwarden/secrets.env
backup:
  target_dir: /mnt/backup/groupwarden
`

// Synthetic community and group IDs (listed exactly in .secret-pii-allowlist).
const (
	communityA = "99999000000999@g.us"
	communityB = "99999000000777@g.us"
	groupSet   = "99999000000888@g.us"
)

func parse(t *testing.T, body string) *Loaded {
	t.Helper()
	l, err := Parse([]byte(base+body), "/etc/groupwarden")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return l
}

// refused asserts the config is refused with a message matching every
// pattern, and returns the message.
func refused(t *testing.T, body string, patterns ...string) string {
	t.Helper()
	_, err := Parse([]byte(base+body), "/etc/groupwarden")
	if err == nil {
		t.Fatalf("accepted:\n%s", body)
	}
	for _, p := range patterns {
		if !regexp.MustCompile(p).MatchString(err.Error()) {
			t.Errorf("error %q does not match %q", err, p)
		}
	}
	return err.Error()
}

const lists = `word_lists:
  crypto: ["crypto", "bitcoin", "usdt"]
  stocks: ["stocks", "stock tips"]
  lures: ["guaranteed", "inbox me", "dm me"]
  dating: ["sugar daddy"]
never_match: ["in stock"]
`

func ruleYAML(name, action, confirmed, when string) string {
	return "    - name: " + name + "\n      action: " + action + "\n      confirmed: " + confirmed + "\n      when:\n" + when
}

// ---- Phase 1 (moved bounds now live in the schema) -------------------------

func TestReplayWindowCappedAt47h(t *testing.T) {
	l := parse(t, "")
	if got := time.Duration(l.Config.ActOnReplayMaxAge); got != 47*time.Hour {
		t.Fatalf("default act_on_replay_max_age = %s, want 47h", got)
	}
	parse(t, "act_on_replay_max_age: 47h\n")
	for _, v := range []string{"47h1m", "48h", "72h", "0s", "-1h", "soon"} {
		refused(t, "act_on_replay_max_age: "+v+"\n", `act_on_replay_max_age`, `line 5:`)
	}
}

func TestLoadRefusesMissingFileAndUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "nope.yaml")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing file: err = %v", err)
	}
	refused(t, "surprise: 1\n", `line 5: surprise is not a setting groupwarden knows \(column 1\)`)
}

func TestRelativePathsResolveFromConfigDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	body := "data_dir: data\nsecrets_file: secrets.env\nbackup:\n  target_dir: /b\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if l.Config.DataDir != filepath.Join(dir, "data") || l.Config.StoreDB() != filepath.Join(dir, "data", "groupwarden.db") {
		t.Fatalf("data_dir = %q", l.Config.DataDir)
	}
	if len(l.Hash) != 12 {
		t.Fatalf("hash = %q", l.Hash)
	}
}

// ---- AC 2.1 parsing and the schema ------------------------------------------

func TestDuplicateKeyRejected(t *testing.T) {
	refused(t, "data_dir: /other\n", `line 5: data_dir is defined twice \(first at line 1\) \(column 1\)`)
	refused(t, "word_lists:\n  crypto: [\"a1b\"]\n  crypto: [\"xyz\"]\n", `line 7: crypto is defined twice`)
}

func TestUnquotedNumberRejectedWithLine(t *testing.T) {
	msg := refused(t, "word_lists:\n  contact:\n    - \"call me\"\n    - 0800\n", `line 8: 0800 must be in quotes \(column 7\)`)
	if strings.Contains(msg, " 800 ") {
		t.Fatalf("the number was read as 800: %s", msg)
	}
	l := parse(t, "word_lists:\n  contact: [\"0800\"]\n")
	if got := l.Config.WordLists["contact"]; !slices.Equal(got, []string{"0800"}) {
		t.Fatalf("quoted 0800 = %q", got)
	}
}

func TestNOStaysString(t *testing.T) {
	l := parse(t, "rules:\n  min_word_length: 2\nword_lists:\n  country: [\"NO\", 'yes', \"off\"]\ncommunities:\n  "+communityA+":\n    name: NO\n")
	if got := l.Config.WordLists["country"]; !slices.Equal(got, []string{"NO", "yes", "off"}) {
		t.Fatalf("words = %q", got)
	}
	if got := l.Config.Communities[communityA].Name; got != "NO" {
		t.Fatalf("plain NO read as %q", got)
	}
	refused(t, "word_lists:\n  country: [NO]\n", `line 6: NO must be in quotes`)
}

func TestDecimalStaysString(t *testing.T) {
	l := parse(t, "word_lists:\n  versions: [\"1.10\", \"2.50\"]\n")
	if got := l.Config.WordLists["versions"]; !slices.Equal(got, []string{"1.10", "2.50"}) {
		t.Fatalf("words = %q", got)
	}
	refused(t, "word_lists:\n  versions: [1.10]\n", `line 6: 1\.10 must be in quotes`)
	refused(t, "communities:\n  "+communityA+":\n    name: 1.10\n", `line 7: 1\.10 must be in quotes`)
}

func TestBadWildcardRejected(t *testing.T) {
	for _, w := range []string{"cr*pto", "*", "stock * tips", "**coin"} {
		refused(t, "word_lists:\n  crypto: [\"ok\", \""+w+"\"]\n", `line 6: `, `\*`)
	}
	refused(t, "never_match: [\"in st*ck\"]\n", `line 5: never_match`)
	parse(t, "word_lists:\n  crypto: [\"broker*\", \"*coin\", \"stock photo*\"]\n")
}

func TestRawRegexKeyRejected(t *testing.T) {
	refused(t, lists+"rules:\n  list:\n"+ruleYAML("r", "log", "false", "        regex: \"crypt[o0]\"\n"),
		`line 17: "regex": raw regular expressions are not allowed`)
	refused(t, "word_lists:\n  pattern: [\"abc\"]\n", `line 6: "pattern": raw regular expressions are not allowed`)
	refused(t, "communities:\n  "+communityA+":\n    Regex: x\n", `"Regex": raw regular expressions are not allowed`)
}

func TestEveryDefaultInSchema(t *testing.T) {
	_, doc, err := configSchema()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"act_on_replay_max_age": "47h", "deafness_alert_hours": 6, "disconnect_alert_minutes": 15,
		"retention.evidence_days": 30, "retention.action_log_months": 12, "retention.announcement_secret_days": 90,
		"rate.per_minute": 10, "rate.burst": 5, "breaker.max_actions": 30, "breaker.window_minutes": 10,
		"reconcile.interval_minutes": 60, "config_sync_minutes": 5, "backup.keep": 14,
		"evidence.max_attachment_mb": 50, "report.attachment_show_hours": 24, "rules.min_word_length": 3,
		"bans.scope": "all_communities", "heartbeat_url": "",
	}
	for key, val := range want {
		node := doc
		for _, part := range strings.Split(key, ".") {
			node = asMap(asMap(node["properties"])[part])
		}
		got, ok := node["default"]
		if !ok || jsonString(got) != jsonString(val) {
			t.Errorf("%s: schema default = %v, want %v", key, got, val)
		}
		hasBound := node["maximum"] != nil || node["enum"] != nil || node["duration"] != nil || node["pattern"] != nil
		if !hasBound {
			t.Errorf("%s: no bound in the schema", key)
		}
	}
	// backup.target_dir is required, with no default.
	if !strings.Contains(jsonString(asMap(asMap(doc["properties"])["backup"])["required"]), "target_dir") {
		t.Error("backup.target_dir is not required")
	}
	if _, err := Parse([]byte("data_dir: /d\nsecrets_file: /s\nbackup: {}\n"), "/"); err == nil ||
		!strings.Contains(err.Error(), "backup.target_dir is required") {
		t.Errorf("missing backup.target_dir: %v", err)
	}
	// Every default above reaches the loaded config.
	c := parse(t, "").Config
	got := map[string]any{
		"deafness_alert_hours": c.DeafnessAlertHours, "disconnect_alert_minutes": c.DisconnectAlertMinutes,
		"retention.evidence_days": c.Retention.EvidenceDays, "retention.action_log_months": c.Retention.ActionLogMonths,
		"retention.announcement_secret_days": c.Retention.AnnouncementSecretDays, "rate.per_minute": c.Rate.PerMinute,
		"rate.burst": c.Rate.Burst, "breaker.max_actions": c.Breaker.MaxActions, "breaker.window_minutes": c.Breaker.WindowMinutes,
		"reconcile.interval_minutes": c.Reconcile.IntervalMinutes, "config_sync_minutes": c.ConfigSyncMinutes,
		"backup.keep": c.Backup.Keep, "evidence.max_attachment_mb": c.Evidence.MaxAttachmentMB,
		"report.attachment_show_hours": c.Report.AttachmentShowHours, "rules.min_word_length": c.Rules.MinWordLength,
		"bans.scope": string(c.Bans.Scope), "heartbeat_url": c.HeartbeatURL,
		"act_on_replay_max_age": short(time.Duration(c.ActOnReplayMaxAge)),
	}
	for key, val := range want {
		if jsonString(got[key]) != jsonString(val) {
			t.Errorf("%s: loaded %v, want the schema default %v", key, got[key], val)
		}
	}
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestValueAboveBoundRefused(t *testing.T) {
	above := map[string]string{
		"deafness_alert_hours: 169":                    "deafness_alert_hours 169 is above the maximum 168",
		"disconnect_alert_minutes: 1441":               "disconnect_alert_minutes 1441 is above the maximum 1440",
		"config_sync_minutes: 1441":                    "config_sync_minutes 1441 is above the maximum 1440",
		"retention:\n  evidence_days: 366":             "retention.evidence_days 366 is above the maximum 365",
		"retention:\n  action_log_months: 121":         "retention.action_log_months 121 is above the maximum 120",
		"rate:\n  per_minute: 61":                      "rate.per_minute 61 is above the maximum 60",
		"rate:\n  burst: 61":                           "rate.burst 61 is above the maximum 60",
		"breaker:\n  max_actions: 1001":                "breaker.max_actions 1001 is above the maximum 1000",
		"breaker:\n  window_minutes: 1441":             "breaker.window_minutes 1441 is above the maximum 1440",
		"reconcile:\n  interval_minutes: 1441":         "reconcile.interval_minutes 1441 is above the maximum 1440",
		"evidence:\n  max_attachment_mb: 51":           "evidence.max_attachment_mb 51 is above the maximum 50",
		"report:\n  attachment_show_hours: 48":         "report.attachment_show_hours 48 is above the maximum 47",
		"rules:\n  min_word_length: 21":                "rules.min_word_length 21 is above the maximum 20",
		"act_on_replay_max_age: 48h":                   "act_on_replay_max_age 48h is above the maximum 47h",
		"retention:\n  announcement_secret_days: 3651": "retention.announcement_secret_days 3651 is above the maximum 3650",
	}
	for body, msg := range above {
		refused(t, body+"\n", regexp.QuoteMeta(msg))
	}
	// backup is in the base config, so this one is written out in full.
	if _, err := Parse([]byte("data_dir: /d\nsecrets_file: /s\nbackup:\n  target_dir: /b\n  keep: 366\n"), "/"); err == nil ||
		!strings.Contains(err.Error(), "backup.keep 366 is above the maximum 365") {
		t.Errorf("backup.keep 366: %v", err)
	}
	refused(t, "bans:\n  scope: everywhere\n", `bans.scope must be one of all_communities, per_community`)
	if msg := refused(t, "heartbeat_url: not-a-url\n", `line 5: heartbeat_url is not in the expected form`); strings.Contains(msg, "not-a-url") {
		t.Errorf("the refused value was repeated: %s", msg)
	}
	// At the bound is accepted.
	parse(t, "deafness_alert_hours: 168\nreport:\n  attachment_show_hours: 47\nevidence:\n  max_attachment_mb: 50\n")
}

// ---- AC 2.2 semantic validation ---------------------------------------------

const artefacts = "          - has: [any_link, invite_link, shortener, phone_number, contact_card, handle]\n"

func TestKeywordOnlyActRefused(t *testing.T) {
	refused(t, lists+"rules:\n  list:\n"+ruleYAML("crypto-only", "delete_remove_ban", "true", "        words: crypto\n"),
		`line 17: rule "crypto-only" deletes, removes and bans`, `words from crypto alone`)
	// The same rule may report.
	parse(t, lists+"rules:\n  list:\n"+ruleYAML("crypto-only", "log", "true", "        words: crypto\n"))
}

func TestLureRuleMayAct(t *testing.T) {
	parse(t, lists+"rules:\n  list:\n"+ruleYAML("crypto-lure", "delete_remove_ban", "true",
		"        all:\n          - words: [crypto, stocks]\n          - words: lures\n"))
}

func TestNonFinanceKeywordPlusLinkMayAct(t *testing.T) {
	parse(t, lists+"rules:\n  list:\n"+ruleYAML("dating-link", "delete_remove_ban", "true",
		"        all:\n          - words: dating\n"+artefacts))
	// handle counts as a contact artefact.
	parse(t, lists+"rules:\n  list:\n"+ruleYAML("crypto-handle", "delete_remove_ban", "true",
		"        all:\n          - words: crypto\n          - has: handle\n"))
	// money_amount is a signal, not an artefact.
	refused(t, lists+"rules:\n  list:\n"+ruleYAML("crypto-money", "delete_remove_ban", "true",
		"        all:\n          - words: crypto\n          - has: money_amount\n"), `words from crypto \+ money_amount alone`)
}

func TestLinkOnlyActRefused(t *testing.T) {
	refused(t, lists+"rules:\n  list:\n"+ruleYAML("any-invite", "delete_remove_ban", "true", "        has: invite_link\n"),
		`rule "any-invite" deletes, removes and bans`, `invite_link alone`)
	parse(t, lists+"rules:\n  list:\n"+ruleYAML("any-invite", "log", "false", "        has: invite_link\n"))
}

func TestAnyGroupCannotBypass(t *testing.T) {
	// One branch of the any: group is "crypto" with nothing else.
	refused(t, lists+"rules:\n  list:\n"+ruleYAML("sneaky", "delete_remove_ban", "true",
		"        any:\n          - all:\n              - words: crypto\n"+
			"              - has: any_link\n          - words: crypto\n"),
		`rule "sneaky"`, `words from crypto alone`)
	refused(t, lists+"rules:\n  list:\n"+ruleYAML("nested", "delete_remove_ban", "true",
		"        all:\n          - words: crypto\n          - any:\n              - has: any_link\n              - has: money_amount\n"),
		`words from crypto \+ money_amount alone`)
	// References must resolve, names must be unique, conditions must be known.
	refused(t, lists+"rules:\n  list:\n"+ruleYAML("r", "log", "false", "        words: nonexistent\n"), `"nonexistent" is not a word list`)
	refused(t, lists+"rules:\n  list:\n"+ruleYAML("r", "log", "false", "        has: telepathy\n"), `"telepathy" is not a built-in condition`)
	refused(t, lists+"rules:\n  list:\n"+ruleYAML("r", "log", "false", "        words: crypto\n")+ruleYAML("r", "log", "false", "        words: lures\n"),
		`rule "r" is defined twice`)
	refused(t, lists+"rules:\n  list:\n"+ruleYAML("r", "log", "false", "        words: crypto\n        has: any_link\n"),
		`exactly one of all, any, words or has`)
}

func decide(l *Loaded, community, text string) rules.Decision {
	return l.Rules.Decide(rules.Input{Community: community,
		Fields: []client.Field{{Name: "body", Text: text, Match: text}}})
}

const pitch = "        all:\n          - words: [crypto, stocks]\n" + artefacts

func TestNewRuleDefaultsToShadow(t *testing.T) {
	body := lists + "mode: enforce\ncommunities:\n  " + communityA + ": {}\nrules:\n  list:\n" +
		"    - name: pitch\n      action: delete_remove_ban\n      when:\n" + pitch
	l := parse(t, body)
	if l.Config.Rules.List[0].Confirmed {
		t.Fatal("confirmed defaulted to true")
	}
	d := decide(l, communityA, "crypto signals t.me/+AbCd")
	if d.Action != rules.Log || !d.WouldHaveActed {
		t.Fatalf("unconfirmed rule decision = %+v", d)
	}
}

func TestEffectiveModeNeedsScopeAndConfirmed(t *testing.T) {
	for _, c := range []struct {
		mode, confirmed string
		acts            bool
	}{{"enforce", "true", true}, {"enforce", "false", false}, {"shadow", "true", false}, {"shadow", "false", false}} {
		l := parse(t, lists+"communities:\n  "+communityA+":\n    mode: "+c.mode+"\nrules:\n  list:\n"+
			ruleYAML("pitch", "delete_remove_ban", c.confirmed, pitch))
		d := decide(l, communityA, "crypto signals t.me/+AbCd")
		if acts := d.Action == rules.DeleteRemoveBan; acts != c.acts {
			t.Errorf("mode %s confirmed %s: decision %+v", c.mode, c.confirmed, d)
		}
	}
}

// ---- AC 2.6 per-community overrides -----------------------------------------

func TestCommunityOverridesGlobal(t *testing.T) {
	l := parse(t, lists+"mode: shadow\ncommunities:\n  "+communityA+":\n    mode: enforce\n    disable_rules: [crypto-lure]\n"+
		"    word_lists:\n      crypto: [\"localcoin\"]\n  "+communityB+": {}\nrules:\n  list:\n"+
		ruleYAML("pitch", "delete_remove_ban", "true", pitch)+
		ruleYAML("crypto-lure", "delete_remove_ban", "true", "        all:\n          - words: crypto\n          - words: lures\n"))
	// A enforces; B inherits the global shadow mode.
	if d := decide(l, communityA, "crypto signals t.me/+AbCd"); d.Action != rules.DeleteRemoveBan {
		t.Errorf("A: %+v", d)
	}
	if d := decide(l, communityB, "crypto signals t.me/+AbCd"); d.Action != rules.Log || !d.WouldHaveActed {
		t.Errorf("B: %+v", d)
	}
	// A disabled the lure rule.
	if d := decide(l, communityA, "crypto profits guaranteed"); d.Action != rules.ActionNone {
		t.Errorf("A lure: %+v", d)
	}
	// A's extra word applies in A only.
	if d := decide(l, communityA, "localcoin pump t.me/+AbCd"); d.Action != rules.DeleteRemoveBan {
		t.Errorf("A extra word: %+v", d)
	}
	if d := decide(l, communityB, "localcoin pump t.me/+AbCd"); d.Action != rules.ActionNone {
		t.Errorf("B extra word: %+v", d)
	}
	refused(t, lists+"communities:\n  "+communityA+":\n    disable_rules: [ghost]\n", `disable_rules names "ghost"`)
	refused(t, lists+"communities:\n  "+communityA+":\n    word_lists:\n      ghosts: [\"boo\"]\n", `"ghosts" is not a global word list`)
}

func TestPerCommunityRetention(t *testing.T) {
	l := parse(t, "communities:\n  "+communityA+":\n    retention:\n      evidence_days: 60\n  "+communityB+": {}\n")
	if r := l.Config.RetentionFor(communityA); r.EvidenceDays != 60 || r.ActionLogMonths != 12 || r.AnnouncementSecretDays != 90 {
		t.Errorf("A = %+v", r)
	}
	if r := l.Config.RetentionFor(communityB); r.EvidenceDays != 30 {
		t.Errorf("B = %+v", r)
	}
	if r := l.Config.LongestRetention(); r.EvidenceDays != 60 || r.AnnouncementSecretDays != 90 {
		t.Errorf("longest = %+v", r)
	}
	refused(t, "communities:\n  "+communityA+":\n    retention:\n      evidence_days: 120\n",
		`announcement_secret_days \(90\) must be at least retention.evidence_days \(120\)`)
	refused(t, "retention:\n  evidence_days: 40\n  announcement_secret_days: 31\n", `line 7: retention.announcement_secret_days \(31\)`)
}

func TestStandaloneGroupSet(t *testing.T) {
	l := parse(t, "communities:\n  Chats:\n    groups: [\""+groupSet+"\"]\n  "+communityA+": {}\n")
	if got := l.Rules.CommunityOf(groupSet, ""); got != "Chats" {
		t.Errorf("standalone group → %q", got)
	}
	if got := l.Rules.CommunityOf("99999000000111@g.us", communityA); got != communityA {
		t.Errorf("linked group → %q", got)
	}
	refused(t, "communities:\n  Chats: {}\n", `line 6: communities: "Chats" is not a WhatsApp Community ID`)
	refused(t, "communities:\n  Chats:\n    groups: [\"not-a-group\"]\n", `communities.Chats.groups.0 is not in the expected form`)
	refused(t, "communities:\n  One:\n    groups: [\""+groupSet+"\"]\n  Two:\n    groups: [\""+groupSet+"\"]\n", `is already in One`)
}

func TestBanScopePerCommunity(t *testing.T) {
	body := lists + "mode: enforce\ncommunities:\n  " + communityA + ": {}\n  " + communityB + ": {}\nrules:\n  list:\n" +
		ruleYAML("pitch", "delete_remove_ban", "true", pitch)
	if d := decide(parse(t, body), communityA, "crypto signals t.me/+AbCd"); !slices.Equal(d.BanIn, []string{communityB, communityA}) && !slices.Equal(d.BanIn, []string{communityA, communityB}) {
		t.Errorf("all_communities ban covers %v", d.BanIn)
	}
	d := decide(parse(t, body+"bans:\n  scope: per_community\n"), communityA, "crypto signals t.me/+AbCd")
	if !slices.Equal(d.BanIn, []string{communityA}) {
		t.Errorf("per_community ban covers %v", d.BanIn)
	}
}

// ---- AC 2.7 reload ------------------------------------------------------------

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func reloadFixture(t *testing.T) (dir, path, good string) {
	dir = t.TempDir()
	path = filepath.Join(dir, "config.yaml")
	good = "data_dir: data\nsecrets_file: s.env\nbackup:\n  target_dir: b\n" + lists
	writeConfig(t, path, good)
	return dir, path, good
}

func TestBadReloadKeepsLastGood(t *testing.T) {
	_, path, good := reloadFixture(t)
	h, rejected, err := Boot(path)
	if err != nil || rejected != nil {
		t.Fatalf("boot: %v %v", err, rejected)
	}
	before := h.Current()
	writeConfig(t, path, good+"rules:\n  list:\n"+ruleYAML("bad", "delete_remove_ban", "true", "        words: crypto\n"))
	_, err = h.Reload()
	var rj *Rejected
	if !errors.As(err, &rj) || !strings.HasPrefix(err.Error(), "REJECTED: ") || !strings.HasSuffix(err.Error(), ", still running v"+before.Hash) {
		t.Fatalf("reload error = %v", err)
	}
	if h.Current() != before {
		t.Fatal("a rejected reload changed the running config")
	}
	writeConfig(t, path, good+"mode: enforce\n")
	l, err := h.Reload()
	if err != nil || h.Current() != l || l.Hash == before.Hash {
		t.Fatalf("good reload: %v (hash %s → %s)", err, before.Hash, l.Hash)
	}
}

func TestBootUsesLastGoodOnDisk(t *testing.T) {
	dir, path, good := reloadFixture(t)
	h, _, err := Boot(path)
	if err != nil {
		t.Fatal(err)
	}
	goodHash := h.Current().Hash
	if _, err := os.Stat(filepath.Join(dir, "data", LastGoodName)); err != nil {
		t.Fatalf("last good copy not kept: %v", err)
	}
	writeConfig(t, path, good+"deafness_alert_hours: 999\n")
	h2, rejected, err := Boot(path)
	if err != nil || rejected == nil {
		t.Fatalf("boot with a bad file: %v %v", err, rejected)
	}
	if h2.Current().Hash != goodHash || !strings.Contains(rejected.Error(), "REJECTED: ") ||
		!strings.Contains(rejected.Error(), "deafness_alert_hours 999 is above the maximum 168") ||
		!strings.HasSuffix(rejected.Error(), "still running v"+goodHash) {
		t.Fatalf("booted %s, rejected = %v", h2.Current().Hash, rejected)
	}
	// With no last good copy a bad file refuses to boot.
	if err := os.Remove(filepath.Join(dir, "data", LastGoodName)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Boot(path); err == nil {
		t.Fatal("booted a bad config with no last good copy")
	}
}

func TestConfigHashIsCompiledRuleset(t *testing.T) {
	a := parse(t, lists)
	// Comments, key order, flow vs block style and defaults written out do
	// not change the compiled config.
	b := parse(t, "# comment\nnever_match:\n  - \"in stock\"\nword_lists:\n  dating: [\"sugar daddy\"]\n"+
		"  lures: [\"guaranteed\", \"inbox me\", \"dm me\"]\n  stocks: [\"stocks\", \"stock tips\"]\n"+
		"  crypto: [\"crypto\", \"bitcoin\", \"usdt\"]\nmode: shadow\nrules:\n  min_word_length: 3\n")
	if a.Hash != b.Hash {
		t.Fatalf("same compiled config, hashes %s and %s", a.Hash, b.Hash)
	}
	c := parse(t, strings.Replace(lists, "\"usdt\"", "\"usdc\"", 1))
	if c.Hash == a.Hash {
		t.Fatal("a word change kept the hash")
	}
	// The hash is the sha256 of the effective settings plus the compiled
	// ruleset, whose own hash is the sha256 of its compiled spec.
	spec, _ := json.Marshal(a.Rules.Spec())
	sum := sha256.Sum256(spec)
	if a.Rules.Hash() != hex.EncodeToString(sum[:]) {
		t.Fatal("ruleset hash is not the sha256 of the compiled spec")
	}
	if a.Hash != hashOf(a.Config, a.Rules) {
		t.Fatal("config hash is not derived from the compiled ruleset")
	}
}

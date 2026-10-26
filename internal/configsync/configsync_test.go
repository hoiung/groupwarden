package configsync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/configsync/configsynctest"
)

// exampleConfig is the shipped example with the node's paths filled in and
// a corpus inside the repo (so the corpus check runs on every new commit).
func exampleConfig(t *testing.T, dataDir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, r := range [][2]string{
		{"\ndata_dir: /var/lib/groupwarden\n", "\ndata_dir: " + dataDir + "\n"},
		{"\n# corpus_dir: corpus ", "\ncorpus_dir: corpus "},
	} {
		if strings.Count(s, r[0]) != 1 {
			t.Fatalf("the example config no longer has the line %q this test fills in", r[0])
		}
		s = strings.Replace(s, r[0], r[1], 1)
	}
	return s
}

var corpusFiles = map[string]string{
	"corpus/spam/1.yaml":          "text: \"bitcoin signals t.me/example_signals\"\n",
	"corpus/legit/general/1.yaml": "text: \"see you at class on Sunday\"\n",
}

type holder struct {
	pid     int
	command string
	held    bool
	err     error
}

func (h *holder) fn(string) (int, string, bool, error) { return h.pid, h.command, h.held, h.err }

type fixture struct {
	repo     *configsynctest.Repo
	live     string
	h        *holder
	signals  []int
	signalOK error
	dataDir  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	data := filepath.Join(t.TempDir(), "data")
	files := map[string]string{"config.yaml": exampleConfig(t, data)}
	for k, v := range corpusFiles {
		files[k] = v
	}
	return &fixture{repo: configsynctest.New(t, files), live: filepath.Join(t.TempDir(), "config", "config.yaml"),
		h: &holder{}, dataDir: data}
}

func (f *fixture) run(t *testing.T) Outcome {
	t.Helper()
	return f.sync().Run(context.Background())
}

func (f *fixture) sync() *Sync {
	return &Sync{Live: f.live, Repo: f.repo.Staging, Git: configsynctest.Git, PullTimeout: PullTimeout, Holder: f.h.fn,
		Signal: func(pid int) error { f.signals = append(f.signals, pid); return f.signalOK },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func releases(t *testing.T, liveDir string) []string {
	t.Helper()
	es, err := os.ReadDir(liveDir + ".releases")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func target(t *testing.T, liveDir string) string {
	t.Helper()
	tg, err := os.Readlink(liveDir)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

// TestSyncPullsChecksSwapsAndReloads walks one config repo through the
// sync's states: the first swap, no change, a new commit reloading the
// running bot, a rejected commit leaving the live config alone, a fixed
// commit swapped in while no bot runs (with old releases pruned), and a pull
// that fails.
func TestSyncPullsChecksSwapsAndReloads(t *testing.T) {
	f := newFixture(t)
	liveDir := filepath.Dir(f.live)
	c1 := f.repo.Head()

	o := f.run(t)
	if o.Status != OK || !o.Swapped || o.Reloaded || o.Commit != c1[:12] ||
		o.Result != "ok "+c1[:12]+": swapped in; the bot is not running, it loads this config when it starts" {
		t.Fatalf("first sync %+v", o)
	}
	if got := target(t, liveDir); got != filepath.Join("config.releases", c1) {
		t.Fatalf("live dir points at %q", got)
	}
	l, err := config.Load(f.live) // the corpus came with it: Load runs the corpus test
	if err != nil {
		t.Fatalf("live config: %v", err)
	}
	if l.Config.CorpusDir != filepath.Join(liveDir, "corpus") || l.Config.DataDir != f.dataDir {
		t.Fatalf("live paths: corpus %s data %s", l.Config.CorpusDir, l.Config.DataDir)
	}
	if _, err := os.Stat(filepath.Join(liveDir, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the release holds .git: %v", err)
	}

	if o := f.run(t); o.Status != OK || o.Swapped || o.Result != "ok "+c1[:12]+": no change" {
		t.Fatalf("second run %+v", o)
	}
	if len(f.signals) != 0 {
		t.Fatalf("signalled with no change: %v", f.signals)
	}

	cfg := exampleConfig(t, f.dataDir)
	c2 := f.repo.Commit(map[string]string{"config.yaml": strings.Replace(cfg, "mode: shadow", "mode: enforce", 1)}, "enforce")
	f.h.pid, f.h.command, f.h.held = 4242, "run", true
	o = f.run(t)
	if o.Status != OK || !o.Swapped || !o.Reloaded || o.Result != "ok "+c2[:12]+": swapped in; the bot was asked to reload" {
		t.Fatalf("new commit %+v", o)
	}
	if !slices.Equal(f.signals, []int{4242}) || target(t, liveDir) != filepath.Join("config.releases", c2) {
		t.Fatalf("signals %v, live %s", f.signals, target(t, liveDir))
	}

	c3 := f.repo.Commit(map[string]string{"config.yaml": strings.Replace(cfg, "mode: shadow", "mode: banana", 1)}, "bad mode")
	o = f.run(t)
	if o.Status != Rejected || o.Swapped || !strings.HasPrefix(o.Result, "REJECTED "+c3[:12]+": ") ||
		!strings.HasSuffix(o.Result, "; the bot keeps its current config") || !strings.Contains(o.Result, "banana") {
		t.Fatalf("bad commit %+v", o)
	}
	if target(t, liveDir) != filepath.Join("config.releases", c2) || slices.Contains(releases(t, liveDir), c3) {
		t.Fatalf("after a rejected commit: live %s, releases %v", target(t, liveDir), releases(t, liveDir))
	}
	if len(f.signals) != 1 {
		t.Fatalf("signalled for a rejected commit: %v", f.signals)
	}

	c4 := f.repo.Commit(map[string]string{"config.yaml": cfg}, "fixed")
	f.h.command = "groups" // a one-off WhatsApp command holds the data dir: never signalled
	if o := f.run(t); o.Status != OK || o.Reloaded || !strings.Contains(o.Result, "the bot is not running") {
		t.Fatalf("fixed commit %+v", o)
	}
	if got := releases(t, liveDir); !slices.Equal(got, sortedPair(c2, c4)) || len(f.signals) != 1 {
		t.Fatalf("releases %v (want the current and the previous), signals %v", got, f.signals)
	}

	if err := os.RemoveAll(f.repo.Remote); err != nil {
		t.Fatal(err)
	}
	o = f.run(t)
	if o.Status != Failed || o.Swapped || !strings.HasPrefix(o.Result, "FAILED -: git pull: ") {
		t.Fatalf("pull failure %+v", o)
	}
	if target(t, liveDir) != filepath.Join("config.releases", c4) {
		t.Fatalf("a failed pull moved the live config: %s", target(t, liveDir))
	}
}

func sortedPair(a, b string) []string {
	s := []string{a, b}
	slices.Sort(s)
	return s
}

// TestSyncRefusesUnsafeLayouts: a live directory that is not the sync's
// symlink is never overwritten; a config whose data dir or secrets would
// live inside the synced tree is rejected; a bot that cannot be found or
// signalled after a swap is a failure the admins must act on (/reload).
func TestSyncRefusesUnsafeLayouts(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.live), 0o700); err != nil {
		t.Fatal(err)
	}
	if o := f.run(t); o.Status != Failed || !strings.Contains(o.Result, "is a directory, not the symlink the sync swaps") {
		t.Fatalf("plain live dir %+v", o)
	}

	g := newFixture(t)
	cfg := exampleConfig(t, g.dataDir)
	for _, c := range []struct{ from, to, key string }{
		{"data_dir: " + g.dataDir, "data_dir: data", "data_dir"},
		{"secrets_file: /etc/groupwarden/secrets.env", "secrets_file: secrets.env", "secrets_file"},
	} {
		commit := g.repo.Commit(map[string]string{"config.yaml": strings.Replace(cfg, c.from, c.to, 1)}, c.key)
		o := g.run(t)
		if o.Status != Rejected || !strings.Contains(o.Result, c.key+" must be an absolute path outside the config repo") ||
			o.Commit != commit[:12] {
			t.Fatalf("%s inside the repo: %+v", c.key, o)
		}
	}

	h := newFixture(t)
	h.h.err = errors.New("lock unreadable")
	if o := h.run(t); o.Status != Failed || !o.Swapped || !strings.Contains(o.Result, "send /reload in the admin chat") {
		t.Fatalf("holder error %+v", o)
	}
	h.repo.Commit(map[string]string{"config.yaml": exampleConfig(t, h.dataDir) + "\n# touched\n"}, "touch")
	h.h = &holder{pid: 7, command: "run", held: true}
	h.signalOK = errors.New("no such process")
	if o := h.run(t); o.Status != Failed || !strings.Contains(o.Result, "could not be asked to reload (no such process)") {
		t.Fatalf("signal error %+v", o)
	}
}

// TestSyncPullTimesOut: a pull that stalls is cut off and reported as a
// failed sync (so it alerts) instead of holding the unit forever; the live
// config stays where it was.
func TestSyncPullTimesOut(t *testing.T) {
	f := newFixture(t)
	if o := f.run(t); o.Status != OK || !o.Swapped {
		t.Fatalf("first sync %+v", o)
	}
	before := target(t, filepath.Dir(f.live))
	s := f.sync()
	s.PullTimeout = 50 * time.Millisecond
	s.Git = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		if args[0] == "pull" {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return configsynctest.Git(ctx, dir, env, args...)
	}
	done := make(chan Outcome, 1)
	go func() { done <- s.Run(context.Background()) }()
	select {
	case o := <-done:
		if o.Status != Failed || !strings.HasPrefix(o.Result, "FAILED -: git pull: ") ||
			!errors.Is(o.Err, context.DeadlineExceeded) {
			t.Fatalf("stalled pull %+v", o)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled pull held the sync past its timeout")
	}
	if got := target(t, filepath.Dir(f.live)); got != before {
		t.Fatalf("a timed-out pull moved the live config: %s", got)
	}

	// A pull that ignores the deadline but returns after it still failed.
	s.Git = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		if args[0] == "pull" {
			<-ctx.Done()
			return "", nil
		}
		return configsynctest.Git(ctx, dir, env, args...)
	}
	if o := s.Run(context.Background()); o.Status != Failed || !errors.Is(o.Err, context.DeadlineExceeded) {
		t.Fatalf("late pull %+v", o)
	}
}

// TestNewEpisode: an alert goes out when a failure starts, when the status
// changes, and when a different commit is rejected; never on success.
func TestNewEpisode(t *testing.T) {
	failed := Outcome{Status: Failed, Commit: "-"}
	rejected := Outcome{Status: Rejected, Commit: "abc123abc123"}
	cases := []struct {
		previous string
		o        Outcome
		want     bool
	}{
		{"", failed, true},
		{"ok abc123abc123: no change", failed, true},
		{"FAILED -: git pull: timeout", failed, false},
		{"REJECTED abc123abc123: bad", failed, true},
		{"FAILED -: git pull: timeout", rejected, true},
		{"REJECTED abc123abc123: bad", rejected, false},
		{"REJECTED 999999999999: bad", rejected, true},
		{"FAILED -: git pull: timeout", Outcome{Status: OK, Commit: "abc"}, false},
	}
	for _, c := range cases {
		if got := NewEpisode(c.previous, c.o); got != c.want {
			t.Errorf("NewEpisode(%q, %s %s) = %v", c.previous, c.o.Status, c.o.Commit, got)
		}
	}
}

// TestSyncRefusesADeployKeyInTheRepo: the deploy key, like the data dir and
// the secrets file, must live outside the synced tree.
func TestSyncRefusesADeployKeyInTheRepo(t *testing.T) {
	f := newFixture(t)
	if o := f.run(t); o.Status != OK || !o.Swapped {
		t.Fatalf("first sync %+v", o)
	}
	liveDir := filepath.Dir(f.live)
	before := target(t, liveDir)
	cfg := strings.Replace(exampleConfig(t, f.dataDir), "\nsecrets_file: ", "\ndeploy_key_file: keys/deploy\nsecrets_file: ", 1)
	commit := f.repo.Commit(map[string]string{"config.yaml": cfg}, "key in the repo")
	o := f.run(t)
	if o.Status != Rejected || o.Swapped || o.Commit != commit[:12] ||
		!strings.Contains(o.Result, "deploy_key_file must be an absolute path outside the config repo") {
		t.Fatalf("deploy key inside the repo: %+v", o)
	}
	if got := target(t, liveDir); got != before || slices.Contains(releases(t, liveDir), commit) {
		t.Fatalf("after a rejected commit: live %s, releases %v", got, releases(t, liveDir))
	}
}

// TestSyncRefusesASymlinkInTheRepo: a release is a copy of regular files
// only; a commit with a symlink fails the copy and leaves the live config and
// the releases directory as they were (no half-built copy).
func TestSyncRefusesASymlinkInTheRepo(t *testing.T) {
	f := newFixture(t)
	if o := f.run(t); o.Status != OK || !o.Swapped {
		t.Fatalf("first sync %+v", o)
	}
	liveDir := filepath.Dir(f.live)
	before, rels := target(t, liveDir), releases(t, liveDir)
	if err := os.Symlink("config.yaml", filepath.Join(f.repo.Work, "linked.yaml")); err != nil {
		t.Fatal(err)
	}
	commit := f.repo.Commit(nil, "a symlink")
	o := f.run(t)
	if o.Status != Failed || o.Swapped || o.Commit != commit[:12] ||
		!strings.Contains(o.Result, "copy the new config: linked.yaml is not a regular file (symlinks are not synced)") {
		t.Fatalf("symlink in the repo: %+v", o)
	}
	if got := target(t, liveDir); got != before {
		t.Fatalf("a failed copy moved the live config: %s", got)
	}
	if got := releases(t, liveDir); !slices.Equal(got, rels) {
		t.Fatalf("releases after a failed copy: %v, want %v", got, rels)
	}
	if len(f.signals) != 0 {
		t.Fatalf("signalled after a failed copy: %v", f.signals)
	}
}

// TestSyncNeedsAReadableLiveConfig: once a live config exists the sync reads
// its deploy key and data dir from it alone. A live file that no longer reads
// fails the run before any pull (the repo's copy is only for the first sync).
func TestSyncNeedsAReadableLiveConfig(t *testing.T) {
	f := newFixture(t)
	if o := f.run(t); o.Status != OK || !o.Swapped {
		t.Fatalf("first sync %+v", o)
	}
	// Break the live copy in place (the release is the sync's own, mode 0600).
	if err := os.WriteFile(f.live, []byte("mode: banana\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := f.sync()
	var calls [][]string
	s.Git = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		calls = append(calls, args)
		return configsynctest.Git(ctx, dir, env, args...)
	}
	o := s.Run(context.Background())
	if o.Status != Failed || o.Settings != nil || o.Commit != "-" ||
		!strings.HasPrefix(o.Result, "FAILED -: no config to read the deploy key and data dir from: ") {
		t.Fatalf("unreadable live config: %+v", o)
	}
	if len(calls) != 0 {
		t.Fatalf("git ran with no settings: %v", calls)
	}
}

// TestSSHEnv: git never prompts, and with a deploy key its ssh command names
// the key as one argument however the path is written (git runs the command
// through the shell).
func TestSSHEnv(t *testing.T) {
	if got := sshEnv(""); !slices.Equal(got, []string{"GIT_TERMINAL_PROMPT=0"}) {
		t.Fatalf("no key: %q", got)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, key := range []string{
		"/var/lib/groupwarden/deploy_key",
		"/srv/my keys/deploy key",
		"/srv/it's/deploy",
		`/srv/$HOME/"q"/` + "`x`" + `/\n;*`,
	} {
		env := sshEnv(key)
		if len(env) != 2 || env[0] != "GIT_TERMINAL_PROMPT=0" || !strings.HasPrefix(env[1], "GIT_SSH_COMMAND=") {
			t.Fatalf("key %q: env %q", key, env)
		}
		// The shell's own word splitting of the command, as git hands it over.
		cmd := exec.Command(sh, "-c", `eval "set -- $GIT_SSH_COMMAND"; printf '%s\n' "$@"`) // #nosec G204 -- the test's own values
		cmd.Env = []string{env[1]}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("key %q: sh: %v", key, err)
		}
		want := []string{"ssh", "-i", key, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes"}
		if got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"); !slices.Equal(got, want) {
			t.Errorf("key %q: ssh runs as %q", key, got)
		}
	}
}

// TestShort: a commit is shown by its first 12 characters.
func TestShort(t *testing.T) {
	for in, want := range map[string]string{"": "", "abc": "abc", "0123456789ab": "0123456789ab",
		"0123456789abcdef0123": "0123456789ab"} {
		if got := short(in); got != want {
			t.Errorf("short(%q) = %q, want %q", in, got, want)
		}
	}
}

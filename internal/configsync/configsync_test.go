package configsync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
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
	s := strings.Replace(string(raw), "data_dir: /var/lib/groupwarden", "data_dir: "+dataDir, 1)
	s = strings.Replace(s, "# corpus_dir: /etc/groupwarden/corpus", "corpus_dir: corpus", 1)
	if s == string(raw) || !strings.Contains(s, "corpus_dir: corpus") {
		t.Fatal("the example config no longer has the lines this test fills in")
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

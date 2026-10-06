// Package configsynctest builds a private config repo for tests: a bare
// remote, a working clone the "operator" commits to, and the node's staging
// clone the sync pulls into. Git runs isolated from the user's own config.
package configsynctest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repo is one config repo with its clones.
type Repo struct {
	t                     *testing.T
	Remote, Work, Staging string
}

// isolated keeps the user's git config (hooks, signing) out of the tests.
func isolated(env []string) []string {
	email := "tester" + "@" + "example.invalid"
	return append(append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=tester", "GIT_AUTHOR_EMAIL="+email, "GIT_COMMITTER_NAME=tester", "GIT_COMMITTER_EMAIL="+email), env...)
}

// Git runs git in dir like the sync's own runner, isolated from the user's
// git config.
func Git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- test helper
	cmd.Env = isolated(env)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (r *Repo) git(dir string, args ...string) string {
	r.t.Helper()
	out, err := Git(context.Background(), dir, nil, args...)
	if err != nil {
		r.t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(out)
}

// New makes the repo with files as its first commit and clones it for the
// node.
func New(t *testing.T, files map[string]string) *Repo {
	t.Helper()
	base := t.TempDir()
	r := &Repo{t: t, Remote: filepath.Join(base, "remote.git"), Work: filepath.Join(base, "work"),
		Staging: filepath.Join(base, "staging")}
	if err := os.MkdirAll(r.Work, 0o700); err != nil {
		t.Fatal(err)
	}
	r.git(r.Work, "init", "-q", "-b", "main")
	r.write(files)
	r.git(r.Work, "add", "-A")
	r.git(r.Work, "commit", "-q", "-m", "first config")
	r.git(base, "clone", "-q", "--bare", r.Work, r.Remote)
	r.git(r.Work, "remote", "add", "origin", r.Remote)
	r.git(base, "clone", "-q", r.Remote, r.Staging)
	return r
}

func (r *Repo) write(files map[string]string) {
	r.t.Helper()
	for rel, body := range files {
		p := filepath.Join(r.Work, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
}

// Commit writes files, commits and pushes them, and returns the commit.
func (r *Repo) Commit(files map[string]string, msg string) string {
	r.t.Helper()
	r.write(files)
	r.git(r.Work, "add", "-A")
	r.git(r.Work, "commit", "-q", "-m", msg)
	r.git(r.Work, "push", "-q", "origin", "HEAD:main")
	return r.git(r.Work, "rev-parse", "HEAD")
}

// Head is the working clone's last commit.
func (r *Repo) Head() string { return r.git(r.Work, "rev-parse", "HEAD") }

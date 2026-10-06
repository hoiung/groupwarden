// Package configsync brings the private config repo to the running bot. A
// systemd timer runs it every config_sync_minutes: it pulls the staging
// clone (--ff-only, with the read-only deploy key), checks the new config
// the way `groupwarden check` does, swaps it in whole and asks the running
// bot to reload (SIGHUP). The bot then posts "config v<hash> loaded" or
// REJECTED in the admin chat.
//
// Layout: the bot's config file sits in a directory that is a symlink,
// <base>/config -> config.releases/<commit>. Each release is a copy of the
// repo's tree at one commit, so the config and its corpus change together
// and a swap is one atomic rename. The current and the previous release are
// kept.
//
//	pull ──fails or times out──► FAILED (alert once per episode)
//	  └─► HEAD is the live release ──► ok: no change
//	  └─► copy HEAD's tree ──► check ──fails──► REJECTED (alert once per commit; live untouched)
//	        └─► swap the symlink ──► bot running: SIGHUP ──► ok (the bot reports loaded / REJECTED)
//	                              └─► bot not running: ok (it loads the release at start)
package configsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/mask"
)

// Status words: the first word of every result line.
const (
	OK       = "ok"
	Failed   = "FAILED"
	Rejected = "REJECTED"
)

// PullTimeout bounds one git pull. A stalled network would otherwise hold the
// oneshot unit forever: no result is recorded, no alert goes out, and the
// timer never fires again.
const PullTimeout = 2 * time.Minute

// Git runs git in dir with extra environment entries and returns its output.
type Git func(ctx context.Context, dir string, env []string, args ...string) (string, error)

// Sync is one run's inputs.
type Sync struct {
	// Live is the bot's config file (its --config); its directory is the
	// symlink the sync swaps.
	Live string
	// Repo is the staging clone of the private config repo.
	Repo string
	Git  Git
	// PullTimeout bounds the pull (PullTimeout in production).
	PullTimeout time.Duration
	// Holder reports which process holds a data dir's lock (app.LockHolder).
	Holder func(dataDir string) (pid int, command string, held bool, err error)
	// Signal asks a process to reload its config.
	Signal func(pid int) error
	Log    *slog.Logger
}

// Outcome is what one run did.
type Outcome struct {
	Status string // OK, Failed or Rejected
	Commit string // the staging clone's HEAD, short ("-" when unknown)
	// Result is the line /status shows and the sync service prints to its
	// journal: "<status> <commit>: <what happened>", identifiers masked (a
	// rejected config names its community by group ID).
	Result   string
	Swapped  bool
	Reloaded bool
	Err      error
	// Settings is the config the run read its deploy key and data dir from
	// (nil when none could be read).
	Settings *config.Config
}

func (o *Outcome) set(status, detail string, err error) Outcome {
	o.Status, o.Err = status, err
	if o.Commit == "" {
		o.Commit = "-"
	}
	o.Result = status + " " + o.Commit + ": " + mask.IDs(detail)
	return *o
}

// Run pulls, checks, swaps and signals once.
func (s *Sync) Run(ctx context.Context) Outcome {
	var o Outcome
	name, liveDir := filepath.Base(s.Live), filepath.Dir(s.Live)
	settings, err := s.settings(name)
	if err != nil {
		return o.set(Failed, "no config to read the deploy key and data dir from: "+err.Error(), err)
	}
	o.Settings = settings
	pullCtx, cancel := context.WithTimeout(ctx, s.PullTimeout)
	_, err = s.Git(pullCtx, s.Repo, sshEnv(settings.DeployKeyFile), "pull", "--ff-only", "--quiet")
	if err == nil {
		err = pullCtx.Err() // a Git that returned only after the deadline still timed out
	}
	cancel()
	if err != nil {
		s.Log.Error("config sync: git pull failed", "repo", s.Repo, "err", err)
		return o.set(Failed, "git pull: "+err.Error(), err)
	}
	head, err := s.Git(ctx, s.Repo, nil, "rev-parse", "HEAD")
	if err != nil {
		return o.set(Failed, "git rev-parse: "+err.Error(), err)
	}
	head = strings.TrimSpace(head)
	o.Commit = short(head)
	previous, err := currentRelease(liveDir)
	if err != nil {
		return o.set(Failed, err.Error(), err)
	}
	if previous == head {
		s.Log.Info("config sync: no change", "commit", o.Commit)
		return o.set(OK, "no change", nil)
	}
	releases := liveDir + ".releases"
	rel := filepath.Join(releases, head)
	if err := buildRelease(s.Repo, rel); err != nil {
		return o.set(Failed, "copy the new config: "+err.Error(), err)
	}
	if err := checkRelease(filepath.Join(rel, name), rel); err != nil {
		s.Log.Error("config sync: new config rejected", "commit", o.Commit, "err", err)
		_ = os.RemoveAll(rel)
		return o.set(Rejected, err.Error()+"; the bot keeps its current config", err)
	}
	if err := swap(liveDir, filepath.Join(filepath.Base(releases), head)); err != nil {
		return o.set(Failed, "swap in the new config: "+err.Error(), err)
	}
	o.Swapped = true
	s.Log.Info("config sync: new config swapped in", "commit", o.Commit, "previous", short(previous))
	if err := prune(releases, head, previous); err != nil {
		s.Log.Warn("config sync: could not remove old releases", "err", err)
	}
	pid, command, held, err := s.Holder(settings.DataDir)
	switch {
	case err != nil:
		return o.set(Failed, "swapped in, but the running bot could not be found ("+err.Error()+
			"): send /reload in the admin chat", err)
	case !held || command != "run":
		return o.set(OK, "swapped in; the bot is not running, it loads this config when it starts", nil)
	}
	if err := s.Signal(pid); err != nil {
		return o.set(Failed, "swapped in, but the bot could not be asked to reload ("+err.Error()+
			"): send /reload in the admin chat", err)
	}
	o.Reloaded = true
	s.Log.Info("config sync: asked the bot to reload", "pid", pid)
	return o.set(OK, "swapped in; the bot was asked to reload", nil)
}

// settings is the live config, or before the first swap the repo's copy.
func (s *Sync) settings(name string) (*config.Config, error) {
	l, err := config.Read(s.Live)
	if err == nil {
		return l.Config, nil
	}
	if _, serr := os.Stat(s.Live); serr == nil {
		return nil, err
	}
	l, rerr := config.Read(filepath.Join(s.Repo, name))
	if rerr != nil {
		return nil, fmt.Errorf("%v; and the repo's copy: %w", err, rerr)
	}
	return l.Config, nil
}

// sshEnv makes git use the deploy key, and never prompt.
func sshEnv(key string) []string {
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	if key != "" { // git runs GIT_SSH_COMMAND through the shell: quote the path
		env = append(env, "GIT_SSH_COMMAND=ssh -i '"+strings.ReplaceAll(key, "'", `'\''`)+"' -o IdentitiesOnly=yes -o BatchMode=yes")
	}
	return env
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// currentRelease is the commit the live directory points at ("" before the
// first swap). A live directory that is not a symlink cannot be swapped.
func currentRelease(liveDir string) (string, error) {
	fi, err := os.Lstat(liveDir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return "", fmt.Errorf("%s is a directory, not the symlink the sync swaps: move it aside and run the sync "+
			"again (docs/deploy.md)", liveDir)
	}
	target, err := os.Readlink(liveDir)
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

// buildRelease copies the repo's tree (without .git) to rel.
func buildRelease(repo, rel string) error {
	tmp := rel + ".tmp"
	_ = os.RemoveAll(tmp)
	_ = os.RemoveAll(rel)
	err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(repo, path)
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		out := filepath.Join(tmp, relPath)
		switch {
		case d.IsDir():
			return os.MkdirAll(out, 0o700)
		case d.Type().IsRegular():
			return copyFile(path, out)
		}
		return fmt.Errorf("%s is not a regular file (symlinks are not synced)", relPath)
	})
	if err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return os.Rename(tmp, rel)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) // #nosec G304 -- a file of the operator's config repo
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- inside the release dir
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// checkRelease runs `groupwarden check` (config.Load, with the corpus test)
// on the new copy, and refuses a config whose data dir or secrets would
// live inside the synced tree.
func checkRelease(path, rel string) error {
	l, err := config.Load(path)
	if err != nil {
		return err
	}
	for key, p := range map[string]string{"data_dir": l.Config.DataDir, "secrets_file": l.Config.SecretsFile,
		"deploy_key_file": l.Config.DeployKeyFile} {
		if p != "" && inside(p, rel) {
			return fmt.Errorf("%s must be an absolute path outside the config repo (it is %s)", key, p)
		}
	}
	return nil
}

func inside(path, dir string) bool {
	r, err := filepath.Rel(dir, path)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

// swap points liveDir at target in one rename.
func swap(liveDir, target string) error {
	tmp := liveDir + ".swap"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, liveDir)
}

// prune removes every release but the current and the previous one.
func prune(releases string, keep ...string) error {
	entries, err := os.ReadDir(releases)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if keep[0] == e.Name() || keep[1] == e.Name() {
			continue
		}
		errs = append(errs, os.RemoveAll(filepath.Join(releases, e.Name())))
	}
	return errors.Join(errs...)
}

// NewEpisode reports whether a failed outcome needs an alert, given the
// previous result line: a change of status starts an episode, and so does a
// different commit being rejected.
func NewEpisode(previous string, o Outcome) bool {
	if o.Status == OK {
		return false
	}
	f := strings.Fields(previous)
	if len(f) < 2 || f[0] != o.Status {
		return true
	}
	return o.Status == Rejected && strings.TrimSuffix(f[1], ":") != o.Commit
}

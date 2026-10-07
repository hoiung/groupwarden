package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/configsync"
	"github.com/hoiung/groupwarden/internal/store"
)

// runGit runs the git binary in dir.
func runGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) // #nosec G204 -- fixed binary; the args are the sync's own
	cmd.Env = append(os.Environ(), env...)
	// Once ctx ends git is killed, but its ssh child can keep the output pipe
	// open; stop waiting for it after this.
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// hangup asks a process to reload its config.
func hangup(pid int) error { return syscall.Kill(pid, syscall.SIGHUP) }

// syncConfig runs one config sync (internal/configsync) for the bot whose
// config file is livePath, from the staging clone repo. It prints the result
// line, records it and the run time for /status and the overdue check, and
// alerts once per failure episode. Exit 1 when the sync failed or the new
// config was rejected.
func (e *env) syncConfig(ctx context.Context, livePath, repo string, log *slog.Logger) int {
	if repo == "" {
		fmt.Fprintln(e.stderr, "usage: groupwarden sync-config --config <live config file> --repo <staging clone>")
		return exitUsage
	}
	s := &configsync.Sync{Live: livePath, Repo: repo, Git: e.git, PullTimeout: configsync.PullTimeout,
		Holder: app.LockHolder, Signal: e.signal, Log: log}
	o := s.Run(ctx)
	fmt.Fprintln(e.stdout, o.Result)
	if o.Settings == nil {
		return exitFail
	}
	st, err := store.Open(ctx, o.Settings.StoreDB(), store.Options{})
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	defer func() { _ = st.Close() }()
	status, err := st.Status(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	var report *store.Report
	if configsync.NewEpisode(status[store.StatusSyncResult].Value, o) {
		report = &store.Report{Kind: string(alert.SyncFailed),
			Text: "The config sync FAILED: " + o.Err.Error() + ". The bot keeps its current config; it tries again " +
				"every " + strconv.Itoa(o.Settings.ConfigSyncMinutes) + " minutes."}
		if o.Status == configsync.Rejected {
			report.Kind = string(alert.ConfigRejected)
			report.Text = "Config commit " + o.Commit + " was REJECTED by the check: " + o.Err.Error() +
				". The bot keeps its current config; fix the config repo."
		}
	}
	if err := st.SetStatusReporting(ctx, map[string]string{
		store.StatusSyncLastRun: store.StatusTime(e.now()),
		store.StatusSyncResult:  o.Result,
	}, report); err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	if o.Status != configsync.OK {
		return exitFail
	}
	return exitOK
}

// schedule prints the systemd OnCalendar value for a timer of cfg (the
// install script renders the timer from it). A sync never waits longer than
// config_sync_minutes: under an hour it runs every N minutes from the top of
// the hour, from an hour up every whole number of hours from midnight (the
// last gap of each hour or day may be shorter), and from a day up at
// midnight (systemd refuses an hour step of 24).
func (e *env) schedule(cfg *config.Config, timer string) int {
	if timer != "sync" {
		fmt.Fprintln(e.stderr, "usage: groupwarden schedule sync")
		return exitUsage
	}
	fmt.Fprintln(e.stdout, onCalendar(cfg.ConfigSyncMinutes))
	return exitOK
}

func onCalendar(minutes int) string {
	switch {
	case minutes < 60:
		return fmt.Sprintf("*-*-* *:00/%d:00", minutes)
	case minutes < 24*60:
		return fmt.Sprintf("*-*-* 00/%d:00:00", minutes/60)
	}
	return "*-*-* 00:00:00"
}

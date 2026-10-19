package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/backup"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/store"
)

// backup makes one encrypted backup of groupwarden.db (internal/backup). It
// prints the result line, records it and the run time for /status and the
// overdue check, and alerts the admins once per failure episode. Exit 1 when
// it failed. The target is checked first, as `check --secrets` does: a
// network share that is not mounted leaves its mount point an empty folder on
// the local disk, and a backup written there would be on the disk it guards.
func (e *env) backup(ctx context.Context, cfg *config.Config, log *slog.Logger) int {
	if _, err := os.Stat(cfg.StoreDB()); err != nil { // #nosec G703 -- operator-configured data dir
		fmt.Fprintf(e.stdout, "FAILED: no groupwarden.db to back up (%v)\n", err)
		return exitFail
	}
	st, err := store.Open(ctx, cfg.StoreDB(), store.Options{})
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	defer func() { _ = st.Close() }()
	var res backup.Result
	err = e.backupTarget(cfg)
	if err != nil {
		err = fmt.Errorf("backup.target_dir cannot take the backup: %w", err)
		log.Error("backup target refused", "err", err)
	} else {
		res, err = backup.Run(ctx, st, backup.Options{TargetDir: cfg.Backup.TargetDir, Recipient: cfg.Backup.AgeRecipient,
			Keep: cfg.Backup.Keep, ScratchDir: cfg.DataDir, Now: e.now(), Log: log})
	}
	result := fmt.Sprintf("ok %s (%d bytes, %d older deleted)", filepath.Base(res.File), res.Bytes, len(res.Removed))
	if err != nil {
		result = "FAILED: " + err.Error()
	}
	fmt.Fprintln(e.stdout, result)
	// INVOCATION_ID: systemd's ID of this run, which the failure handler
	// compares (empty when run by hand).
	if code := e.recordBackup(ctx, st, cfg, result, err, e.getenv("INVOCATION_ID")); code != exitOK || err != nil {
		return exitFail
	}
	return exitOK
}

// recordBackup records a backup run's result line and time, for /status and
// the overdue check, and reports a failure to the admins once per failure
// episode.
func (e *env) recordBackup(ctx context.Context, st *store.Store, cfg *config.Config, result string, failure error,
	invocation string) int {
	status, err := st.Status(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	var report *store.Report
	if failure != nil && !strings.HasPrefix(status[store.StatusBackupResult].Value, "FAILED") {
		report = &store.Report{Kind: string(alert.BackupFailed),
			Text: "A backup FAILED: " + failure.Error() + ". Earlier backups in " + cfg.Backup.TargetDir +
				" are kept; the nightly backup tries again tomorrow night, and an upgrade with deploy/install.sh stops " +
				"until a backup works (docs/runbook.md)."}
	}
	if err := st.SetStatusReporting(ctx, map[string]string{
		store.StatusBackupLastRun:    store.StatusTime(e.now()),
		store.StatusBackupResult:     result,
		store.StatusBackupInvocation: invocation,
	}, report); err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	return exitOK
}

// exitNamespace is systemd's exit status for a unit it could not set up:
// one of the paths it must make writable could not be opened.
const exitNamespace = "226"

// backupFailed is the backup unit's failure handler: systemd runs it
// (OnFailure=) with the failed run's outcome in MONITOR_*. A run that recorded
// its own result (a backup that failed and said why) needs nothing more; the
// run is matched by its invocation ID (systemd 251 and later pass it).
// Otherwise the unit stopped before the backup recorded anything: systemd
// could not open backup.target_dir to start it (a network share that cannot
// be reached: the NAS is off, the network is down, the password changed), or
// the run was stopped at its time limit. That is recorded and reported the
// same way as a failed backup, the same night.
func (e *env) backupFailed(ctx context.Context, cfg *config.Config, log *slog.Logger) int {
	if _, err := os.Stat(cfg.StoreDB()); err != nil { // #nosec G703 -- operator-configured data dir
		fmt.Fprintf(e.stderr, "groupwarden: no groupwarden.db to record the failure in (%v)\n", err)
		return exitFail
	}
	st, err := store.Open(ctx, cfg.StoreDB(), store.Options{})
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
	run, recorded := e.getenv("MONITOR_INVOCATION_ID"), status[store.StatusBackupResult].Value
	switch {
	case run != "" && status[store.StatusBackupInvocation].Value == run:
		fmt.Fprintln(e.stdout, "the failed backup recorded its own result: "+recorded)
		return exitOK
	case run == "" && strings.HasPrefix(recorded, "FAILED"):
		// systemd before 251 does not say which run failed. A failed run
		// records FAILED or nothing, so a recorded failure may be this run's
		// own reason: it is kept (its episode was reported already). The
		// run time is still recorded: the timer did run, and the overdue
		// check must not blame it for a share that stays unreachable.
		log.Warn("systemd did not say which backup run failed (before systemd 251); the failure already recorded is kept",
			"result", recorded)
		if err := st.SetStatus(ctx, map[string]string{store.StatusBackupLastRun: store.StatusTime(e.now())}); err != nil {
			fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
			return exitFail
		}
		fmt.Fprintln(e.stdout, "a failed backup is already recorded: "+recorded)
		return exitOK
	}
	detail := "systemd did not say how: see journalctl -u groupwarden-backup.service"
	if result := e.getenv("MONITOR_SERVICE_RESULT"); result != "" {
		detail = fmt.Sprintf("systemd: %s, %s %s", result, e.getenv("MONITOR_EXIT_CODE"), e.getenv("MONITOR_EXIT_STATUS"))
	}
	why := "the backup unit stopped before the backup recorded a result (" + detail + ")"
	if e.getenv("MONITOR_EXIT_STATUS") == exitNamespace {
		why += "; systemd could not open " + cfg.Backup.TargetDir + " to start it: is the network share reachable " +
			"(the NAS on, the network up, the password current)?"
	}
	log.Error("backup unit failed", "err", why)
	result := "FAILED: " + why
	fmt.Fprintln(e.stdout, result)
	return e.recordBackup(ctx, st, cfg, result, errors.New(why), run)
}

// restore puts a backup back as groupwarden.db. It takes the data-dir lock,
// so it cannot run beside the bot; the restored database keeps every action
// paused until an admin resumes.
func (e *env) restore(ctx context.Context, cfg *config.Config, identity string, args []string) int {
	if len(args) != 1 || identity == "" {
		fmt.Fprintln(e.stderr, "usage: groupwarden restore --identity <age key file> <backup file>")
		return exitUsage
	}
	lock, err := app.AcquireLock(cfg.DataDir, "restore")
	if errors.Is(err, app.ErrAlreadyRunning) {
		fmt.Fprintln(e.stderr, "groupwarden: groupwarden is running: stop groupwarden.service first")
		return exitFail
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	defer lock.Release()
	if err := backup.Restore(ctx, args[0], identity, cfg.StoreDB()); err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	fmt.Fprintf(e.stdout, "restored %s as %s: every action stays PAUSED until an admin checks /status and presses [Resume]\n",
		args[0], cfg.StoreDB())
	return exitOK
}

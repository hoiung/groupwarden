package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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
// it failed.
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
	res, err := backup.Run(ctx, st, backup.Options{TargetDir: cfg.Backup.TargetDir, Recipient: cfg.Backup.AgeRecipient,
		Keep: cfg.Backup.Keep, ScratchDir: cfg.DataDir, Now: e.now(), Log: log})
	result := fmt.Sprintf("ok %s (%d bytes, %d older deleted)", filepath.Base(res.File), res.Bytes, len(res.Removed))
	if err != nil {
		result = "FAILED: " + err.Error()
	}
	fmt.Fprintln(e.stdout, result)
	status, serr := st.Status(ctx)
	if serr != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", serr)
		return exitFail
	}
	var report *store.Report
	if err != nil && !strings.HasPrefix(status[store.StatusBackupResult].Value, "FAILED") {
		report = &store.Report{Kind: string(alert.BackupFailed), Priority: true,
			Text: "The nightly backup FAILED: " + err.Error() + ". Earlier backups in " + cfg.Backup.TargetDir +
				" are kept; it tries again tomorrow night (docs/runbook.md)."}
	}
	if serr := st.SetStatusReporting(ctx, map[string]string{
		store.StatusBackupLastRun: strconv.FormatInt(e.now().UnixMilli(), 10),
		store.StatusBackupResult:  result,
	}, report); serr != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", serr)
		return exitFail
	}
	if err != nil {
		return exitFail
	}
	return exitOK
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

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/store"
)

// check validates the config and, with --secrets, every machine-checkable
// provisioning input, one `OK <item>` or `MISSING <item>: <why>` line each.
func (e *env) check(path string, secrets bool) int {
	l, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(e.stdout, "MISSING config: %v\n", err)
		return exitFail
	}
	cfg := l.Config
	fmt.Fprintf(e.stdout, "OK config v%s\n", l.Hash)
	if !secrets {
		return exitOK
	}
	missing := 0
	report := func(item string, err error) {
		if err != nil {
			missing++
			fmt.Fprintf(e.stdout, "MISSING %s: %v\n", item, err)
			return
		}
		fmt.Fprintf(e.stdout, "OK %s\n", item)
	}
	report("data-dir", linuxDir(cfg.DataDir))
	report("config-dir", linuxDir(cfg.Dir))
	kv, secErr := config.ReadSecretsFile(cfg.SecretsFile)
	if secErr != nil {
		report("telegram-token", secErr)
		report("telegram-chat-id", secErr)
	} else {
		report("telegram-token", config.CheckTelegramToken(kv[config.KeyTelegramBot]))
		_, err := config.ParseChatID(kv[config.KeyTelegramChatID])
		report("telegram-chat-id", err)
	}
	report("age-public-key", ageRecipient(cfg.Backup.AgeRecipient))
	report("deploy-key", deployKey(cfg.DeployKeyFile))
	report("backup-target", e.backupTarget(cfg))
	if missing > 0 {
		return exitFail
	}
	return exitOK
}

func linuxDir(dir string) error {
	info, err := os.Stat(dir) // #nosec G703 -- an operator-configured directory
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return store.CheckLinuxFS(dir)
}

func ageRecipient(v string) error {
	if v == "" {
		return errors.New("backup.age_recipient is not set")
	}
	if strings.HasPrefix(strings.ToUpper(v), "AGE-SECRET-KEY-") {
		return errors.New("backup.age_recipient holds a PRIVATE key: put the public key (age1...) in config and keep the private key off this node")
	}
	if _, err := age.ParseX25519Recipient(v); err != nil {
		return fmt.Errorf("backup.age_recipient is not an age public key: %w", err)
	}
	return nil
}

func deployKey(path string) error {
	if path == "" {
		return errors.New("deploy_key_file is not set")
	}
	info, err := os.Stat(path) // #nosec G703 -- the operator names the deploy key
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by others (mode %04o); run chmod 600", path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path) // #nosec G304 G703 -- the operator names the deploy key
	if err != nil {
		return err
	}
	if !strings.Contains(string(raw), "PRIVATE KEY-----") {
		return fmt.Errorf("%s is not an SSH private key", path)
	}
	return nil
}

// backupTarget must be a writable directory on the Linux filesystem, on a
// different disk from the data dir.
func (e *env) backupTarget(cfg *config.Config) error {
	dir := cfg.Backup.TargetDir
	if err := linuxDir(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".groupwarden-check-*")
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil { // #nosec G703 -- the probe file this function just created
		return err
	}
	bd, err := e.deviceOf(dir)
	if err != nil {
		return err
	}
	dd, err := e.deviceOf(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	if bd == dd {
		return fmt.Errorf("%s is on the same disk as the data dir; a disk failure would take both", dir)
	}
	return nil
}

// healthcheck reads run's status from the store (no lock, no WhatsApp) and
// exits 0 only when run is alive, connected, not deaf and has its config.
func (e *env) healthcheck(ctx context.Context, cfg *config.Config) int {
	if _, err := os.Stat(cfg.StoreDB()); err != nil { // #nosec G703 -- operator-configured data dir
		fmt.Fprintln(e.stdout, "running: no (no database yet)")
		return exitFail
	}
	st, err := store.Open(ctx, cfg.StoreDB(), store.Options{})
	if err != nil {
		fmt.Fprintf(e.stdout, "running: unknown (%v)\n", err)
		return exitFail
	}
	defer st.Close()
	status, err := st.Status(ctx)
	if err != nil {
		fmt.Fprintf(e.stdout, "running: unknown (%v)\n", err)
		return exitFail
	}
	now := e.now()
	h := app.ReadHealth(status, now)
	if h.Running {
		fmt.Fprintf(e.stdout, "running: yes (heartbeat %s ago)\n", h.BeatAge.Round(time.Second))
	} else {
		fmt.Fprintln(e.stdout, "running: no (no recent heartbeat)")
	}
	fmt.Fprintf(e.stdout, "connected: %s\n", yesNo(h.Connected))
	fmt.Fprintf(e.stdout, "deaf: %s\n", yesNo(h.Deaf))
	if h.ConfigHash != "" {
		fmt.Fprintf(e.stdout, "config: v%s\n", h.ConfigHash)
	} else {
		fmt.Fprintln(e.stdout, "config: not loaded")
	}
	switch h.Telegram {
	case "1":
		fmt.Fprintln(e.stdout, "telegram: ok")
	case "0":
		fmt.Fprintf(e.stdout, "telegram: REFUSED (marked unhealthy at %s: bot token revoked or bot removed from the admin chat)\n",
			h.TelegramSince.UTC().Format(time.RFC3339))
	default:
		fmt.Fprintln(e.stdout, "telegram: not reached yet")
	}
	if last := status[store.StatusLastEvent].Time(); !last.IsZero() {
		fmt.Fprintf(e.stdout, "last event: %s ago\n", now.Sub(last).Round(time.Second))
	} else {
		fmt.Fprintln(e.stdout, "last event: none yet")
	}
	if n, err := st.OutboxLen(ctx); err == nil {
		fmt.Fprintf(e.stdout, "queued actions: %d\n", n)
	} else {
		fmt.Fprintf(e.stdout, "queued actions: unknown (%v)\n", err)
	}
	pauses, err := st.Pauses(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(e.stdout, "paused: yes (pause state unreadable: %v)\n", err)
	case len(pauses) == 0:
		fmt.Fprintln(e.stdout, "paused: no")
	default:
		for _, p := range pauses {
			fmt.Fprintf(e.stdout, "paused: %s since %s (%s): %s\n", p.Scope.Stops(), p.Since.UTC().Format(time.RFC3339), p.Source,
				p.Reason)
		}
	}
	if !h.OK() {
		return exitFail
	}
	return exitOK
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

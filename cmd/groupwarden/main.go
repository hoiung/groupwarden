// Command groupwarden is a self-hosted anti-spam moderator for WhatsApp
// Communities. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/whatsmeow"
	"github.com/hoiung/groupwarden/internal/config"
)

// Exit codes. ExitFatal (app.ExitFatal) means a human must act; systemd does
// not restart on it.
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// env is everything a command touches outside its own logic; tests replace it.
type env struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	now            func() time.Time
	// openAdapter builds the WhatsApp adapter for cfg.
	openAdapter func(ctx context.Context, cfg *config.Config, log *slog.Logger) (client.Adapter, error)
	// deviceOf returns the device (disk) a path lives on.
	deviceOf func(path string) (uint64, error)
	// signals returns a context cancelled on SIGINT/SIGTERM.
	signals func() (context.Context, context.CancelFunc)
	// connectTimeout bounds the wait for a one-off connection.
	connectTimeout time.Duration
}

func main() {
	e := &env{
		stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv, now: time.Now,
		openAdapter: func(ctx context.Context, cfg *config.Config, log *slog.Logger) (client.Adapter, error) {
			return whatsmeow.Open(ctx, whatsmeow.Options{DataDir: cfg.DataDir, Log: log})
		},
		deviceOf: deviceOf,
		signals: func() (context.Context, context.CancelFunc) {
			return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		},
		connectTimeout: 90 * time.Second,
	}
	os.Exit(e.run(os.Args[1:]))
}

const usage = `usage: groupwarden <command> [flags]

WhatsApp commands (take the data-dir lock; only one at a time):
  pair [--phone <digits>]      link the bot phone (QR code, or a pairing code with --phone)
  run                          moderate
  groups                       list joined communities and groups with their IDs
  resolve-link <invite link>   show the group and community a link points to, without joining

Store commands (work beside run):
  check [--secrets]            validate the config; --secrets lists each input as OK or MISSING
  healthcheck                  exit 0 only when run is connected, not deaf and has its config
  fatal-exit-code              print the exit code that means "a human must act"

Every command except fatal-exit-code takes --config <file> (default: $GROUPWARDEN_CONFIG).
`

func (e *env) run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "fatal-exit-code":
		fmt.Fprintln(e.stdout, app.ExitFatal)
		return exitOK
	case "pair", "run", "groups", "resolve-link", "check", "healthcheck":
	case "-h", "--help", "help":
		fmt.Fprint(e.stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(e.stderr, "unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	cfgPath := fs.String("config", e.getenv("GROUPWARDEN_CONFIG"), "config file")
	phone := fs.String("phone", "", "pair with a code for this number (digits with country code)")
	secrets := fs.Bool("secrets", false, "check every provisioning input")
	if err := fs.Parse(rest); err != nil {
		return exitUsage
	}
	if *cfgPath == "" {
		fmt.Fprintln(e.stderr, "no config: pass --config <file> or set GROUPWARDEN_CONFIG")
		return exitUsage
	}
	if cmd == "check" {
		return e.check(*cfgPath, *secrets)
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(e.stderr, "refusing to start: %v\n", err)
		return exitFail
	}
	log := slog.New(slog.NewJSONHandler(e.stderr, nil))
	ctx, stop := e.signals()
	defer stop()
	switch cmd {
	case "healthcheck":
		return e.healthcheck(ctx, cfg)
	case "pair":
		return e.withWhatsApp(ctx, cfg, log, func(ctx context.Context, w *whatsApp) error {
			p, ok := w.adapter.(interface {
				Pair(ctx context.Context, phone string, out io.Writer) error
			})
			if !ok {
				return errors.New("this adapter cannot pair")
			}
			return p.Pair(ctx, *phone, e.stdout)
		})
	case "run":
		return e.runBot(ctx, cfg, log)
	case "groups":
		return e.withWhatsApp(ctx, cfg, log, func(ctx context.Context, w *whatsApp) error { return e.groups(ctx, w) })
	case "resolve-link":
		if fs.NArg() != 1 {
			fmt.Fprintln(e.stderr, "usage: groupwarden resolve-link <invite link>")
			return exitUsage
		}
		return e.withWhatsApp(ctx, cfg, log, func(ctx context.Context, w *whatsApp) error {
			return e.resolveLink(ctx, w, fs.Arg(0))
		})
	}
	return exitUsage
}

func deviceOf(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return st.Dev, nil
}

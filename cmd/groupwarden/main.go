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
	"slices"
	"strings"
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
	// hangups returns a channel that receives on each SIGHUP (a reload
	// request) and a function that stops it; nil means no reloads.
	hangups func() (<-chan struct{}, func())
	// connectTimeout bounds the wait for a one-off connection.
	connectTimeout time.Duration
	// telegramURL replaces api.telegram.org ("" = the real one; tests run a
	// fake Bot API).
	telegramURL string
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
		hangups:        hangups,
		connectTimeout: 90 * time.Second,
	}
	os.Exit(e.run(os.Args[1:]))
}

// hangups turns SIGHUP into reload requests (one pending at most).
func hangups() (<-chan struct{}, func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sig:
				select {
				case out <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	return out, func() { signal.Stop(sig); close(done) }
}

const usage = `usage: groupwarden <command> [flags]

WhatsApp commands (take the data-dir lock; only one at a time):
  pair [--phone <digits>]      link the bot phone (QR code, or a pairing code with --phone)
  run                          moderate (SIGHUP reloads the config)
  groups                       list joined communities and groups with their IDs
  resolve-link <invite link>   show the group and community a link points to, without joining

Store commands (work beside run):
  check [--secrets]            validate the config; --secrets lists each input as OK or MISSING
  healthcheck                  exit 0 only when run is connected, not deaf and has its config
  corpus test --corpus <dir>   test every rule, as if enforced, against labelled samples
  ledger summary               count shadow and enforce actions per community
  ban add|remove <lid|phone>   add to or lift from the ban list [--community <id>]
  ban list                     print the ban list
  member show <lid|phone>      print everything held about one person (JSON)
  member forget <lid|phone>    delete it (an active ban is kept)
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
	case "corpus", "ledger", "ban", "member":
		subs := map[string][]string{"corpus": {"test"}, "ledger": {"summary"}, "ban": {"add", "remove", "list"},
			"member": {"show", "forget"}}[cmd]
		if len(rest) == 0 || !slices.Contains(subs, rest[0]) {
			fmt.Fprintf(e.stderr, "usage: groupwarden %s %s ...\n", cmd, strings.Join(subs, "|"))
			return exitUsage
		}
		cmd, rest = cmd+" "+rest[0], rest[1:]
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
	corpusDir := fs.String("corpus", "", "corpus directory (spam/ and legit/<class>/)")
	community := fs.String("community", "", "the community a ban applies to (bans.scope per_community)")
	args, err := parseInterspersed(fs, rest)
	if err != nil {
		return exitUsage
	}
	if *cfgPath == "" {
		fmt.Fprintln(e.stderr, "no config: pass --config <file> or set GROUPWARDEN_CONFIG")
		return exitUsage
	}
	switch cmd {
	case "check":
		return e.check(*cfgPath, *secrets)
	case "corpus test":
		return e.corpusTest(*cfgPath, *corpusDir)
	}
	log := slog.New(slog.NewJSONHandler(e.stderr, nil))
	ctx, stop := e.signals()
	defer stop()
	if cmd == "run" {
		return e.runBot(ctx, *cfgPath, log)
	}
	l, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(e.stderr, "refusing to start: %v\n", err)
		return exitFail
	}
	cfg := l.Config
	switch cmd {
	case "healthcheck":
		return e.healthcheck(ctx, cfg)
	case "ledger summary":
		return e.ledgerSummary(ctx, cfg)
	case "ban add", "ban remove", "ban list":
		return e.ban(ctx, l, strings.TrimPrefix(cmd, "ban "), args, *community)
	case "member show", "member forget":
		return e.member(ctx, cfg, strings.TrimPrefix(cmd, "member "), args)
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
	case "groups":
		return e.withWhatsApp(ctx, cfg, log, func(ctx context.Context, w *whatsApp) error { return e.groups(ctx, w) })
	case "resolve-link":
		if len(args) != 1 {
			fmt.Fprintln(e.stderr, "usage: groupwarden resolve-link <invite link>")
			return exitUsage
		}
		return e.withWhatsApp(ctx, cfg, log, func(ctx context.Context, w *whatsApp) error {
			return e.resolveLink(ctx, w, args[0])
		})
	}
	return exitUsage
}

// parseInterspersed parses flags wherever they appear among the positional
// arguments ("ban add <phone> --community <id>"); the standard parser stops
// at the first positional one.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func deviceOf(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return st.Dev, nil
}

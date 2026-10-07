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
	// The zone database inside the binary: TZ (daily_check_time's zone) works
	// in an image without /usr/share/zoneinfo.
	_ "time/tzdata"

	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/whatsmeow"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/configsync"
	"github.com/hoiung/groupwarden/internal/corpus"
	"github.com/hoiung/groupwarden/internal/mask"
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
	stdin          io.Reader
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
	// git runs git for the config sync; signal asks the running bot to
	// reload (SIGHUP).
	git    configsync.Git
	signal func(pid int) error
}

func main() {
	os.Exit(productionEnv().run(os.Args[1:]))
}

// productionEnv wires the real process, WhatsApp, git and signals.
func productionEnv() *env {
	return &env{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv, now: time.Now,
		openAdapter: func(ctx context.Context, cfg *config.Config, log *slog.Logger) (client.Adapter, error) {
			return whatsmeow.Open(ctx, whatsmeow.Options{DataDir: cfg.DataDir, Log: log})
		},
		deviceOf: deviceOf,
		signals: func() (context.Context, context.CancelFunc) {
			return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		},
		hangups:        hangups,
		connectTimeout: 90 * time.Second,
		git:            runGit,
		signal:         hangup,
	}
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

Commands that take the data-dir lock (only one at a time):
  pair [--phone <digits>]      link the bot phone (QR code, or a pairing code with --phone)
  run                          moderate (SIGHUP reloads the config)
  groups                       list joined communities and groups with their IDs
  resolve-link <invite link>   show the group and community a link points to, without joining
  restore --identity <age key file> <backup file>
                               put a backup back as groupwarden.db (stop run first); it starts paused

Store commands (work beside run):
  check [--secrets]            validate the config; --secrets lists each input as OK or MISSING
  healthcheck                  exit 0 only when run is connected, not deaf and has its config
  corpus test --corpus <dir>   test every rule, as if enforced, against labelled samples
  corpus add --label spam|legit --corpus <dir> [--type <type>] [--push-name <text>] [--note <text>] [--public]
                               save the message on stdin as one sample (no --config needed);
                               --public redacts fully, for tests/corpus
  ledger summary               count shadow and enforce actions per community
  ban add|remove <lid|phone>   add to or lift from the ban list [--community <id>]
  ban list                     print the ban list
  member show <lid|phone>      print everything held about one person (JSON)
  member forget <lid|phone>    delete it (an active ban is kept)
  sync-config --repo <dir>     pull the config repo clone, check it, swap it in, ask run to reload
                               (--config is the live config file; run by the sync timer)
  schedule sync                print the sync timer's OnCalendar value (the install script uses it)
  backup                       write an encrypted backup of groupwarden.db to backup.target_dir
                               and keep the newest backup.keep (run nightly by the backup timer)
  backup-dir                   print backup.target_dir (the install script lets the backup unit write there)
  fatal-exit-code             print the exit code that means "a human must act"

Every command except fatal-exit-code and corpus add takes --config <file> (default: $GROUPWARDEN_CONFIG).
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
	case "pair", "run", "groups", "resolve-link", "check", "healthcheck", "sync-config", "schedule", "backup",
		"backup-dir", "restore":
	case "corpus", "ledger", "ban", "member":
		subs := map[string][]string{"corpus": {"test", "add"}, "ledger": {"summary"}, "ban": {"add", "remove", "list"},
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
	community := fs.String("community", "", "the community a ban is added to or lifted in (bans.scope per_community only; "+
		"ban remove without it lifts every ban)")
	label := fs.String("label", "", "corpus add: spam or legit")
	sampleType := fs.String("type", "", "corpus add: text, image-caption, poll, contact, invite or event")
	pushName := fs.String("push-name", "", "corpus add: the sender's display name")
	note := fs.String("note", "", "corpus add: why it is spam or legit")
	public := fs.Bool("public", false, "corpus add: full redaction, for the public tests/corpus")
	repo := fs.String("repo", "", "sync-config: the staging clone of the private config repo")
	identity := fs.String("identity", "", "restore: the file holding the age private key the backups are encrypted to")
	args, err := parseInterspersed(fs, rest)
	if err != nil {
		return exitUsage
	}
	if cmd == "corpus add" {
		return e.corpusAdd(*corpusDir, *label, corpus.Sample{Type: *sampleType, PushName: *pushName, Note: *note}, *public)
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
	log := mask.JSONLogger(e.stderr, nil)
	ctx, stop := e.signals()
	defer stop()
	switch cmd {
	case "run":
		return e.runBot(ctx, *cfgPath, log)
	case "sync-config": // the live config may not exist yet (the first sync)
		return e.syncConfig(ctx, *cfgPath, *repo, log)
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
	case "backup":
		return e.backup(ctx, cfg, log)
	case "backup-dir":
		fmt.Fprintln(e.stdout, cfg.Backup.TargetDir)
		return exitOK
	case "restore":
		return e.restore(ctx, cfg, *identity, args)
	case "schedule":
		if len(args) != 1 {
			fmt.Fprintln(e.stderr, "usage: groupwarden schedule sync")
			return exitUsage
		}
		return e.schedule(cfg, args[0])
	case "ledger summary":
		return e.ledgerSummary(ctx, cfg)
	case "ban add", "ban remove", "ban list":
		return e.ban(ctx, l, strings.TrimPrefix(cmd, "ban "), args, *community)
	case "member show", "member forget":
		return e.member(ctx, cfg, strings.TrimPrefix(cmd, "member "), args)
	case "pair":
		return e.withWhatsApp(ctx, cmd, cfg, log, func(ctx context.Context, w *whatsApp) error {
			p, ok := w.adapter.(interface {
				Pair(ctx context.Context, phone string, out io.Writer) error
			})
			if !ok {
				return errors.New("this adapter cannot pair")
			}
			return p.Pair(ctx, *phone, e.stdout)
		})
	case "groups":
		return e.withWhatsApp(ctx, cmd, cfg, log, func(ctx context.Context, w *whatsApp) error { return e.groups(ctx, w) })
	case "resolve-link":
		if len(args) != 1 {
			fmt.Fprintln(e.stderr, "usage: groupwarden resolve-link <invite link>")
			return exitUsage
		}
		return e.withWhatsApp(ctx, cmd, cfg, log, func(ctx context.Context, w *whatsApp) error {
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

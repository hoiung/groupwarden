package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hoiung/groupwarden/internal/action"
	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/reconcile"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
)

// whatsApp is an open WhatsApp session: the data-dir lock, the store and the adapter.
type whatsApp struct {
	adapter client.Adapter
	store   *store.Store
	inbox   *pipeline.Inbox
	alerter *alertSink
	log     *slog.Logger
}

// alertSink sends alerts to the log until `run` points it at the admin chat
// (the store, opened first, reports a write failure through it).
type alertSink struct {
	mu sync.Mutex
	to alert.Alerter
}

func (s *alertSink) Alert(ctx context.Context, a alert.Alert) error {
	s.mu.Lock()
	to := s.to
	s.mu.Unlock()
	return to.Alert(ctx, a)
}

func (s *alertSink) set(a alert.Alerter) {
	s.mu.Lock()
	s.to = a
	s.mu.Unlock()
}

// withWhatsApp takes the single-instance lock, opens the store and the
// adapter, runs fn and releases everything. A second WhatsApp command fails
// at once: two connections on one session knock each other off.
func (e *env) withWhatsApp(ctx context.Context, command string, cfg *config.Config, log *slog.Logger, fn func(context.Context, *whatsApp) error) int {
	lock, err := app.AcquireLock(cfg.DataDir, command)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	defer lock.Release()
	alerter := &alertSink{to: alert.Log{Logger: log}}
	st, err := store.Open(ctx, cfg.StoreDB(), store.Options{OnWriteFailure: func(err error) {
		_ = alerter.Alert(context.WithoutCancel(ctx), alert.Alert{Kind: alert.StorageFailure, Priority: true,
			Text: "groupwarden.db cannot be written (" + err.Error() + "): every action is PAUSED until an admin resumes"})
	}})
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	defer st.Close()
	a, err := e.openAdapter(ctx, cfg, log)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	defer a.Close()
	w := &whatsApp{adapter: a, store: st, inbox: pipeline.NewInbox(st), alerter: alerter, log: log}
	if err := fn(ctx, w); err != nil {
		var fe *app.FatalError
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		if errors.As(err, &fe) {
			return app.ExitFatal
		}
		return exitFail
	}
	return exitOK
}

// onceSink is the sink of a one-off connection (groups, resolve-link). Any
// message that arrives meanwhile goes to the inbox, so `run` still decides it.
type onceSink struct {
	inbox *pipeline.Inbox
	ch    chan client.Lifecycle
}

func (s onceSink) Persist(ev client.Event) error { return s.inbox.Persist(ev) }
func (s onceSink) Lifecycle(l client.Lifecycle) {
	select {
	case s.ch <- l:
	default:
	}
}

// connect opens a one-off connection and waits until it is ready.
func (e *env) connect(ctx context.Context, w *whatsApp) error {
	s := onceSink{inbox: w.inbox, ch: make(chan client.Lifecycle, 8)}
	w.adapter.Events(s)
	if err := w.adapter.Connect(ctx); err != nil {
		return err
	}
	timeout := time.After(e.connectTimeout)
	for {
		select {
		case l := <-s.ch:
			switch {
			case l.Kind == client.Connected:
				return nil
			case l.Kind.Fatal():
				return &app.FatalError{Kind: l.Kind, Detail: l.Detail}
			case l.Kind == client.TemporaryBan:
				return fmt.Errorf("the bot number is temporarily banned: %s", l.Detail)
			}
		case <-timeout:
			return fmt.Errorf("no connection to WhatsApp within %s", e.connectTimeout)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// groups prints every joined community with its linked groups, then any
// standalone groups, one ID per line, for config.yaml.
func (e *env) groups(ctx context.Context, w *whatsApp) error {
	if err := e.connect(ctx, w); err != nil {
		return err
	}
	defer w.adapter.Disconnect()
	all, err := w.adapter.JoinedGroups(ctx)
	if err != nil {
		return fmt.Errorf("list groups: %w", err)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	children := map[client.JID][]client.Group{}
	var communities, standalone []client.Group
	for _, g := range all {
		switch {
		case g.IsCommunity:
			communities = append(communities, g)
		case g.Parent != "":
			children[g.Parent] = append(children[g.Parent], g)
		default:
			standalone = append(standalone, g)
		}
	}
	for _, c := range communities {
		fmt.Fprintf(e.stdout, "community %s %q\n", c.JID, c.Name)
		for _, g := range children[c.JID] {
			fmt.Fprintf(e.stdout, "  group %s %q%s\n", g.JID, g.Name, announcement(g))
		}
		delete(children, c.JID)
	}
	// Linked groups whose community the bot is not in.
	parents := make([]string, 0, len(children))
	for p := range children {
		parents = append(parents, string(p))
	}
	sort.Strings(parents)
	for _, p := range parents {
		fmt.Fprintf(e.stdout, "community %s (bot not a member)\n", p)
		for _, g := range children[client.JID(p)] {
			fmt.Fprintf(e.stdout, "  group %s %q%s\n", g.JID, g.Name, announcement(g))
		}
	}
	for _, g := range standalone {
		fmt.Fprintf(e.stdout, "group %s %q\n", g.JID, g.Name)
	}
	return nil
}

func announcement(g client.Group) string {
	if g.IsAnnouncement {
		return " (announcement)"
	}
	return ""
}

// resolveLink prints the group and community an invite link points to. It
// only looks the link up; it never joins.
func (e *env) resolveLink(ctx context.Context, w *whatsApp, link string) error {
	code, err := client.InviteCode(link)
	if err != nil {
		return err
	}
	if err := e.connect(ctx, w); err != nil {
		return err
	}
	defer w.adapter.Disconnect()
	g, parent, err := w.adapter.InviteInfo(ctx, code)
	if err != nil {
		return fmt.Errorf("look up invite link: %w", err)
	}
	fmt.Fprintf(e.stdout, "group %s %q\n", g.JID, g.Name)
	if parent != "" {
		fmt.Fprintf(e.stdout, "community %s\n", parent)
	} else {
		fmt.Fprintln(e.stdout, "community none (a standalone group)")
	}
	return nil
}

// runBot is `groupwarden run`. A config file that fails its checks still
// boots when data_dir holds the last good copy (and the admins are told it
// was REJECTED); with no good copy the bot refuses to start. The admin chat
// is required: without its token and chat ID nobody would hear a report or
// be able to press [Undo].
func (e *env) runBot(ctx context.Context, path string, log *slog.Logger) int {
	holder, rejected, err := config.Boot(path)
	if err != nil {
		fmt.Fprintf(e.stderr, "refusing to start: %v\n", err)
		return exitFail
	}
	cur := holder.Current()
	opts, err := adminChatOptions(cur.Config)
	if err != nil {
		fmt.Fprintf(e.stderr, "refusing to start: the Telegram admin chat: %v\n", err)
		return exitFail
	}
	opts.ServerURL = e.telegramURL
	var reload <-chan struct{}
	if e.hangups != nil {
		ch, stop := e.hangups()
		defer stop()
		reload = ch
	}
	return e.withWhatsApp(ctx, "run", cur.Config, log, func(ctx context.Context, w *whatsApp) error {
		log.Info("starting", "version", app.Version(), "config", "v"+cur.Hash)
		session, err := store.OpenSession(ctx, cur.Config.WhatsmeowDB(), nil)
		if err != nil {
			return err
		}
		defer session.Close()
		a, err := moderationApp(w, holder, session, opts, log)
		if err != nil {
			return err
		}
		a.Settings, a.Reload, a.BootRejected = app.SettingsFrom(cur.Config), reload, rejected
		err = a.Run(ctx)
		log.Info("stopped", "err", err)
		return err
	})
}

// adminChatOptions reads the admin chat's bot token and chat ID from the
// secrets file.
func adminChatOptions(cfg *config.Config) (telegram.Options, error) {
	secrets, err := config.ReadSecretsFile(cfg.SecretsFile)
	if err != nil {
		return telegram.Options{}, err
	}
	token := secrets[config.KeyTelegramBot]
	if err := config.CheckTelegramToken(token); err != nil {
		return telegram.Options{}, err
	}
	id, err := config.ParseChatID(secrets[config.KeyTelegramChatID])
	if err != nil {
		return telegram.Options{}, err
	}
	return telegram.Options{Token: token, ChatID: id}, nil // secret-allow (the variable read from the secrets file)
}

// moderationApp wires the inbox worker, the decision, the ledger, the admin
// chat and every moderation worker around one WhatsApp session.
func moderationApp(w *whatsApp, holder *config.Holder, session *store.Session, opts telegram.Options,
	log *slog.Logger) (*app.App, error) {
	dir := &pipeline.Directory{}
	chat, err := telegram.New(opts, &telegram.Chat{Store: w.store, Config: holder, Groups: dir, Log: log})
	if err != nil {
		return nil, err
	}
	w.alerter.set(chat)
	media := &action.MediaFetcher{Store: w.store, Adapter: w.adapter, Config: holder,
		Dir: filepath.Join(holder.Current().Config.DataDir, "evidence"), Log: log}
	exec := &action.Executor{Store: w.store, Adapter: w.adapter, Config: holder, Directory: dir, Alerter: w.alerter,
		Reported: chat.Wake, Log: log}
	wake := func() {
		exec.Wake()
		chat.Wake()
		media.Wake()
	}
	enforcer := &pipeline.Enforcer{Store: w.store, Config: holder, Directory: dir, Adapter: w.adapter, Wake: wake, Log: log}
	mod := &pipeline.Moderator{Store: w.store, Config: holder, Directory: dir, Enforcer: enforcer, Log: log}
	exec.Moderator = mod
	a := &app.App{
		Adapter: w.adapter, Store: w.store, Inbox: w.inbox, Alerter: w.alerter, Log: log, Clock: app.SystemClock{},
		Worker:    &pipeline.Worker{Store: w.store, Inbox: w.inbox, Config: holder, Log: log, Decider: mod},
		Config:    holder,
		Directory: dir,
		Executor:  exec,
		Media:     media,
		Purger: &ledger.Purger{Store: w.store, Config: holder, Session: session, AnnouncementChats: dir.AnnouncementGroups,
			Log: log, Now: time.Now},
		Sweep:     &reconcile.Sweep{Enforcer: enforcer, Directory: dir, Config: holder, Log: log, Sleep: action.Sleep},
		AdminChat: chat,
		Admin:     &pipeline.Admin{Store: w.store, Config: holder, Directory: dir, Enforcer: enforcer, Log: log},
	}
	chat.Controls = a.Controls()
	return a, nil
}

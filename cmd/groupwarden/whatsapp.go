package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
)

// whatsApp is an open WhatsApp session: the data-dir lock, the store and the adapter.
type whatsApp struct {
	adapter client.Adapter
	store   *store.Store
	inbox   *pipeline.Inbox
	alerter alert.Alerter
	log     *slog.Logger
}

// withWhatsApp takes the single-instance lock, opens the store and the
// adapter, runs fn and releases everything. A second WhatsApp command fails
// at once: two connections on one session knock each other off.
func (e *env) withWhatsApp(ctx context.Context, cfg *config.Config, log *slog.Logger, fn func(context.Context, *whatsApp) error) int {
	lock, err := app.AcquireLock(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	defer lock.Release()
	alerter := alert.Log{Logger: log}
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

// inviteCode takes the code out of an invite link (or a bare code).
func inviteCode(link string) (string, error) {
	s := strings.TrimSpace(link)
	s, _, _ = strings.Cut(s, "?")
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		if !strings.Contains(s[:i], "chat.whatsapp.com") {
			return "", fmt.Errorf("%q is not a WhatsApp group invite link", link)
		}
		s = s[i+1:]
	}
	if s == "" || strings.ContainsAny(s, " .:@") {
		return "", fmt.Errorf("%q is not a WhatsApp group invite link", link)
	}
	return s, nil
}

// resolveLink prints the group and community an invite link points to. It
// only looks the link up; it never joins.
func (e *env) resolveLink(ctx context.Context, w *whatsApp, link string) error {
	code, err := inviteCode(link)
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

// runBot is `groupwarden run`.
func (e *env) runBot(ctx context.Context, cfg *config.Config, log *slog.Logger) int {
	return e.withWhatsApp(ctx, cfg, log, func(ctx context.Context, w *whatsApp) error {
		log.Info("starting", "config", "v"+cfg.Hash())
		a := &app.App{
			Adapter: w.adapter, Store: w.store, Inbox: w.inbox, Alerter: w.alerter, Log: log, Clock: app.SystemClock{},
			Worker: &pipeline.Worker{Store: w.store, Inbox: w.inbox, MaxReplayAge: time.Duration(cfg.ActOnReplayMaxAge), Log: log,
				Decider: &pipeline.Moderator{Store: w.store, Alerter: w.alerter, Log: log}},
			Settings: app.Settings{
				DeafAfter:           time.Duration(cfg.DeafnessAlertHours) * time.Hour,
				DisconnectAlert:     time.Duration(cfg.DisconnectAlertMinutes) * time.Minute,
				CompanionCheckEvery: time.Duration(cfg.Reconcile.IntervalMinutes) * time.Minute,
				ConfigHash:          cfg.Hash(),
			},
		}
		err := a.Run(ctx)
		log.Info("stopped", "err", err)
		return err
	})
}

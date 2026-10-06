package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/hoiung/groupwarden/internal/action"
	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/reconcile"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
)

// AlertSink forwards alerts to the log until Build points it at the admin
// chat (the store, opened first, reports a write failure through it).
type AlertSink struct {
	mu sync.Mutex
	to alert.Alerter
}

// NewAlertSink starts a sink that sends to first.
func NewAlertSink(first alert.Alerter) *AlertSink { return &AlertSink{to: first} }

// Alert sends a to the current target.
func (s *AlertSink) Alert(ctx context.Context, a alert.Alert) error {
	s.mu.Lock()
	to := s.to
	s.mu.Unlock()
	return to.Alert(ctx, a)
}

// Set points the sink at a.
func (s *AlertSink) Set(a alert.Alerter) {
	s.mu.Lock()
	s.to = a
	s.mu.Unlock()
}

// Parts is one open WhatsApp session and what the bot needs around it.
type Parts struct {
	Adapter  client.Adapter
	Store    *store.Store
	Inbox    *pipeline.Inbox
	Config   *config.Holder
	Session  *store.Session // whatsmeow.db, for the retention purge
	Telegram telegram.Options
	// Alerts is pointed at the admin chat once it exists.
	Alerts *AlertSink
	Log    *slog.Logger
}

// Build wires the inbox worker, the decision, the ledger, the admin chat and
// every moderation worker around one WhatsApp session: the bot `groupwarden
// run` runs.
func Build(p Parts) (*App, error) {
	dir := &pipeline.Directory{}
	chat, err := telegram.New(p.Telegram, &telegram.Chat{Store: p.Store, Config: p.Config, Groups: dir, Log: p.Log})
	if err != nil {
		return nil, err
	}
	p.Alerts.Set(chat)
	media := &action.MediaFetcher{Store: p.Store, Adapter: p.Adapter, Config: p.Config,
		Dir: filepath.Join(p.Config.Current().Config.DataDir, "evidence"), Log: p.Log}
	exec := &action.Executor{Store: p.Store, Adapter: p.Adapter, Config: p.Config, Directory: dir, Alerter: p.Alerts,
		Reported: chat.Wake, Log: p.Log}
	wake := func() {
		exec.Wake()
		chat.Wake()
		media.Wake()
	}
	enforcer := &pipeline.Enforcer{Store: p.Store, Config: p.Config, Directory: dir, Adapter: p.Adapter, Wake: wake, Log: p.Log}
	mod := &pipeline.Moderator{Store: p.Store, Config: p.Config, Directory: dir, Enforcer: enforcer, Log: p.Log}
	exec.Moderator = mod
	a := &App{
		Adapter: p.Adapter, Store: p.Store, Inbox: p.Inbox, Alerter: p.Alerts, Log: p.Log, Clock: SystemClock{},
		Settings:  SettingsFrom(p.Config.Current().Config),
		Worker:    &pipeline.Worker{Store: p.Store, Inbox: p.Inbox, Config: p.Config, Log: p.Log, Decider: mod, Wake: chat.Wake},
		Config:    p.Config,
		Directory: dir,
		Executor:  exec,
		Media:     media,
		Purger: &ledger.Purger{Store: p.Store, Config: p.Config, Session: p.Session, AnnouncementChats: dir.AnnouncementGroups,
			Log: p.Log, Now: time.Now},
		Sweep:     &reconcile.Sweep{Enforcer: enforcer, Directory: dir, Config: p.Config, Log: p.Log, Sleep: action.Sleep},
		AdminChat: chat,
		Admin:     &pipeline.Admin{Store: p.Store, Config: p.Config, Directory: dir, Enforcer: enforcer, Log: p.Log},
	}
	chat.Controls = a.Controls()
	return a, nil
}

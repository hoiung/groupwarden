// Package modtest wires the whole moderation path (inbox worker, decision,
// ledger, executor, attachment fetcher) over a fake WhatsApp adapter and a
// fake clock, for tests in the packages that make it up.
package modtest

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/action"
	"github.com/hoiung/groupwarden/internal/alert/alerttest"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/config/configtest"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
)

// Synthetic IDs (each listed in .secret-pii-allowlist).
const (
	// Community is a WhatsApp Community with linked groups G1 and G2.
	Community client.JID = "99999000000999@g.us"
	G1        client.JID = "99999000000111@g.us"
	G2        client.JID = "99999000000222@g.us"
	// SetB is a configured set of standalone groups holding GB.
	SetB            = "set-b"
	GB   client.JID = "99999000000888@g.us"

	Spammer      client.JID = "99999000000444@lid"
	SpammerPhone client.JID = "447700900123@s.whatsapp.net"
	Admin        client.JID = "99999000000555@lid"
	AdminPhone   client.JID = "447700900456@s.whatsapp.net"
	Member       client.JID = "99999000000666@lid"
	Other1       client.JID = "99999000000777@lid"
	Other2       client.JID = "99999000000888@lid"
	Bot          client.JID = "99999000000999@lid"
	BotPhone     client.JID = "447700900789@s.whatsapp.net"
)

// T0 is the kit's start time.
var T0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// Config is the kit's moderation config: "pitch" (crypto word + link) acts
// and is confirmed; "lure" (crypto word + lure phrase) acts but is
// watch-only; both communities enforce.
const Config = `mode: enforce
word_lists:
  crypto: ["crypto", "bitcoin"]
  lures: ["inbox me"]
rules:
  list:
    - name: pitch
      action: delete_remove_ban
      confirmed: true
      when:
        all:
          - words: crypto
          - has: any_link
    - name: lure
      action: delete_remove_ban
      when:
        all:
          - words: crypto
          - words: lures
communities:
  "99999000000999@g.us":
    name: community a
  set-b:
    name: set b
    groups: ["99999000000888@g.us"]
`

// SpamText matches the confirmed "pitch" rule.
const SpamText = "cheap crypto signals https://example.org/join"

// Clock is a fake clock; Sleep and Advance move it, and an After channel
// fires once the clock reaches its time.
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	slept   []time.Duration
	waiters []waiter
	// OnSleep runs inside every Sleep, after the clock moved.
	OnSleep func(d time.Duration)
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

// Now is the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After returns a channel that receives once the clock has moved d on.
func (c *Clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.waiters = append(c.waiters, waiter{at: c.now.Add(d), ch: ch})
	return ch
}

// move advances the clock and fires every After now due (c.mu held).
func (c *Clock) move(d time.Duration) {
	c.now = c.now.Add(d)
	keep := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			keep = append(keep, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = keep
}

// Advance moves the clock on.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.move(d)
	c.mu.Unlock()
}

// Sleep records d and advances the clock by it.
func (c *Clock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.move(d)
	c.slept = append(c.slept, d)
	hook := c.OnSleep
	c.mu.Unlock()
	if hook != nil {
		hook(d)
	}
	return ctx.Err()
}

// Slept lists every Sleep duration, in order.
func (c *Clock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// Groups is what the fake adapter lists: the bot is an admin everywhere
// (super-admin of the community), Admin is a human admin everywhere, the
// spammer is in G1 and G2 by both addresses, Member is in G1 and GB.
func Groups() []client.Group {
	bot := client.Participant{JID: Bot, LID: Bot, Phone: BotPhone, IsAdmin: true}
	admin := client.Participant{JID: Admin, LID: Admin, Phone: AdminPhone, IsAdmin: true}
	spammer := client.Participant{JID: Spammer, LID: Spammer, Phone: SpammerPhone}
	member := client.Participant{JID: Member, LID: Member}
	superBot := bot
	superBot.IsSuperAdmin = true
	return []client.Group{
		{JID: Community, Name: "community a", IsCommunity: true, Participants: []client.Participant{superBot, admin}},
		{JID: G1, Name: "general", Parent: Community, Participants: []client.Participant{bot, admin, spammer, member}},
		{JID: G2, Name: "jobs", Parent: Community, Participants: []client.Participant{bot, admin, spammer}},
		{JID: GB, Name: "standalone", Participants: []client.Participant{bot, admin, member}},
	}
}

// Kit is the moderation path over fakes.
type Kit struct {
	T        testing.TB
	Ctx      context.Context
	Clock    *Clock
	DBPath   string
	CfgPath  string
	Store    *store.Store
	Holder   *config.Holder
	Dir      *pipeline.Directory
	Fake     *clienttest.Fake
	Alerts   *alerttest.Recorder
	Enforcer *pipeline.Enforcer
	Mod      *pipeline.Moderator
	Worker   *pipeline.Worker
	Exec     *action.Executor
	Media    *action.MediaFetcher
	// Log is every component's logger; Logged counts its lines.
	Log *slog.Logger

	base      string // the config the kit was built with
	logs      *logBuffer
	banOrders int // Ban calls so far
}

// logBuffer holds the kit's log lines (the executor and fetcher may log from
// other goroutines).
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Logged counts the log lines with message msg (info level and above).
func (k *Kit) Logged(msg string) int { return len(k.LogLines(msg)) }

// LogLines lists the log lines whose message is msg, oldest first. The text
// handler quotes a message only when it has to (msg=sweep, msg="a sweep").
func (k *Kit) LogLines(msg string) []string {
	k.logs.mu.Lock()
	defer k.logs.mu.Unlock()
	var out []string
	for _, l := range strings.Split(k.logs.buf.String(), "\n") {
		for _, m := range []string{" msg=" + strconv.Quote(msg), " msg=" + msg} {
			if strings.Contains(l, m+" ") || strings.HasSuffix(l, m) {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

// New builds a kit with Config plus extra.
func New(t testing.TB, extra string) *Kit {
	t.Helper()
	return NewConfig(t, Config+extra)
}

// NewConfig builds a kit with a whole moderation config of its own.
func NewConfig(t testing.TB, cfg string) *Kit {
	t.Helper()
	k := &Kit{T: t, Ctx: context.Background(), Clock: &Clock{now: T0}, Alerts: &alerttest.Recorder{}, base: cfg,
		logs: &logBuffer{}, DBPath: filepath.Join(t.TempDir(), "groupwarden.db")}
	k.Log = slog.New(slog.NewTextHandler(k.logs, nil))
	k.Holder, k.CfgPath = configtest.Holder(t, cfg)
	k.Fake = &clienttest.Fake{Groups: Groups(), SelfIDs: client.Self{Phone: BotPhone, LID: Bot}}
	k.Dir = &pipeline.Directory{}
	k.Dir.SetSelf(k.Fake.SelfIDs)
	Load(t, k.Dir, k.Fake.Groups)
	k.open()
	return k
}

// Load fills dir from groups, as a group list read from WhatsApp does.
func Load(t testing.TB, dir *pipeline.Directory, groups []client.Group) {
	t.Helper()
	if err := dir.Refresh(func() ([]client.Group, error) { return groups, nil }); err != nil {
		t.Fatal(err)
	}
}

func (k *Kit) open() {
	k.T.Helper()
	st, err := store.Open(k.Ctx, k.DBPath, store.Options{Now: k.Clock.Now})
	if err != nil {
		k.T.Fatal(err)
	}
	k.T.Cleanup(func() { _ = st.Close() })
	k.Store = st
	k.Exec = &action.Executor{Store: st, Adapter: k.Fake, Config: k.Holder, Directory: k.Dir, Alerter: k.Alerts,
		Log: k.Log, Now: k.Clock.Now, Sleep: k.Clock.Sleep}
	k.Enforcer = &pipeline.Enforcer{Store: st, Config: k.Holder, Directory: k.Dir, Adapter: k.Fake, Log: k.Log}
	k.Mod = &pipeline.Moderator{Store: st, Config: k.Holder, Directory: k.Dir, Enforcer: k.Enforcer, Log: k.Log}
	k.Exec.Moderator = k.Mod
	k.Worker = &pipeline.Worker{Store: st, Inbox: pipeline.NewInbox(st), Decider: k.Mod, Config: k.Holder, Log: k.Log}
	k.Media = &action.MediaFetcher{Store: st, Adapter: k.Fake, Config: k.Holder, Dir: filepath.Join(filepath.Dir(k.DBPath), "evidence"),
		Log: k.Log}
}

// Reopen closes the store as a crash would and opens it again with fresh
// workers (the directory and the fake survive, like WhatsApp itself).
func (k *Kit) Reopen() {
	k.T.Helper()
	_ = k.Store.Close()
	k.open()
}

// Reload rewrites the config file with the kit's config plus extra and
// reloads it.
func (k *Kit) Reload(extra string) {
	k.T.Helper()
	k.ReloadConfig(k.base + extra)
}

// ReloadConfig rewrites the config file with cfg and reloads it.
func (k *Kit) ReloadConfig(cfg string) {
	k.T.Helper()
	if err := os.WriteFile(k.CfgPath, []byte(configtest.Base+cfg), 0o600); err != nil {
		k.T.Fatal(err)
	}
	if _, err := k.Holder.Reload(); err != nil {
		k.T.Fatal(err)
	}
}

// Deliver persists ev as the WhatsApp handler would and decides it.
func (k *Kit) Deliver(ev client.Event) {
	k.T.Helper()
	if err := k.Worker.Inbox.Persist(ev); err != nil {
		k.T.Fatal(err)
	}
	if err := k.Worker.Drain(k.Ctx); err != nil {
		k.T.Fatal(err)
	}
}

// Fire runs the executor until nothing more is due and returns how many
// actions it settled.
func (k *Kit) Fire() int {
	k.T.Helper()
	n := 0
	for i := 0; i < 200; i++ {
		fired, err := k.Exec.Step(k.Ctx)
		if err != nil {
			k.T.Fatal(err)
		}
		if !fired {
			return n
		}
		n++
	}
	k.T.Fatal("the executor never settled")
	return n
}

// Msg builds a message from sender in chat at the kit's current time.
func (k *Kit) Msg(id string, chat, sender client.JID, text string) *client.Message {
	return &client.Message{Chat: chat, Sender: sender, ID: id, TargetID: id, Time: k.Clock.Now(),
		Fields: []client.Field{{Name: "body", Text: text, Match: text}}}
}

// Spam is a "pitch" match from the spammer in chat.
func (k *Kit) Spam(id string, chat client.JID) *client.Message {
	m := k.Msg(id, chat, Spammer, SpamText)
	m.SenderAlt = SpammerPhone
	return m
}

// Ban adds member to the ban list (every community), as an admin would. Each
// call is its own ban order, so a member unbanned in between is banned again
// (a member still banned keeps their one ban row).
func (k *Kit) Ban(member client.Member) {
	k.T.Helper()
	k.banOrders++
	k.write(ledger.Plan{Trigger: "test:ban:" + member.Key() + ":" + strconv.Itoa(k.banOrders), Target: member,
		ConfigHash: k.Holder.Current().Hash, Ban: []string{store.BanEverywhere}, BanEnforce: true,
		BanCommunity: string(Community), Reason: "test ban"})
}

// Unban lifts every ban of member.
func (k *Kit) Unban(member client.Member) {
	k.T.Helper()
	k.write(ledger.Plan{Trigger: "test:unban:" + member.Key(), Target: member, ConfigHash: k.Holder.Current().Hash,
		Lift: true, BanCommunity: string(Community), Reason: "test unban"})
}

func (k *Kit) write(p ledger.Plan) {
	k.T.Helper()
	if err := k.Store.Write(k.Ctx, func(tx *sql.Tx) error {
		_, err := ledger.Write(k.Ctx, tx, p, k.Clock.Now())
		return err
	}); err != nil {
		k.T.Fatal(err)
	}
}

// MarkAllSent records every stored report as delivered (what the admin chat
// does once it has posted them).
func (k *Kit) MarkAllSent() {
	k.T.Helper()
	reps, err := k.Store.UnsentReports(k.Ctx, 10000)
	if err != nil {
		k.T.Fatal(err)
	}
	for _, r := range reps {
		if err := k.Store.MarkReportSent(k.Ctx, r.ID); err != nil {
			k.T.Fatal(err)
		}
	}
}

// Rows lists every ledger row about member.
func (k *Kit) Rows(member client.Member) []store.LedgerRow {
	k.T.Helper()
	rows, err := k.Store.LedgerForTargets(k.Ctx, member.IDs())
	if err != nil {
		k.T.Fatal(err)
	}
	return rows
}

// Find returns the rows about member with action a in chat.
func (k *Kit) Find(member client.Member, a store.Action, chat client.JID) []store.LedgerRow {
	var out []store.LedgerRow
	for _, r := range k.Rows(member) {
		if r.Action == a && r.Chat == string(chat) {
			out = append(out, r)
		}
	}
	return out
}

// Banned reports whether member is on the ban list for community ("" = any).
func (k *Kit) Banned(member client.Member, community string) bool {
	k.T.Helper()
	_, ok, err := k.Store.FindBan(k.Ctx, member.IDs(), community)
	if err != nil {
		k.T.Fatal(err)
	}
	return ok
}

// Reports lists the stored reports of kind, oldest first (read through a
// handle of the test's own: the store has no by-kind query).
func (k *Kit) Reports(kind string) []store.Report {
	k.T.Helper()
	db, err := sql.Open("sqlite", "file:"+k.DBPath+"?_pragma=busy_timeout(10000)")
	if err != nil {
		k.T.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(k.Ctx, `SELECT id, kind, priority, community, subject, text, buttons FROM reports
WHERE kind = ? ORDER BY id`, kind)
	if err != nil {
		k.T.Fatal(err)
	}
	defer rows.Close()
	var out []store.Report
	for rows.Next() {
		var r store.Report
		var buttons string
		if err := rows.Scan(&r.ID, &r.Kind, &r.Priority, &r.Community, &r.Subject, &r.Text, &buttons); err != nil {
			k.T.Fatal(err)
		}
		if buttons != "" {
			r.Buttons = strings.Split(buttons, ",")
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		k.T.Fatal(err)
	}
	return out
}

// The kit's people.
var (
	SpammerM = client.Member{LID: Spammer, Phone: SpammerPhone}
	AdminM   = client.Member{LID: Admin, Phone: AdminPhone}
	MemberM  = client.Member{LID: Member}
	Other1M  = client.Member{LID: Other1}
	Other2M  = client.Member{LID: Other2}
)

package app

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/reconcile"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
)

// staleAfter: a queued action older than this is listed as stale in /status.
const staleAfter = time.Hour

// Version is the running binary's version: its module version and the
// commit it was built from.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(version unknown)"
	}
	v := info.Main.Version
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			v += " (" + s.Value[:12] + ")"
		}
	}
	return v
}

// Controls is what the admin chat's buttons and commands do in this app.
func (a *App) Controls() telegram.Controls { return controls{a} }

type controls struct{ a *App }

func (c controls) Undo(ctx context.Context, reportID int64, by telegram.Actor) (string, error) {
	res, err := c.a.Admin.Undo(ctx, reportID, by.String())
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf("Undone by %s (#%d): %s is unbanned everywhere (%d ban(s) lifted, %d action(s) overturned). ",
		by.Name, reportID, mask.IDs(res.Member.Key()), res.BansLifted, res.Overturned)
	if len(res.RemovedFrom) == 0 {
		text += "The bot had not removed them from any group."
	} else {
		names := make([]string, len(res.RemovedFrom))
		for i, g := range res.RemovedFrom {
			names[i] = c.a.groupLabel(g)
		}
		text += "Re-invite them by hand to: " + strings.Join(names, ", ") + "."
	}
	return text + " A deleted message cannot be restored; the report keeps its text until the evidence window ends.", nil
}

func (c controls) Ban(ctx context.Context, reportID int64, by telegram.Actor) (string, error) {
	res, err := c.a.Admin.Ban(ctx, reportID, by.String())
	if err != nil {
		return "", err
	}
	return banReply(res, reportID, by, true), nil
}

func (c controls) AddToBanList(ctx context.Context, reportID int64, by telegram.Actor) (string, error) {
	res, err := c.a.Admin.AddToBanList(ctx, reportID, by.String())
	if err != nil {
		return "", err
	}
	return banReply(res, reportID, by, false), nil
}

func banReply(res pipeline.Banned, reportID int64, by telegram.Actor, message bool) string {
	if res.Spared {
		return "Not banned: they are a current admin of a moderated group. An admin must decide."
	}
	var parts []string
	if message {
		switch {
		case res.TooOld:
			parts = append(parts, "the message is older than act_on_replay_max_age, so it stays")
		case res.Deleting:
			parts = append(parts, "the message is being deleted")
		}
	}
	parts = append(parts, fmt.Sprintf("%s is removed from %d group(s) and banned", mask.IDs(res.Member.Key()), res.Removals))
	text := fmt.Sprintf("Banned by %s (#%d): %s.", by.Name, reportID, strings.Join(parts, "; "))
	if res.Shadow {
		text += " The community is in shadow mode: this is recorded only, nothing is sent."
	}
	return text + fmt.Sprintf(" To undo: /unban %d", reportID)
}

func (c controls) Resume(ctx context.Context, by telegram.Actor) (string, error) {
	if c.a.Executor == nil {
		return "", errors.New("the action executor is not running")
	}
	pauses, err := c.a.Store.Pauses(ctx)
	if err != nil {
		return "", err
	}
	if err := c.a.Executor.Resume(ctx); err != nil {
		return "", err
	}
	c.a.Log.Info("resumed from the admin chat", "by", by.String(), "pauses", len(pauses))
	if len(pauses) == 0 {
		return "Nothing was paused.", nil
	}
	return fmt.Sprintf("Resumed by %s: %d pause(s) lifted. Queued actions are checked again before they are sent.",
		by.Name, len(pauses)), nil
}

func (c controls) Pause(ctx context.Context, by telegram.Actor) (string, error) {
	now := c.a.Clock.Now()
	if err := c.a.Store.SetPause(ctx, store.Pause{Source: store.SourceAdmin, Scope: store.ScopeAll,
		Reason: "paused by " + by.Name + " with /pause", Since: now}); err != nil {
		return "", err
	}
	c.a.Log.Warn("paused from the admin chat", "by", by.String())
	return "Paused by " + by.Name + ": every action stops, deletes included. Reports continue. /resume to continue.", nil
}

func (c controls) Reload(ctx context.Context, by telegram.Actor) (string, error) {
	c.a.Log.Info("reload requested from the admin chat", "by", by.String())
	return c.a.reload(ctx, false), nil
}

func (c controls) Join(ctx context.Context, link string, by telegram.Actor) (string, error) {
	code, err := client.InviteCode(link)
	if err != nil {
		return "", err
	}
	g, parent, err := c.a.Adapter.InviteInfo(ctx, code)
	if err != nil {
		return "", fmt.Errorf("look up the invite link: %w", err)
	}
	rs := c.a.Config.Current().Rules
	if _, configured := rs.ModeFor(string(parent)); parent == "" || !configured {
		c.a.Log.Warn("join refused: not a group of a configured community", "group", mask.IDs(string(g.JID)),
			"parent", mask.IDs(string(parent)), "by", by.String())
		return fmt.Sprintf("Not joined: %q is not in a configured community. The bot joins only groups of the "+
			"communities in its config.", g.Name), nil
	}
	jid, pending, err := c.a.Adapter.JoinWithLink(ctx, code)
	if err != nil {
		return "", fmt.Errorf("join: %w", err)
	}
	c.a.Log.Info("joined from the admin chat", "group", mask.IDs(string(jid)), "pending", pending, "by", by.String())
	if pending {
		return fmt.Sprintf("Join request sent to %q: a group admin must approve it, then promote the bot to admin.", g.Name), nil
	}
	return fmt.Sprintf("Joined %q. A human admin must now promote the bot to admin there; it moderates the group "+
		"once it is an admin.", g.Name), nil
}

func (c controls) Status(ctx context.Context) (string, error) { return c.a.statusText(ctx) }

// groupLabel names a group for the admins: its name and masked ID.
func (a *App) groupLabel(g client.JID) string {
	if name := a.Directory.GroupName(string(g)); name != "" {
		return name + " (" + mask.IDs(string(g)) + ")"
	}
	return mask.IDs(string(g))
}

// communityLabel names a configured community: its configured name, else
// its masked ID.
func (a *App) communityLabel(id string) string {
	if cm, ok := a.Config.Current().Config.Communities[id]; ok && cm.Name != "" {
		return cm.Name
	}
	return mask.IDs(id)
}

// statusText is /status: the connection, coverage, modes, pauses, config,
// timers, bans and queued actions.
func (a *App) statusText(ctx context.Context) (string, error) {
	now := a.Clock.Now()
	st, err := a.Store.Status(ctx)
	if err != nil {
		return "", err
	}
	cur := a.Config.Current()
	var b strings.Builder
	fmt.Fprintf(&b, "groupwarden %s\n", Version())
	if a.mon == nil {
		b.WriteString("WhatsApp: not running\n")
	} else {
		connected, deaf, last := a.mon.snapshot()
		fmt.Fprintf(&b, "WhatsApp: %s; last event %s", yesNo(connected, "connected", "NOT connected"), ago(now, last))
		if deaf {
			b.WriteString(" (DEAF: no events for too long)")
		}
		b.WriteString("\n")
	}
	pauses, err := a.Store.Pauses(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(&b, "Paused: yes (pause state unreadable: %v)\n", err)
	case len(pauses) == 0:
		b.WriteString("Paused: no\n")
	default:
		for _, p := range pauses {
			what := "removals and bans (deletes continue)"
			if p.Scope == store.ScopeAll {
				what = "every action"
			}
			fmt.Fprintf(&b, "Paused: %s since %s (%s): %s\n", what, p.Since.UTC().Format("2006-01-02 15:04 MST"),
				p.Source, p.Reason)
		}
	}
	fmt.Fprintf(&b, "Config: v%s; last sync %s", cur.Hash, ago(now, msStatus(st, store.StatusSyncLastRun)))
	if r := st[store.StatusSyncResult].Value; r != "" {
		fmt.Fprintf(&b, " (%s)", r)
	}
	fmt.Fprintf(&b, "\nBackup: last run %s\n", ago(now, msStatus(st, store.StatusBackupLastRun)))
	bans, err := a.Store.Bans(ctx)
	if err != nil {
		return "", err
	}
	queued, err := a.Store.OutboxLen(ctx)
	if err != nil {
		return "", err
	}
	stale, err := a.Store.CountStaleIntended(ctx, now.Add(-staleAfter))
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "Bans: %d; queued actions: %d; stale ledger rows (queued over %s): %d\n", len(bans), queued,
		staleAfter, stale)
	var cov reconcile.Coverage
	if a.Sweep != nil {
		cov = a.Sweep.Coverage()
	}
	if cov.At.IsZero() {
		b.WriteString("Coverage: not checked yet (the first sweep runs once connected)\n")
	} else {
		fmt.Fprintf(&b, "Coverage (checked %s):\n", ago(now, cov.At))
	}
	byCommunity := map[string]reconcile.CommunityCoverage{}
	for _, cc := range cov.Communities {
		byCommunity[cc.Community] = cc
	}
	for _, c := range cur.Rules.Communities() {
		mode, _ := cur.Rules.ModeFor(c)
		cc := byCommunity[c]
		n := map[string]int{}
		for _, g := range cc.Groups {
			n[g.State]++
		}
		fmt.Fprintf(&b, "%s (%s): %d covered, %d absent, %d not admin\n", a.communityLabel(c), mode,
			n[reconcile.StateCovered], n[reconcile.StateAbsent], n[reconcile.StateNotAdmin])
		if cc.Err != "" {
			fmt.Fprintf(&b, "  could not list its groups: %s\n", cc.Err)
		}
		for _, g := range cc.Groups {
			label := mask.IDs(string(g.JID))
			if g.Name != "" {
				label = g.Name + " (" + label + ")"
			}
			fmt.Fprintf(&b, "  %s: %s", label, strings.ReplaceAll(g.State, "_", " "))
			if g.State != reconcile.StateAbsent {
				fmt.Fprintf(&b, ", %d human admin(s)", g.HumanAdmins)
			}
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String()), nil
}

func yesNo(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}

// ago says how long before now t was ("never" for the zero time).
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return now.Sub(t).Round(time.Second).String() + " ago"
}

// msStatus reads a unix-ms status value (zero when absent or unreadable).
func msStatus(st map[string]store.StatusValue, key string) time.Time {
	ms, err := strconv.ParseInt(st[key].Value, 10, 64)
	if err != nil || ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

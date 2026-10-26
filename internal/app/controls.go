package app

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/reconcile"
	"github.com/hoiung/groupwarden/internal/rules"
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
			names[i] = c.a.Directory.Label(g)
		}
		text += "Re-invite them by hand to: " + strings.Join(names, ", ") + "."
	}
	return text + " A deleted message cannot be restored; the report keeps its text until the evidence window ends.", nil
}

// Ban is [Ban] and /ban as a reply to its report. A ban applied asks for a
// sweep at once, so the member is removed wherever the bot finds them
// without waiting for reconcile.interval_minutes; AddToBanList and
// BanNumber do the same.
func (c controls) Ban(ctx context.Context, reportID int64, by telegram.Actor) (string, error) {
	res, err := c.a.Admin.Ban(ctx, reportID, by.String())
	if err != nil {
		return "", err
	}
	c.sweepAfter(res)
	return banReply(res, reportID, by, true), nil
}

func (c controls) AddToBanList(ctx context.Context, reportID int64, by telegram.Actor) (string, error) {
	res, err := c.a.Admin.AddToBanList(ctx, reportID, by.String())
	if err != nil {
		return "", err
	}
	c.sweepAfter(res)
	return banReply(res, reportID, by, false), nil
}

// sweepAfter asks for a sweep once a report's ban is applied (a spared admin
// and a ban recorded in shadow mode put nobody on the ban list).
func (c controls) sweepAfter(res pipeline.Banned) {
	if !res.Spared && !res.Shadow {
		c.a.Log.Info("sweep requested after a ban from the admin chat", "target", mask.IDs(res.Member.Key()))
		c.a.requestSweep()
	}
}

// BanNumber is /ban <number>.
func (c controls) BanNumber(ctx context.Context, phone client.JID, by telegram.Actor) (string, error) {
	res, err := c.a.Admin.BanNumber(ctx, phone, by.String())
	if err != nil {
		return "", err
	}
	who := mask.IDs(string(phone))
	switch {
	case res.Self:
		return who + " is the bot's own number: it is never banned.", nil
	case res.PerCommunity:
		return "Not banned: bans.scope is per_community, so a ban needs its community. On the node, run " +
			"`groupwarden ban add <number> --community <id>`, or send /ban as a reply to a report from that community.", nil
	case res.Spared:
		return "Not banned: " + who + " is a current admin of a community or group the bot moderates. An admin " +
			"must decide.", nil
	case res.Already:
		return who + " is already banned; nothing changed.", nil
	}
	c.a.Log.Info("sweep requested after a ban from the admin chat", "target", mask.IDs(res.Member.Key()))
	c.a.requestSweep()
	text := fmt.Sprintf("Banned by %s: %s is banned everywhere. %s. ", by.Name, who,
		upperFirst(postsNote(res.Posts, res.Paused, false)))
	if res.Paused != "" {
		text += "The bot sweeps every group now; removing them waits for the pause to end"
	} else {
		text += "The bot sweeps every group now and removes them wherever it finds them"
	}
	if anyShadow(c.a.Config.Current().Rules) {
		text += ", except in a community in shadow mode, where the sweep only reports them"
	}
	return text + ". To undo: /unban with the same number.", nil
}

// UnbanNumber is /unban <number>.
func (c controls) UnbanNumber(ctx context.Context, phone client.JID, by telegram.Actor) (string, error) {
	res, err := c.a.Admin.UnbanNumber(ctx, phone, by.String())
	if err != nil {
		return "", err
	}
	who := mask.IDs(string(phone))
	if res.NotBanned {
		return who + " is not banned; nothing changed.", nil
	}
	return fmt.Sprintf("Unbanned by %s: %s is unbanned everywhere (%d ban(s) lifted). They are not added back to any "+
		"group: re-invite them by hand where they should be.", by.Name, who, res.Lifted), nil
}

// postsNote says what a ban did about the member's posts the bot still has a
// record of: how many are being deleted and how many could not be. other:
// the reported post is counted apart.
func postsNote(p pipeline.Posts, paused store.Scope, other bool) string {
	which := "their posts on record"
	if other {
		which = "their other posts on record"
	}
	deleting := "being deleted"
	if paused == store.ScopeAll {
		deleting = "deleted once the pause ends"
	}
	s := fmt.Sprintf("%s: %d %s, %d too old to delete", which, p.Deleting, deleting, p.Refused)
	if p.Refused > 0 {
		s += " (older than act_on_replay_max_age)"
	}
	if p.Shadow > 0 {
		s += fmt.Sprintf(", %d left in a community in shadow mode", p.Shadow)
	}
	if p.Spared > 0 {
		s += fmt.Sprintf(", %d left where they are a current admin", p.Spared)
	}
	return s
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// anyShadow reports whether a configured community is in shadow mode, where
// the sweep reports a banned member instead of removing them.
func anyShadow(rs *rules.Ruleset) bool {
	return slices.ContainsFunc(rs.Communities(), func(c string) bool {
		mode, _ := rs.ModeFor(c)
		return mode != rules.Enforce
	})
}

func banReply(res pipeline.Banned, reportID int64, by telegram.Actor, message bool) string {
	if res.Spared {
		return "Not banned: they are a current admin of the community or group this report is from. An admin must decide."
	}
	var parts []string
	if message {
		switch {
		case res.TooOld:
			parts = append(parts, "the message is older than act_on_replay_max_age, so it stays")
		case res.Deleting && res.Paused == store.ScopeAll:
			parts = append(parts, "the message is deleted once the pause ends")
		case res.Deleting:
			parts = append(parts, "the message is being deleted")
		}
	}
	who := mask.IDs(res.Member.Key())
	if res.Shadow {
		// No ban is applied and nothing is sent, paused or not: the reply
		// neither says "banned" nor offers an /unban of a ban that is not there.
		would := fmt.Sprintf("%s would be removed from %d group(s) and banned", who, res.Removals)
		if res.Posts.Shadow > 0 {
			would += fmt.Sprintf(", and %d more post(s) of theirs deleted", res.Posts.Shadow)
		}
		parts = append(parts, would+", but the community is in shadow mode: nothing is banned or sent")
		return fmt.Sprintf("Recorded by %s (#%d): %s.", by.Name, reportID, strings.Join(parts, "; "))
	}
	parts = append(parts, postsNote(res.Posts, res.Paused, message))
	switch {
	case res.Paused != "":
		parts = append(parts, fmt.Sprintf("%s is banned; their removal from %d group(s) waits for the pause to end",
			who, res.Removals))
	default:
		parts = append(parts, fmt.Sprintf("%s is removed from %d group(s) and banned", who, res.Removals))
	}
	return fmt.Sprintf("Banned by %s (#%d): %s. To undo: /unban #%d", by.Name, reportID, strings.Join(parts, "; "), reportID)
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

func (c controls) Status(ctx context.Context) (string, error) { return c.a.statusText(ctx) }

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
			fmt.Fprintf(&b, "Paused: %s since %s (%s): %s\n", p.Scope.Stops(), p.Since.UTC().Format("2006-01-02 15:04 MST"),
				p.Source, p.Reason)
		}
	}
	fmt.Fprintf(&b, "Config: v%s; last sync %s", cur.Hash, ago(now, st[store.StatusSyncLastRun].Time()))
	if r := st[store.StatusSyncResult].Value; r != "" {
		fmt.Fprintf(&b, " (%s)", r)
	}
	fmt.Fprintf(&b, "\nBackup: last run %s", ago(now, st[store.StatusBackupLastRun].Time()))
	if r := st[store.StatusBackupResult].Value; r != "" {
		fmt.Fprintf(&b, " (%s)", r)
	}
	b.WriteString("\n")
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
	held, err := a.Store.HeldBans(ctx)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "Bans: %d", len(bans))
	if len(held) > 0 {
		fmt.Fprintf(&b, " (+%d held by a pause: they apply once the pause ends)", len(held))
	}
	fmt.Fprintf(&b, "; queued actions: %d; stale ledger rows (queued over %s): %d\n", queued, staleAfter, stale)
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
		how := string(mode)
		if !cur.FromFile(c) {
			how += ", switched on with /enable"
		}
		fmt.Fprintf(&b, "%s (%s): %d covered, %d absent, %d not admin\n", pipeline.CommunityLabel(cur.Config, c), how,
			n[store.CoverageCovered], n[store.CoverageAbsent], n[store.CoverageNotAdmin])
		if cc.Err != "" {
			fmt.Fprintf(&b, "  could not list its groups: %s\n", cc.Err)
		}
		for _, g := range cc.Groups {
			fmt.Fprintf(&b, "  %s: %s", pipeline.Label(g.Name, g.JID), strings.ReplaceAll(g.State, "_", " "))
			if g.State != store.CoverageAbsent {
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

package app

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/rules"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
)

// EnabledEntries is the store's /enable list as the config merges it.
func EnabledEntries(list []store.Enabled) []config.Enabled {
	out := make([]config.Enabled, 0, len(list))
	for _, e := range list {
		out = append(out, config.Enabled{ID: e.ID, Group: e.Kind == store.KindGroup, Name: e.Name})
	}
	return out
}

// LoadEnabled merges the store's /enable list into the running config
// (`run` does it before the first event is decided).
func LoadEnabled(ctx context.Context, st *store.Store, h *config.Holder) (*config.Loaded, error) {
	list, err := st.EnabledList(ctx)
	if err != nil {
		return nil, err
	}
	return h.SetEnabled(EnabledEntries(list))
}

// applyEnabled loads the /enable list into the running config again. The
// caller asks for the sweep once its own WhatsApp calls are done: a sweep
// that lists a community's groups before /enable joined the community would
// try to join them by itself too early, and never tries again.
func (a *App) applyEnabled(ctx context.Context) error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	l, err := LoadEnabled(ctx, a.Store, a.Config)
	if err != nil {
		a.Log.Error("enabled list not loaded", "err", err)
		return err
	}
	a.Log.Info("enabled list loaded", "config", "v"+l.Hash, "communities", len(l.Rules.Communities()))
	a.writeStatus(ctx)
	return nil
}

// Enable is /enable: a community link switches the whole community on, a
// group link that group alone, and the entry is kept even when the config
// file lists it too (it stays on once it leaves the file). The bot joins
// through the link when it is not in it; a group already on as part of its
// community is only joined. A sweep follows, so coverage (and joining a
// community's groups) comes at once.
func (c controls) Enable(ctx context.Context, link string, by telegram.Actor) (string, error) {
	code, g, parent, err := c.lookUp(ctx, link)
	if err != nil {
		return "", err
	}
	what := linkTarget(g)
	cur := c.a.Config.Current()
	var parts []string
	in := pipeline.GroupCommunity(g.JID, parent, g.IsCommunity, cur.Rules)
	switch {
	case in != "" && in != string(g.JID):
		parts = append(parts, fmt.Sprintf("The %s is already on (%s)", what, onBecause(cur, in, g.JID)))
	default:
		if err := c.record(ctx, g, by); err != nil {
			return "", err
		}
		switch {
		case in == "":
			parts = append(parts, fmt.Sprintf("Enabled by %s: the bot moderates the %s from now on", by.Name, what))
		case cur.FromFile(in):
			s := fmt.Sprintf("The %s is already on (set in the config file); /enable now keeps it on too, so it "+
				"stays on once it leaves the file", what)
			// Out of the file it runs on the global settings: say so where
			// the file gives it its own (a shadow community would enforce).
			if own := cur.Config.Communities[in].OwnSettings(); len(own) > 0 {
				s += " (then with the global settings, not the file's own " + andList(own) + " for it)"
			}
			parts = append(parts, s)
		default:
			parts = append(parts, fmt.Sprintf("The %s is already on (switched on with /enable)", what))
		}
	}
	switch {
	case c.a.Directory.Known(g.JID) && c.a.Directory.BotIsAdmin(g.JID):
		parts = append(parts, "the bot is in it and an admin there")
	case c.a.Directory.Known(g.JID):
		parts = append(parts, "the bot is in it but not an admin: a human admin must promote it there")
	default:
		jid, pending, err := c.a.Adapter.JoinWithLink(ctx, code)
		switch {
		case err != nil:
			c.a.Log.Warn("join from the admin chat failed", "group", mask.IDs(string(g.JID)), "by", by.String(),
				"err", mask.IDs(err.Error()))
			parts = append(parts, "joining it failed ("+mask.IDs(err.Error())+"): send /enable again to retry")
		case pending:
			c.a.Log.Info("join requested from the admin chat", "group", mask.IDs(string(jid)), "by", by.String())
			parts = append(parts, "join request sent: an admin there must approve it, then promote the bot to admin")
		default:
			c.a.Log.Info("joined from the admin chat", "group", mask.IDs(string(jid)), "by", by.String())
			parts = append(parts, "the bot joined it: a human admin must now promote the bot to admin there")
		}
	}
	c.a.requestSweep()
	if g.IsCommunity {
		parts = append(parts, "/status lists its groups after the sweep")
	}
	return strings.Join(parts, "; ") + ".", nil
}

// Disable is /disable: it switches off what /enable switched on for the
// link's community or group, or for the entry /status shows under that name
// (the way to switch one off once its link no longer works: reset, revoked,
// or the group deleted). The bot stays in the group; what the config file
// lists stays on.
func (c controls) Disable(ctx context.Context, linkOrName string, by telegram.Actor) (string, error) {
	g, onNow, err := c.disableTarget(ctx, linkOrName)
	if err != nil {
		return "", err
	}
	what := linkTarget(g)
	removed, err := c.a.Store.Disable(ctx, string(g.JID))
	if err != nil {
		return "", fmt.Errorf("remove it: %w", err)
	}
	if removed {
		c.a.Log.Info("disabled from the admin chat", "id", mask.IDs(string(g.JID)), "by", by.String())
	}
	// Rebuilt even when nothing was removed: a running config an earlier
	// failed rebuild left with the entry is put right here.
	if err := c.a.applyEnabled(ctx); err != nil {
		if removed {
			return "", fmt.Errorf("removed from the /enable list, but the running config still has it: %w", err)
		}
		return "", err
	}
	if removed {
		c.a.requestSweep()
	}
	cur := c.a.Config.Current()
	in := onNow(cur.Rules)
	switch {
	case in == "" && removed:
		return fmt.Sprintf("Disabled by %s: the bot no longer moderates the %s. It stays in it until an admin removes it.%s",
			by.Name, what, c.groupsStillOn(ctx, g)), nil
	case in == "":
		return fmt.Sprintf("The %s is not on.", what), nil
	case cur.FromFile(in) && in != string(g.JID) && len(cur.Config.Communities[in].Groups) == 0:
		// The file lists its community, not the group.
		return fmt.Sprintf("The %s is still on (%s): remove that community from the config file to switch it off.", what,
			onBecause(cur, in, g.JID)), nil
	case cur.FromFile(in):
		return fmt.Sprintf("The %s is still on (%s): remove it from the config file to switch it off.", what,
			onBecause(cur, in, g.JID)), nil
	}
	return fmt.Sprintf("The %s is still on (%s): send /disable with that community's invite link to switch it off.",
		what, onBecause(cur, in, g.JID)), nil
}

// disableTarget finds what /disable names: the /enable entry /status shows
// under that name, else the community or group an invite link points to.
// onNow says which configured community it counts towards under a ruleset
// ("" = not moderated).
func (c controls) disableTarget(ctx context.Context, linkOrName string) (client.Group, func(*rules.Ruleset) string, error) {
	list, err := c.a.Store.EnabledList(ctx)
	if err != nil {
		return client.Group{}, nil, err
	}
	cur := c.a.Config.Current()
	name := strings.TrimSpace(linkOrName)
	var named []store.Enabled
	for _, e := range list {
		if e.Name != "" && strings.EqualFold(e.Name, name) {
			named = append(named, e)
		}
	}
	switch {
	case len(named) == 1:
		e := named[0]
		g := client.Group{JID: client.JID(e.ID), Name: e.Name, IsCommunity: e.Kind == store.KindCommunity}
		return g, func(rs *rules.Ruleset) string {
			if g.IsCommunity {
				return pipeline.GroupCommunity(g.JID, "", true, rs)
			}
			// The link is not looked up, so the group's community is what the
			// bot last learned about it.
			return c.a.Directory.Community(g.JID, rs)
		}, nil
	case len(named) > 1:
		return client.Group{}, nil, fmt.Errorf("%d entries switched on with /enable are named \"%s\": send /disable with "+
			"the invite link of the one to switch off (an entry whose group was deleted moderates nothing)", len(named), name)
	}
	// What the config file lists, by the name /status shows (the file's, also
	// when /enable keeps it under WhatsApp's): Disable drops any /enable entry
	// for it and says to remove it from the file.
	for _, id := range cur.Rules.Communities() {
		if label := pipeline.CommunityLabel(cur.Config, id); cur.FromFile(id) && strings.EqualFold(label, name) {
			g := client.Group{JID: client.JID(id), Name: label, IsCommunity: len(cur.Config.Communities[id].Groups) == 0}
			return g, func(rs *rules.Ruleset) string {
				if _, ok := rs.ModeFor(id); ok {
					return id
				}
				return ""
			}, nil
		}
	}
	if _, err := client.InviteCode(linkOrName); err != nil {
		return client.Group{}, nil, fmt.Errorf("%w, nor the name of something switched on with /enable (/status lists them)", err)
	}
	_, g, parent, err := c.lookUp(ctx, linkOrName)
	if err != nil {
		return client.Group{}, nil, fmt.Errorf("%w: when its link no longer works, send /disable with the name /status "+
			"shows for it", err)
	}
	return g, func(rs *rules.Ruleset) string { return pipeline.GroupCommunity(g.JID, parent, g.IsCommunity, rs) }, nil
}

// andList joins words as "a", "a and b", "a, b and c".
func andList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

// record keeps g in the /enable list and loads the list into the running
// config; an entry already kept is left as it is, but loaded again (an
// earlier failed rebuild may have left the running config without it). A
// new entry the config does not take is taken back out.
func (c controls) record(ctx context.Context, g client.Group, by telegram.Actor) error {
	list, err := c.a.Store.EnabledList(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(list, func(e store.Enabled) bool { return e.ID == string(g.JID) }) {
		return c.a.applyEnabled(ctx)
	}
	e := store.Enabled{ID: string(g.JID), Kind: store.KindGroup, Name: g.Name, By: by.String(), At: c.a.Clock.Now()}
	if g.IsCommunity {
		e.Kind = store.KindCommunity
	}
	if err := c.a.Store.Enable(ctx, e); err != nil {
		return fmt.Errorf("record it: %w", err)
	}
	if err := c.a.applyEnabled(ctx); err != nil {
		if _, derr := c.a.Store.Disable(ctx, e.ID); derr != nil {
			c.a.Log.Error("take back a refused /enable entry", "id", mask.IDs(e.ID), "err", derr)
		}
		return err
	}
	c.a.Log.Info("enabled from the admin chat", "id", mask.IDs(e.ID), "kind", e.Kind, "by", by.String())
	return nil
}

// lookUp reads an invite link without joining.
func (c controls) lookUp(ctx context.Context, link string) (string, client.Group, client.JID, error) {
	code, err := client.InviteCode(link)
	if err != nil {
		return "", client.Group{}, "", err
	}
	g, parent, err := c.a.Adapter.InviteInfo(ctx, code)
	if err != nil {
		return "", client.Group{}, "", fmt.Errorf("look up the invite link: %w", err)
	}
	return code, g, parent, nil
}

// linkTarget names what an invite link points to: `community "X"` or
// `group "X"`, the name as WhatsApp has it (not Go-escaped: an emoji joined
// with U+200D, a quote or a right-to-left mark shows as written).
func linkTarget(g client.Group) string {
	if g.IsCommunity {
		return "community \"" + g.Name + "\""
	}
	return "group \"" + g.Name + "\""
}

// groupsStillOn is the sentence a community's /disable adds for each of its
// groups switched on alone with /enable, which stay on ("" when none; a group
// whose community the bot has not learned is not named).
func (c controls) groupsStillOn(ctx context.Context, g client.Group) string {
	if !g.IsCommunity {
		return ""
	}
	list, err := c.a.Store.EnabledList(ctx)
	if err != nil {
		c.a.Log.Warn("read the /enable list for the groups still on", "err", err)
		return ""
	}
	cur := c.a.Config.Current()
	var names []string
	for _, e := range list {
		if e.Kind == store.KindGroup && c.a.Directory.Parent(client.JID(e.ID)) == g.JID {
			names = append(names, "\""+pipeline.CommunityLabel(cur.Config, e.ID)+"\"")
		}
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return " Its group " + names[0] + " stays on (switched on alone with /enable): send /disable with its link or name " +
			"to switch it off too."
	}
	return " Its groups " + andList(names) + " stay on (each switched on alone with /enable): send /disable with their " +
		"links or names to switch them off too."
}

// onBecause says why target is on: as part of a community, by the config
// file, or by /enable.
func onBecause(cur *config.Loaded, in string, target client.JID) string {
	switch {
	case in != string(target):
		return "as part of " + pipeline.CommunityLabel(cur.Config, in)
	case cur.FromFile(in):
		return "set in the config file"
	}
	return "switched on with /enable"
}

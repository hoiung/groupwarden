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

// applyEnabled loads the /enable list into the running config again, then
// asks for a sweep so coverage (and joining a community's groups) follows at
// once.
func (a *App) applyEnabled(ctx context.Context) error {
	a.reloadMu.Lock()
	defer a.reloadMu.Unlock()
	l, err := LoadEnabled(ctx, a.Store, a.Config)
	if err != nil {
		a.Log.Error("enabled list not loaded", "err", err)
		return err
	}
	a.Log.Info("enabled list loaded", "config", "v"+l.Hash)
	a.writeStatus(ctx)
	a.requestSweep()
	return nil
}

// Enable is /enable: a community link switches the whole community on, a
// group link that group alone, and the entry is kept even when the config
// file lists it too (it stays on once it leaves the file). The bot joins
// through the link when it is not in it; a group already on as part of its
// community is only joined.
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
			parts = append(parts, fmt.Sprintf("The %s is already on (set in the config file); /enable now keeps it "+
				"on too, so it stays on once it leaves the file", what))
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
	if g.IsCommunity {
		parts = append(parts, "/status lists its groups after the sweep")
	}
	return strings.Join(parts, "; ") + ".", nil
}

// Disable is /disable: it switches off what /enable switched on for the
// link's community or group. The bot stays in the group; what the config
// file lists stays on.
func (c controls) Disable(ctx context.Context, link string, by telegram.Actor) (string, error) {
	_, g, parent, err := c.lookUp(ctx, link)
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
		if err := c.a.applyEnabled(ctx); err != nil {
			return "", fmt.Errorf("removed from the /enable list, but the running config still has it (a restart drops it): %w", err)
		}
	}
	cur := c.a.Config.Current()
	in := pipeline.GroupCommunity(g.JID, parent, g.IsCommunity, cur.Rules)
	switch {
	case in == "" && removed:
		return fmt.Sprintf("Disabled by %s: the bot no longer moderates the %s. It stays in it until an admin removes it.",
			by.Name, what), nil
	case in == "":
		return fmt.Sprintf("The %s is not on.", what), nil
	case cur.FromFile(in):
		return fmt.Sprintf("The %s is still on (%s): remove it from the config file to switch it off.", what,
			onBecause(cur, in, g.JID)), nil
	}
	return fmt.Sprintf("The %s is still on (%s): send /disable with that community's invite link to switch it off.",
		what, onBecause(cur, in, g.JID)), nil
}

// record keeps g in the /enable list and loads the list into the running
// config; an entry already kept is left as it is. A new entry the config does
// not take is taken back out.
func (c controls) record(ctx context.Context, g client.Group, by telegram.Actor) error {
	list, err := c.a.Store.EnabledList(ctx)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(list, func(e store.Enabled) bool { return e.ID == string(g.JID) }) {
		return nil
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
// `group "X"`.
func linkTarget(g client.Group) string {
	if g.IsCommunity {
		return fmt.Sprintf("community %q", g.Name)
	}
	return fmt.Sprintf("group %q", g.Name)
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

package telegram_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/config/configtest"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// Synthetic IDs for /enable: a community the config does not list and one
// of its groups.
const (
	otherCommunity client.JID = "99999000000777@g.us"
	otherGroup     client.JID = "99999000000333@g.us"
)

// enabled lists the store's /enable entries as "kind id".
func (h *harness) enabled() []string {
	h.t.Helper()
	list, err := h.k.Store.EnabledList(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, e := range list {
		out = append(out, e.Kind+" "+e.ID)
	}
	return out
}

// on reports whether the running config moderates community (or set) id.
func (h *harness) on(id client.JID) bool {
	_, ok := h.k.Holder.Current().Rules.ModeFor(string(id))
	return ok
}

// belongsTo is the configured community a group of parent counts towards
// under the running config ("" = not moderated).
func (h *harness) belongsTo(group, parent client.JID) string {
	return pipeline.GroupCommunity(group, parent, false, h.k.Holder.Current().Rules)
}

// TestEnableCommunityLink: a community link the config does not list
// switches on the whole community, joins it through the link, survives a
// restart and a reload, and /disable switches it off again.
func TestEnableCommunityLink(t *testing.T) {
	h := newHarness(t, "")
	h.k.Fake.Invites = map[string]client.Group{
		"CoMm1":  {JID: otherCommunity, Name: "book club", IsCommunity: true},
		"GrOuP1": {JID: otherGroup, Name: "book chat", Parent: otherCommunity},
	}
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/CoMm1", 0)
	reply := h.lastReply()
	if !strings.Contains(reply, `the bot moderates the community "book club" from now on`) ||
		!strings.Contains(reply, "the bot joined it") || h.k.Fake.Count("JoinWithLink CoMm1") != 1 {
		t.Fatalf("calls %v reply %q", h.k.Fake.Calls(), reply)
	}
	if got := h.enabled(); strings.Join(got, ",") != "community "+string(otherCommunity) || !h.on(otherCommunity) {
		t.Fatalf("enabled %v, on %v", got, h.on(otherCommunity))
	}
	// Its group is on as part of it: nothing more is stored.
	if c := h.belongsTo(otherGroup, otherCommunity); c != string(otherCommunity) {
		t.Fatalf("its group belongs to %q", c)
	}
	h.command(telegramtest.ChatID, adminUser, "/status", 0)
	if !strings.Contains(h.lastReply(), "book club (enforce, switched on with /enable)") {
		t.Fatalf("status %q", h.lastReply())
	}
	// A restart: the file loaded fresh, then the store's list.
	fresh, err := config.Parse(h.k.Holder.Current().Raw, filepath.Dir(h.k.CfgPath))
	if err != nil {
		t.Fatal(err)
	}
	restarted := config.NewHolder(h.k.CfgPath, fresh)
	if _, ok := restarted.Current().Rules.ModeFor(string(otherCommunity)); ok {
		t.Fatal("the file alone lists the community")
	}
	if _, err := app.LoadEnabled(context.Background(), h.k.Store, restarted); err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.Current().Rules.ModeFor(string(otherCommunity)); !ok {
		t.Fatal("the /enable list did not survive the restart")
	}
	// A reload of the file keeps the entry (every load merges the list).
	h.command(telegramtest.ChatID, adminUser, "/reload", 0)
	if !h.on(otherCommunity) {
		t.Fatalf("a reload dropped the /enable entry: %q", h.lastReply())
	}
	// Its group is switched off with the community, not on its own.
	h.command(telegramtest.ChatID, adminUser, "/disable https://chat.whatsapp.com/GrOuP1", 0)
	if !strings.Contains(h.lastReply(), "still on (as part of book club): send /disable with that community's invite link") {
		t.Fatalf("reply %q", h.lastReply())
	}
	h.command(telegramtest.ChatID, adminUser, "/disable https://chat.whatsapp.com/CoMm1", 0)
	if !strings.Contains(h.lastReply(), `the bot no longer moderates the community "book club"`) || h.on(otherCommunity) ||
		len(h.enabled()) != 0 {
		t.Fatalf("reply %q, on %v, enabled %v", h.lastReply(), h.on(otherCommunity), h.enabled())
	}
	h.command(telegramtest.ChatID, adminUser, "/disable https://chat.whatsapp.com/CoMm1", 0)
	if h.lastReply() != `The community "book club" is not on.` {
		t.Fatalf("reply %q", h.lastReply())
	}
}

// TestEnableSingleGroupLink: a group link switches on that group alone, not
// the community it is in (a friend shares one public group only).
func TestEnableSingleGroupLink(t *testing.T) {
	h := newHarness(t, "")
	h.k.Fake.Invites = map[string]client.Group{"GrOuP1": {JID: otherGroup, Name: "public chat", Parent: otherCommunity}}
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/GrOuP1", 0)
	if !strings.Contains(h.lastReply(), `the bot moderates the group "public chat" from now on`) ||
		h.k.Fake.Count("JoinWithLink GrOuP1") != 1 {
		t.Fatalf("calls %v reply %q", h.k.Fake.Calls(), h.lastReply())
	}
	if c := h.belongsTo(otherGroup, otherCommunity); c != string(otherGroup) {
		t.Fatalf("the group belongs to %q, want its own set", c)
	}
	if h.on(otherCommunity) || strings.Join(h.enabled(), ",") != "group "+string(otherGroup) {
		t.Fatalf("community on %v, enabled %v", h.on(otherCommunity), h.enabled())
	}
	h.command(telegramtest.ChatID, adminUser, "/disable https://chat.whatsapp.com/GrOuP1", 0)
	if !strings.Contains(h.lastReply(), `the bot no longer moderates the group "public chat"`) ||
		h.belongsTo(otherGroup, otherCommunity) != "" {
		t.Fatalf("reply %q", h.lastReply())
	}
}

// TestEnableWhatIsAlreadyOn: a group of a configured community is joined
// without a new entry (the retired join command's job); one the bot is already an admin of
// is not joined again; /disable leaves the config file's communities on.
func TestEnableWhatIsAlreadyOn(t *testing.T) {
	h := newHarness(t, "")
	h.k.Fake.Invites = map[string]client.Group{
		"OuRs1": {JID: otherGroup, Name: "events", Parent: modtest.Community},
		"GeN1":  {JID: modtest.G1, Name: "general", Parent: modtest.Community},
		"CoMa1": {JID: modtest.Community, Name: "community a", IsCommunity: true},
	}
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/OuRs1", 0)
	if !strings.Contains(h.lastReply(), `The group "events" is already on (as part of community a)`) ||
		!strings.Contains(h.lastReply(), "promote the bot to admin") || h.k.Fake.Count("JoinWithLink OuRs1") != 1 {
		t.Fatalf("calls %v reply %q", h.k.Fake.Calls(), h.lastReply())
	}
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/GeN1", 0)
	if !strings.Contains(h.lastReply(), "the bot is in it and an admin there") || h.k.Fake.Count("JoinWithLink GeN1") != 0 {
		t.Fatalf("calls %v reply %q", h.k.Fake.Calls(), h.lastReply())
	}
	if len(h.enabled()) != 0 {
		t.Fatalf("enabled %v, want nothing stored for a group of a community that is on", h.enabled())
	}
	// A community the file lists: kept by /enable too, so it stays on when it
	// leaves the file (moving it into the bot's list leaves no gap).
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/CoMa1", 0)
	if !strings.Contains(h.lastReply(), `The community "community a" is already on (set in the config file); /enable now keeps it on too`) ||
		strings.Join(h.enabled(), ",") != "community "+string(modtest.Community) {
		t.Fatalf("reply %q, enabled %v", h.lastReply(), h.enabled())
	}
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/CoMa1", 0)
	if !strings.Contains(h.lastReply(), `The community "community a" is already on (set in the config file)`) || len(h.enabled()) != 1 {
		t.Fatalf("a second /enable: reply %q, enabled %v", h.lastReply(), h.enabled())
	}
	// /disable drops the entry; the file still lists the community.
	h.command(telegramtest.ChatID, adminUser, "/disable https://chat.whatsapp.com/CoMa1", 0)
	if !strings.Contains(h.lastReply(), "still on (set in the config file): remove it from the config file") ||
		!h.on(modtest.Community) || len(h.enabled()) != 0 {
		t.Fatalf("reply %q, enabled %v", h.lastReply(), h.enabled())
	}
	// Moved: /enable, then the file without it, then a reload.
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/CoMa1", 0)
	file := strings.Replace(modtest.Config, "  \"99999000000999@g.us\":\n    name: community a\n", "", 1)
	if file == modtest.Config {
		t.Fatal("the test config no longer lists community a as expected")
	}
	if err := os.WriteFile(h.k.CfgPath, []byte(configtest.Base+file), 0o600); err != nil {
		t.Fatal(err)
	}
	h.command(telegramtest.ChatID, adminUser, "/reload", 0)
	if !h.on(modtest.Community) || h.k.Holder.Current().FromFile(string(modtest.Community)) {
		t.Fatalf("after leaving the file: on %v, from file %v (%q)", h.on(modtest.Community),
			h.k.Holder.Current().FromFile(string(modtest.Community)), h.lastReply())
	}
	h.command(telegramtest.ChatID, adminUser, "/disable https://chat.whatsapp.com/CoMa1", 0)
	if !strings.Contains(h.lastReply(), `the bot no longer moderates the community "community a"`) || h.on(modtest.Community) {
		t.Fatalf("reply %q", h.lastReply())
	}
	for cmd, want := range map[string]string{
		"/enable":                                 "Usage: /enable <invite link>",
		"/disable":                                "Usage: /disable <invite link>",
		"/enable https://example.org/x":           "not a WhatsApp group invite link",
		"/disable https://example.org/x":          "not a WhatsApp group invite link",
		"/enable https://chat.whatsapp.com/NoNe1": "look up the invite link",
	} {
		h.command(telegramtest.ChatID, adminUser, cmd, 0)
		if !strings.Contains(h.lastReply(), want) {
			t.Fatalf("%s: reply %q, want %q", cmd, h.lastReply(), want)
		}
	}
	if len(h.enabled()) != 0 {
		t.Fatalf("a refused command stored %v", h.enabled())
	}
}

// TestEnableJoinFailureKeepsItOn: a join WhatsApp refuses leaves the entry on
// (the sweep lists the group as absent) and says how to retry.
func TestEnableJoinFailureKeepsItOn(t *testing.T) {
	h := newHarness(t, "")
	h.k.Fake.Invites = map[string]client.Group{"GrOuP1": {JID: otherGroup, Name: "public chat"}}
	h.k.Fake.FailNext("JoinWithLink", errors.New("join refused"))
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/GrOuP1", 0)
	if !strings.Contains(h.lastReply(), "joining it failed (join refused): send /enable again to retry") ||
		strings.Join(h.enabled(), ",") != "group "+string(otherGroup) {
		t.Fatalf("reply %q, enabled %v", h.lastReply(), h.enabled())
	}
}

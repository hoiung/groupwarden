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
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// Synthetic IDs for /enable: a community the config does not list, one of
// its groups, and a group with an older group's ID (creator's number, "-",
// creation time).
const (
	otherCommunity client.JID = "99999000000777@g.us"
	otherGroup     client.JID = "99999000000333@g.us"
	oldGroup       client.JID = "447700900123-1500000111@g.us"
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
		"/disable":                                "Usage: /disable <invite link or name>",
		"/enable https://example.org/x":           "not a WhatsApp group invite link",
		"/disable https://example.org/x":          "not a WhatsApp group invite link, nor the name of something switched on with /enable",
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

// TestEnableReplies: the bot in the group but not an admin, a join that waits
// for approval, and a second /enable of what /enable switched on.
func TestEnableReplies(t *testing.T) {
	h := newHarness(t, "")
	h.k.Fake.Invites = map[string]client.Group{
		"JoBs1":  {JID: modtest.G2, Name: "jobs", Parent: modtest.Community},
		"GrOuP1": {JID: otherGroup, Name: "public chat"},
	}
	h.k.Dir.BotNotAdmin(modtest.G2)
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/JoBs1", 0)
	if !strings.Contains(h.lastReply(), "the bot is in it but not an admin: a human admin must promote it there") ||
		h.k.Fake.Count("JoinWithLink JoBs1") != 0 {
		t.Fatalf("calls %v reply %q", h.k.Fake.Calls(), h.lastReply())
	}
	h.k.Fake.JoinPending = map[client.JID]bool{otherGroup: true}
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/GrOuP1", 0)
	if !strings.Contains(h.lastReply(), "join request sent: an admin there must approve it, then promote the bot to admin") ||
		h.k.Logged("join requested from the admin chat") != 1 {
		t.Fatalf("reply %q", h.lastReply())
	}
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/GrOuP1", 0)
	if !strings.Contains(h.lastReply(), `The group "public chat" is already on (switched on with /enable)`) ||
		h.k.Logged("enabled from the admin chat") != 1 {
		t.Fatalf("a second /enable: reply %q", h.lastReply())
	}
}

// TestEnableRefusedEntryTakenBack: an entry the config will not take (here
// an ID that is no group ID) is taken back out of the store and nothing is
// switched on; a /disable whose rebuild fails still logs who removed what.
func TestEnableRefusedEntryTakenBack(t *testing.T) {
	h := newHarness(t, "")
	h.k.Fake.Invites = map[string]client.Group{
		"OdD1":  {JID: "not-a-group", Name: "odd"},
		"CoMm1": {JID: otherCommunity, Name: "book club", IsCommunity: true},
	}
	before := h.k.Holder.Current()
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/OdD1", 0)
	if !strings.HasPrefix(h.lastReply(), "Could not do that:") || len(h.enabled()) != 0 ||
		h.k.Holder.Current() != before || h.k.Logged("enabled list not loaded") != 1 {
		t.Fatalf("reply %q, enabled %v", h.lastReply(), h.enabled())
	}
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/CoMm1", 0)
	if !h.on(otherCommunity) {
		t.Fatalf("reply %q", h.lastReply())
	}
	// A row the config cannot take, written beside the bot: the rebuild fails.
	if err := h.k.Store.Enable(context.Background(), store.Enabled{ID: "not-a-group", Kind: store.KindGroup, By: "test",
		At: h.k.Clock.Now()}); err != nil {
		t.Fatal(err)
	}
	h.command(telegramtest.ChatID, adminUser, "/disable https://chat.whatsapp.com/CoMm1", 0)
	if !strings.Contains(h.lastReply(), "removed from the /enable list, but the running config still has it") ||
		h.k.Logged("disabled from the admin chat") != 1 {
		t.Fatalf("reply %q", h.lastReply())
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

// TestDisableByName: an entry whose link no longer works (reset, revoked, the
// group deleted) is switched off by the name /status shows; the dead link
// says so, two entries of one name are refused, and a name nothing has is
// told apart from a bad link.
func TestDisableByName(t *testing.T) {
	h := newHarness(t, "")
	h.k.Fake.Invites = map[string]client.Group{
		"GrOuP1": {JID: otherGroup, Name: "Public Chat"},
		"CoMm1":  {JID: otherCommunity, Name: "book club", IsCommunity: true},
	}
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/GrOuP1", 0)
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/CoMm1", 0)
	if len(h.enabled()) != 2 {
		t.Fatalf("enabled %v", h.enabled())
	}
	delete(h.k.Fake.Invites, "GrOuP1") // the friend reset the group's link
	h.command(telegramtest.ChatID, adminUser, "/disable https://chat.whatsapp.com/GrOuP1", 0)
	if !strings.Contains(h.lastReply(), "when its link no longer works, send /disable with the name /status shows for it") ||
		len(h.enabled()) != 2 {
		t.Fatalf("a dead link: reply %q, enabled %v", h.lastReply(), h.enabled())
	}
	h.command(telegramtest.ChatID, adminUser, "/disable  public chat ", 0)
	if !strings.Contains(h.lastReply(), `the bot no longer moderates the group "Public Chat"`) ||
		h.belongsTo(otherGroup, "") != "" || strings.Join(h.enabled(), ",") != "community "+string(otherCommunity) {
		t.Fatalf("by name: reply %q, enabled %v", h.lastReply(), h.enabled())
	}
	h.command(telegramtest.ChatID, adminUser, "/disable no such chat", 0)
	if !strings.Contains(h.lastReply(), "nor the name of something switched on with /enable (/status lists them)") {
		t.Fatalf("an unknown name: reply %q", h.lastReply())
	}
	// Two entries of one name: the link tells them apart.
	if err := h.k.Store.Enable(context.Background(), store.Enabled{ID: string(otherGroup), Kind: store.KindGroup,
		Name: "Book Club", By: "test", At: h.k.Clock.Now()}); err != nil {
		t.Fatal(err)
	}
	h.command(telegramtest.ChatID, adminUser, "/disable book club", 0)
	if !strings.Contains(h.lastReply(), `2 entries switched on with /enable are named "book club"`) || len(h.enabled()) != 2 {
		t.Fatalf("an ambiguous name: reply %q, enabled %v", h.lastReply(), h.enabled())
	}
	h.command(telegramtest.ChatID, adminUser, "/disable https://chat.whatsapp.com/CoMm1", 0)
	if !strings.Contains(h.lastReply(), `the bot no longer moderates the community "book club"`) || h.on(otherCommunity) {
		t.Fatalf("by link: reply %q", h.lastReply())
	}
}

// TestEnableOlderGroupID: a group with an older group's ID is switched on
// alone like any other, and the creator's number stays out of the reply.
func TestEnableOlderGroupID(t *testing.T) {
	h := newHarness(t, "")
	h.k.Fake.Invites = map[string]client.Group{"OlD1": {JID: oldGroup, Name: "old chat"}}
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/OlD1", 0)
	if !strings.Contains(h.lastReply(), `the bot moderates the group "old chat" from now on`) ||
		h.belongsTo(oldGroup, "") != string(oldGroup) {
		t.Fatalf("reply %q, enabled %v", h.lastReply(), h.enabled())
	}
}

// TestEnableWarnsOfTheFilesOwnSettings: /enable of a community the file
// gives its own settings says that, out of the file, it runs on the global
// ones (a shadow community would start enforcing).
func TestEnableWarnsOfTheFilesOwnSettings(t *testing.T) {
	h := newHarness(t, "  \""+string(otherCommunity)+"\":\n    name: shadowed\n    mode: shadow\n")
	h.k.Fake.Invites = map[string]client.Group{
		"ShAd1": {JID: otherCommunity, Name: "shadowed", IsCommunity: true},
		"CoMa1": {JID: modtest.Community, Name: "community a", IsCommunity: true},
	}
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/ShAd1", 0)
	if !strings.Contains(h.lastReply(), "stays on once it leaves the file (then with the global settings, not the file's own mode for it)") {
		t.Fatalf("reply %q", h.lastReply())
	}
	h.command(telegramtest.ChatID, adminUser, "/enable https://chat.whatsapp.com/CoMa1", 0)
	if strings.Contains(h.lastReply(), "global settings") {
		t.Fatalf("a community with only a name: reply %q", h.lastReply())
	}
}

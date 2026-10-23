package telegram_test

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config/configtest"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// The acl: people (Telegram user IDs). None is a Telegram admin of the chat;
// adminUser is one, and is in no list.
const (
	extraSuper int64 = 510 // super admin listed first; no test acts as them
	superUser  int64 = 511 // super admin
	commAdmin  int64 = 512 // community admin of otherCommunity
	commAdmin2 int64 = 513 // community admin of secondCommunity
)

// secondCommunity is a community the config file does not moderate and the bot
// is not in, so it and a group in it can be switched on with /enable.
const secondCommunity client.JID = "99999000000444@g.us"

// aclSection names superUser super admin, commAdmin community admin of
// otherCommunity and commAdmin2 community admin of secondCommunity, so each
// admin is decided on their own list. superUser and commAdmin's community are
// each the second entry of their list and commAdmin2's the only one, so a gate
// that reads one end of a list is caught.
var aclSection = "acl:\n  super_admins: [" + strconv.FormatInt(extraSuper, 10) + ", " + strconv.FormatInt(superUser, 10) +
	"]\n  admins:\n    - user: " + strconv.FormatInt(commAdmin, 10) +
	"\n      assigned: [\"" + string(modtest.G2) + "\", \"" + string(otherCommunity) + "\"]\n    - user: " +
	strconv.FormatInt(commAdmin2, 10) + "\n      assigned: [\"" + string(secondCommunity) + "\"]\n"

// aclSubject is one person the acl: tests act as.
type aclSubject struct {
	name string
	user int64
	role telegram.Role
}

var aclSubjects = []aclSubject{
	{"super", superUser, telegram.RoleSuper},
	{"admin", commAdmin, telegram.RoleAdmin},
	{"member", otherUser, telegram.RoleMember},
	{"tgadmin", adminUser, telegram.RoleMember}, // a Telegram admin the file does not name
}

// aclCommands are the eight commands and one the bot does not know.
var aclCommands = []string{"status", "pause", "resume", "reload", "enable", "disable", "ban", "unban", "unknown"}

// aclButtons are the seven buttons' callback codes.
var aclButtons = []string{"undo", "ban", "resume", "addban", "no", "show", "done"}

// mayUnderACL is the role table (Issue #6 Before/After), written out rather
// than read from the code it checks.
var mayUnderACL = map[string][]string{
	"super": {"cmd-status", "cmd-pause", "cmd-resume", "cmd-reload", "cmd-enable", "cmd-disable", "cmd-ban", "cmd-unban",
		"cmd-unknown", "btn-undo", "btn-ban", "btn-resume", "btn-addban", "btn-no", "btn-show", "btn-done"},
	"admin": {"cmd-status", "cmd-enable", "cmd-disable", "cmd-ban", "cmd-unban", "cmd-unknown", "btn-undo", "btn-ban",
		"btn-addban", "btn-no", "btn-show"},
	"member":  {"cmd-ban", "cmd-unban", "cmd-unknown", "btn-undo", "btn-ban", "btn-addban", "btn-no", "btn-show"},
	"tgadmin": {"cmd-ban", "cmd-unban", "cmd-unknown", "btn-undo", "btn-ban", "btn-addban", "btn-no", "btn-show"},
}

// aclActions is every action the role function decides: the commands, then
// the buttons.
func aclActions() []string {
	var out []string
	for _, c := range aclCommands {
		out = append(out, "cmd-"+c)
	}
	for _, b := range aclButtons {
		out = append(out, "btn-"+b)
	}
	return out
}

// TestRoleTable: under acl: the file alone gives each person their role and
// what it may do, with no Telegram call; without it a Telegram admin of the
// chat may do everything and anyone else nothing.
func TestRoleTable(t *testing.T) {
	h := newHarness(t, aclSection)
	acl := h.k.Holder.Current().Config.ACL
	if acl == nil {
		t.Fatal("the test config has no acl: section")
	}
	ctx := context.Background()
	for _, s := range aclSubjects {
		for _, action := range aclActions() {
			t.Run("acl:"+s.name+":"+action, func(t *testing.T) {
				asked := len(h.srv.Requests("getChatMember"))
				ok, role, _ := h.chat.Decide(ctx, acl, s.user, action)
				if want := slices.Contains(mayUnderACL[s.name], action); ok != want || role != s.role {
					t.Fatalf("allowed %v role %q, want %v %q", ok, role, want, s.role)
				}
				if n := len(h.srv.Requests("getChatMember")); n != asked {
					t.Fatal("the decision asked Telegram; under acl: the file alone decides")
				}
			})
		}
	}
	for _, s := range []struct {
		name string
		user int64
		may  bool
	}{{"tgadmin", adminUser, true}, {"nonadmin", otherUser, false}} {
		for _, action := range aclActions() {
			t.Run("noacl:"+s.name+":"+action, func(t *testing.T) {
				if ok, role, _ := h.chat.Decide(ctx, nil, s.user, action); ok != s.may || role != "" {
					t.Fatalf("allowed %v role %q, want %v and no role", ok, role, s.may)
				}
			})
		}
	}
}

// TestEveryActionHasARole: every command and button has its entry in the
// table the gate reads, and the table has nothing else (a missing one would
// need the super admin).
func TestEveryActionHasARole(t *testing.T) {
	want := []string{"cmd-unknown"}
	for _, c := range telegram.AdminCommandNames() {
		want = append(want, "cmd-"+c)
	}
	for _, code := range telegram.ButtonCodes {
		want = append(want, "btn-"+code)
	}
	var got []string
	for action := range telegram.ActionNeeds {
		got = append(got, action)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("actions with a role %v, want %v", got, want)
	}
}

// commandRefusals is the reply each refusable command gets under acl:.
var commandRefusals = map[string]string{
	"status":  "Only community admins and the super admin can use /status.",
	"enable":  "Only community admins and the super admin can use /enable.",
	"disable": "Only community admins and the super admin can use /disable.",
	"pause":   "Only the super admin can use /pause.",
	"resume":  "Only the super admin can use /resume.",
	"reload":  "Only the super admin can use /reload.",
}

// pauses counts the active pauses.
func (h *harness) pauses() int {
	h.t.Helper()
	ps, err := h.k.Store.Pauses(h.k.Ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return len(ps)
}

// TestCommandRoles: under acl: each person's bare commands get past the gate
// or are refused with who may use them; a refused /pause pauses nothing.
func TestCommandRoles(t *testing.T) {
	for _, s := range aclSubjects {
		h := newHarness(t, aclSection)
		for _, cmd := range aclCommands {
			t.Run(s.name+":"+cmd, func(t *testing.T) {
				text := "/" + cmd
				if cmd == "unknown" {
					text = "/help"
				}
				paused := h.pauses()
				h.command(telegramtest.ChatID, s.user, text, 0)
				reply := h.lastReply()
				if slices.Contains(mayUnderACL[s.name], "cmd-"+cmd) {
					if strings.HasPrefix(reply, "Only ") {
						t.Fatalf("%s refused: %q", text, reply)
					}
					if cmd == "pause" && h.pauses() == paused {
						t.Fatalf("an allowed /pause paused nothing: %q", reply)
					}
					return
				}
				if reply != commandRefusals[cmd] {
					t.Fatalf("%s: reply %q, want %q", text, reply, commandRefusals[cmd])
				}
				if h.pauses() != paused {
					t.Fatalf("a refused %s changed the pauses", text)
				}
			})
		}
	}
}

// writeConfig replaces the config file with the kit's config plus extra,
// without loading it.
func (h *harness) writeConfig(extra string) {
	h.t.Helper()
	if err := os.WriteFile(h.k.CfgPath, []byte(configtest.Base+modtest.Config+extra), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// TestACLReload: an edit to acl: decides from the next command after
// /reload; a malformed one is refused and the old list keeps deciding.
func TestACLReload(t *testing.T) {
	t.Run("applies", func(t *testing.T) {
		h := newHarness(t, "")
		h.command(telegramtest.ChatID, otherUser, "/pause", 0)
		if h.lastReply() != "Only admins of this chat can use groupwarden commands." || h.pauses() != 0 {
			t.Fatalf("before: reply %q, pauses %d", h.lastReply(), h.pauses())
		}
		h.writeConfig("acl:\n  super_admins: [" + strconv.FormatInt(otherUser, 10) + "]\n")
		h.command(telegramtest.ChatID, adminUser, "/reload", 0)
		if !strings.Contains(h.lastReply(), "loaded") {
			t.Fatalf("reload: %q", h.lastReply())
		}
		h.command(telegramtest.ChatID, otherUser, "/pause", 0)
		if strings.HasPrefix(h.lastReply(), "Only ") || h.pauses() == 0 {
			t.Fatalf("after: reply %q, pauses %d", h.lastReply(), h.pauses())
		}
		// The Telegram admin the file does not name is now a member.
		h.command(telegramtest.ChatID, adminUser, "/status", 0)
		if h.lastReply() != commandRefusals["status"] {
			t.Fatalf("a Telegram admin not in the file: %q", h.lastReply())
		}
	})
	t.Run("malformed_keeps_old", func(t *testing.T) {
		h := newHarness(t, "acl:\n  super_admins: ["+strconv.FormatInt(otherUser, 10)+"]\n")
		h.writeConfig("acl:\n  super_admins: []\n")
		h.command(telegramtest.ChatID, otherUser, "/reload", 0)
		if !contains(h.lastReply(), "REJECTED", "acl.super_admins") {
			t.Fatalf("reload of a bad acl: %q", h.lastReply())
		}
		h.command(telegramtest.ChatID, adminUser, "/pause", 0)
		if h.lastReply() != commandRefusals["pause"] || h.pauses() != 0 {
			t.Fatalf("the Telegram admin under the old list: reply %q, pauses %d", h.lastReply(), h.pauses())
		}
		h.command(telegramtest.ChatID, otherUser, "/pause", 0)
		if strings.HasPrefix(h.lastReply(), "Only ") || h.pauses() == 0 {
			t.Fatalf("the old super admin: reply %q, pauses %d", h.lastReply(), h.pauses())
		}
	})
}

// allButtonsReport posts a report carrying every button and returns its ID.
func (h *harness) allButtonsReport() int64 {
	h.t.Helper()
	id := h.report(store.Report{Kind: "would_remove", Text: "a report", Buttons: []string{ledger.ButtonUndo, ledger.ButtonBan,
		ledger.ButtonResume, ledger.ButtonAddToBanList, ledger.ButtonNo, ledger.ButtonShowAttachment, ledger.ButtonDone}})
	h.drain()
	return id
}

// unclaimed reports whether nothing was recorded for a press of button on
// the report (it claims the press to find out).
func (h *harness) unclaimed(reportID int64, button string) bool {
	h.t.Helper()
	_, claimed, err := h.k.Store.ClaimPress(h.k.Ctx, store.Press{ReportID: reportID, Button: button})
	if err != nil {
		h.t.Fatal(err)
	}
	return claimed
}

// TestButtonRoles: under acl: each person's press gets past the gate or is
// answered with who may press it and not recorded.
func TestButtonRoles(t *testing.T) {
	names := map[string]string{}
	for name, code := range telegram.ButtonCodes {
		names[code] = name
	}
	for _, s := range aclSubjects {
		h := newHarness(t, aclSection)
		id := h.allButtonsReport()
		for _, code := range aclButtons {
			t.Run(s.name+":"+code, func(t *testing.T) {
				h.press(s.user, code, id)
				answers := h.answers()
				answer := answers[len(answers)-1]
				if slices.Contains(mayUnderACL[s.name], "btn-"+code) {
					if strings.HasPrefix(answer, "Only ") {
						t.Fatalf("[%s] refused: %q", names[code], answer)
					}
					return
				}
				if want := "Only the super admin can press [" + names[code] + "]."; answer != want {
					t.Fatalf("answer %q, want %q", answer, want)
				}
				if !h.unclaimed(id, names[code]) {
					t.Fatal("a refused press was recorded")
				}
			})
		}
	}
}

// enableHarness is an acl: harness whose invite links point at commAdmin's
// community, a group in it, commAdmin2's community, a group in it, and a group
// in no community.
func enableHarness(t *testing.T) *harness {
	h := newHarness(t, aclSection)
	h.k.Fake.Invites = map[string]client.Group{
		"CoMm1":  {JID: otherCommunity, Name: "our community", IsCommunity: true},
		"CoMm2":  {JID: secondCommunity, Name: "their community", IsCommunity: true},
		"GrOuP1": {JID: otherGroup, Name: "public chat", Parent: otherCommunity},
		"ElSeW1": {JID: otherGroup, Name: "public chat", Parent: secondCommunity},
		"AlOnE1": {JID: otherGroup, Name: "public chat"},
	}
	return h
}

// notAssigned is the reply to a community admin's /enable or /disable of
// something not theirs.
const notAssigned = "You can switch on or off only the communities and groups assigned to you."

// TestEnableAssignedOnly: a community admin's /enable records and joins only
// what is assigned to them (or a group in it), never a group in another
// admin's community; the super admin's anything.
func TestEnableAssignedOnly(t *testing.T) {
	for _, c := range []struct {
		name, code string
		user       int64
		entry      string // "" = refused
	}{
		{"own_community", "CoMm1", commAdmin, "community " + string(otherCommunity)},
		{"group_in_own_community", "GrOuP1", commAdmin, "group " + string(otherGroup)},
		{"other", "AlOnE1", commAdmin, ""},
		{"group_in_other_community", "ElSeW1", commAdmin, ""},
		{"second_admin_own", "ElSeW1", commAdmin2, "group " + string(otherGroup)},
		{"second_admin_community", "CoMm2", commAdmin2, "community " + string(secondCommunity)},
		{"second_admin_other", "CoMm1", commAdmin2, ""},
		{"super_other", "AlOnE1", superUser, "group " + string(otherGroup)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := enableHarness(t)
			h.command(telegramtest.ChatID, c.user, "/enable https://chat.whatsapp.com/"+c.code, 0)
			reply, got, joins := h.lastReply(), strings.Join(h.enabled(), ","), h.k.Fake.Count("JoinWithLink "+c.code)
			if c.entry == "" {
				if reply != notAssigned || got != "" || joins != 0 {
					t.Fatalf("reply %q, enabled %q, joins %d", reply, got, joins)
				}
				return
			}
			if got != c.entry || joins != 1 || strings.HasPrefix(reply, "Could not do that:") {
				t.Fatalf("reply %q, enabled %q, joins %d", reply, got, joins)
			}
		})
	}
}

// TestDisableAssignedOnly: a community admin's /disable, by link or by the
// name /status shows, removes only what is in their community, not a group in
// another admin's community.
func TestDisableAssignedOnly(t *testing.T) {
	for _, c := range []struct {
		name, code, disable string
		learned             client.JID // the community the bot learned the group is in ("" = not learned)
		removed             bool
	}{
		{"own_link", "GrOuP1", "https://chat.whatsapp.com/GrOuP1", "", true},
		{"own_name", "GrOuP1", "public chat", otherCommunity, true},
		{"other_link", "AlOnE1", "https://chat.whatsapp.com/AlOnE1", "", false},
		{"other_name", "AlOnE1", "public chat", "", false},
		{"other_community_link", "ElSeW1", "https://chat.whatsapp.com/ElSeW1", "", false},
		{"other_community_name", "ElSeW1", "public chat", secondCommunity, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := enableHarness(t)
			if c.learned != "" {
				h.k.Dir.Join(client.Group{JID: otherGroup, Name: "public chat", Parent: c.learned})
			}
			h.command(telegramtest.ChatID, superUser, "/enable https://chat.whatsapp.com/"+c.code, 0)
			if len(h.enabled()) != 1 {
				t.Fatalf("set-up /enable: %q, enabled %v", h.lastReply(), h.enabled())
			}
			h.command(telegramtest.ChatID, commAdmin, "/disable "+c.disable, 0)
			reply := h.lastReply()
			if c.removed {
				if len(h.enabled()) != 0 || !strings.Contains(reply, "no longer moderates") {
					t.Fatalf("reply %q, enabled %v", reply, h.enabled())
				}
				return
			}
			if reply != notAssigned || strings.HasPrefix(reply, "Could not do that:") || len(h.enabled()) != 1 {
				t.Fatalf("reply %q, enabled %v", reply, h.enabled())
			}
		})
	}
}

// warnLines are the chat's Warn lines carrying msg.
func (h *harness) warnLines(msg string) []string {
	var out []string
	for _, l := range strings.Split(h.logs.String(), "\n") {
		if strings.Contains(l, "level=WARN") && strings.Contains(l, `msg="`+msg+`"`) {
			out = append(out, l)
		}
	}
	return out
}

// TestACLRefusalLogged: a refused command or press under acl: is one Warn
// line with the user ID, their role and the role needed.
func TestACLRefusalLogged(t *testing.T) {
	user := "user=" + strconv.FormatInt(otherUser, 10)
	t.Run("command", func(t *testing.T) {
		h := newHarness(t, aclSection)
		h.command(telegramtest.ChatID, otherUser, "/pause", 0)
		if lines := h.warnLines("command refused"); len(lines) != 1 ||
			!contains(lines[0], user, "command=pause", "role=member", `needs="super admin"`) {
			t.Fatalf("lines %q", lines)
		}
	})
	t.Run("button", func(t *testing.T) {
		h := newHarness(t, aclSection)
		h.press(otherUser, "resume", h.allButtonsReport())
		if lines := h.warnLines("button press refused"); len(lines) != 1 ||
			!contains(lines[0], user, "button=Resume", "role=member", `needs="super admin"`) {
			t.Fatalf("lines %q", lines)
		}
	})
}

// whoLine is the list's line saying who may use what under acl:.
const whoLine = "Everyone in this chat can use /ban and /unban."

// TestCommandListACL: without acl: the list is for the chat's admins; with
// it, one text for everyone says who may use what, and a config load that
// changes it edits the pinned list in place.
func TestCommandListACL(t *testing.T) {
	t.Run("noacl", func(t *testing.T) {
		h := newHarness(t, "")
		h.command(telegramtest.ChatID, adminUser, "/help", 0)
		if !strings.Contains(h.lastReply(), "admins of this chat only") || strings.Contains(h.lastReply(), whoLine) {
			t.Fatalf("reply %q", h.lastReply())
		}
	})
	t.Run("acl_reply", func(t *testing.T) {
		h := newHarness(t, aclSection)
		h.command(telegramtest.ChatID, otherUser, "/help", 0)
		if strings.Contains(h.lastReply(), "admins of this chat only") || !strings.Contains(h.lastReply(), whoLine) {
			t.Fatalf("reply %q", h.lastReply())
		}
	})
	t.Run("acl_pinned_after_reload", func(t *testing.T) {
		h := newHarness(t, "")
		ctx, cancel := context.WithCancel(h.k.Ctx)
		done := make(chan struct{})
		go func() {
			h.chat.SetupLoop(ctx)
			close(done)
		}()
		defer func() {
			cancel()
			<-done
		}()
		waitFor(t, func() bool { _, _, state := h.pinStatus(); return state == "pinned" })
		h.writeConfig(aclSection)
		h.command(telegramtest.ChatID, adminUser, "/reload", 0)
		want := telegram.CommandList(h.k.Holder.Current().Config.ACL)
		if !strings.Contains(want, whoLine) {
			t.Fatalf("the reload did not load the acl: section: %q", h.lastReply())
		}
		waitFor(t, func() bool {
			edits := h.srv.Requests("editMessageText")
			return len(edits) == 1 && edits[0].Params["text"] == want
		})
		if posts := requests(h, "sendMessage", telegram.CommandList(nil)); posts != 1 {
			t.Fatalf("the list was posted %d times, want once (then edited)", posts)
		}
	})
}

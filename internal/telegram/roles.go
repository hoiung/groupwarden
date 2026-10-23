package telegram

import (
	"context"
	"slices"

	"github.com/hoiung/groupwarden/internal/config"
)

// Role is what a person may do in the admin chat under the config's acl:
// section (docs/config.md "Admin-chat access"). Without acl: the gate gives
// no role ("").
type Role string

const (
	RoleMember Role = "member"
	RoleAdmin  Role = "community admin"
	RoleSuper  Role = "super admin"
)

// rank orders the roles: each may do what the roles below it may.
var rank = map[Role]int{RoleMember: 1, RoleAdmin: 2, RoleSuper: 3}

// actionNeeds is the role each command ("cmd-<name>", "cmd-unknown" for one
// the bot does not know) and button ("btn-<code>") needs under acl:. An action
// missing here needs the super admin (fail closed).
var actionNeeds = map[string]Role{
	"cmd-status":  RoleAdmin,
	"cmd-enable":  RoleAdmin,
	"cmd-disable": RoleAdmin,
	"cmd-pause":   RoleSuper,
	"cmd-resume":  RoleSuper,
	"cmd-reload":  RoleSuper,
	"cmd-ban":     RoleMember,
	"cmd-unban":   RoleMember,
	"cmd-unknown": RoleMember,
	"btn-undo":    RoleMember,
	"btn-ban":     RoleMember,
	"btn-addban":  RoleMember,
	"btn-no":      RoleMember,
	"btn-show":    RoleMember,
	"btn-resume":  RoleSuper,
	"btn-done":    RoleSuper,
}

// commandAction is the action a command is decided as.
func commandAction(cmd string) string {
	if slices.ContainsFunc(adminCommands, func(c adminCommand) bool { return c.name == cmd }) {
		return "cmd-" + cmd
	}
	return "cmd-unknown"
}

// access is one decision: whether the person may, their role and assigned
// list (both unset without acl:), and the role the action needs.
type access struct {
	allowed  bool
	role     Role
	needs    Role
	assigned []string
}

// decide says whether userID may do action, under acl as read once for this
// decision. With acl the file alone decides (anyone it does not name is a
// member, a Telegram admin of the chat included), with no Telegram call;
// without it every Telegram admin of the chat may do everything and nobody
// else anything.
func (c *Chat) decide(ctx context.Context, acl *config.ACL, userID int64, action string) access {
	if acl == nil {
		return access{allowed: c.isAdmin(ctx, userID)}
	}
	a := access{role: RoleMember, needs: RoleSuper}
	if n, ok := actionNeeds[action]; ok {
		a.needs = n
	}
	if slices.Contains(acl.SuperAdmins, userID) {
		a.role = RoleSuper
	} else if i := slices.IndexFunc(acl.Admins, func(ad config.ACLAdmin) bool { return ad.User == userID }); i >= 0 {
		a.role, a.assigned = RoleAdmin, acl.Admins[i].Assigned
	}
	a.allowed = rank[a.role] >= rank[a.needs]
	return a
}

// whoMay names the people a role's actions are open to.
func whoMay(needs Role) string {
	if needs == RoleAdmin {
		return "community admins and the super admin"
	}
	return "the super admin"
}

// commandRefusal is the reply to a command refused under acl:.
func commandRefusal(cmd string, needs Role) string {
	return "Only " + whoMay(needs) + " can use /" + cmd + "."
}

// buttonRefusal is the answer to a press refused under acl:.
func buttonRefusal(button string, needs Role) string {
	return "Only " + whoMay(needs) + " can press [" + button + "]."
}

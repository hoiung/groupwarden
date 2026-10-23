package config

import (
	"reflect"
	"testing"
)

// Synthetic Telegram user IDs.
const (
	tgSuper = 111111111
	tgAdmin = 222222222
)

// olderGroup is an older group's ID ("<number>-<number>@g.us").
const olderGroup = "447700900123-1500000111@g.us"

func TestACLParses(t *testing.T) {
	cases := []struct {
		name, body string
		want       *ACL
	}{
		{"before_after_example", "acl:\n  super_admins: [111111111]\n  admins:\n    - user: 222222222\n" +
			"      assigned: [\"" + communityA + "\"]\n",
			&ACL{SuperAdmins: []int64{tgSuper}, Admins: []ACLAdmin{{User: tgAdmin, Assigned: []string{communityA}}}}},
		{"older_group_id", "acl:\n  super_admins: [111111111]\n  admins:\n    - user: 222222222\n" +
			"      assigned: [\"" + olderGroup + "\", \"" + groupSet + "\"]\n",
			&ACL{SuperAdmins: []int64{tgSuper}, Admins: []ACLAdmin{{User: tgAdmin, Assigned: []string{olderGroup, groupSet}}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parse(t, c.body).Config.ACL; !reflect.DeepEqual(got, c.want) {
				t.Fatalf("acl = %+v, want %+v", got, c.want)
			}
		})
	}
}

// TestNoACLSection: the public example has no acl: section, so a deployer who
// copies it keeps today's rule (every Telegram admin of the chat).
func TestNoACLSection(t *testing.T) {
	l, err := Read("../../examples/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if l.Config.ACL != nil {
		t.Fatalf("examples/config.yaml has acl %+v, want none", l.Config.ACL)
	}
}

func TestACLRejects(t *testing.T) {
	t.Run("no_super_admin", func(t *testing.T) {
		refused(t, "acl:\n  super_admins: []\n", `line 6: acl\.super_admins needs at least 1 entry`)
		refused(t, "acl:\n  admins: []\n", `acl\.super_admins is required`)
	})
	t.Run("bad_user_id", func(t *testing.T) {
		refused(t, "acl:\n  super_admins: [0]\n", `line 6: acl\.super_admins\.0 0 is below the minimum 1`)
		refused(t, "acl:\n  super_admins: [111111111]\n  admins:\n    - user: \"222222222\"\n",
			`line 8: acl\.admins\.0\.user must be a whole number`)
	})
	t.Run("bad_assigned_entry", func(t *testing.T) {
		refused(t, "acl:\n  super_admins: [111111111]\n  admins:\n    - user: 222222222\n      assigned: [\"my community\"]\n",
			`line 9: acl\.admins\.0\.assigned\.0 is not in the expected form`)
	})
	t.Run("user_twice_in_admins", func(t *testing.T) {
		refused(t, "acl:\n  super_admins: [111111111]\n  admins:\n    - user: 222222222\n    - user: 222222222\n",
			`line 9: acl\.admins\.1\.user is listed twice in acl\.admins: one person has one role`)
	})
	t.Run("user_in_both_lists", func(t *testing.T) {
		refused(t, "acl:\n  super_admins: [111111111]\n  admins:\n    - user: 111111111\n",
			`line 8: acl\.admins\.0\.user is also in acl\.super_admins: one person has one role`)
	})
}

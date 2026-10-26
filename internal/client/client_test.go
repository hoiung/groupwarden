package client

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Synthetic IDs (listed exactly in .secret-pii-allowlist). A device address
// ("user:N@server") reads as an email address, so it is built at run time.
const (
	lid   JID = "99999000000444@lid"
	phone JID = "447700900123@s.whatsapp.net"
	group JID = "99999000000111@g.us"
)

func device(j JID, n string) JID { return JID(j.User() + ":" + n + "@" + j.Server()) }

// TestJIDParts: the user, server and bare forms of an address, with any
// device suffix dropped.
func TestJIDParts(t *testing.T) {
	for _, c := range []struct {
		j            JID
		user, server string
		bare         JID
	}{
		{lid, "99999000000444", "lid", lid},
		{device(lid, "3"), "99999000000444", "lid", lid},
		{device(phone, "12"), "447700900123", "s.whatsapp.net", phone},
		{group, "99999000000111", "g.us", group},
		{"", "", "", ""},
	} {
		if c.j.User() != c.user || c.j.Server() != c.server || c.j.Bare() != c.bare {
			t.Errorf("%q: user %q server %q bare %q", c.j, c.j.User(), c.j.Server(), c.j.Bare())
		}
	}
}

// TestMemberOf: a person's addresses sort into LID and phone (device
// suffixes dropped, the first of each kind kept); a group or an empty
// address is neither. The ban-list key is the LID, else the phone number.
func TestMemberOf(t *testing.T) {
	m := MemberOf(device(phone, "2"), "", group, device(lid, "5"), "99999000000555@lid")
	if m.LID != lid || m.Phone != phone {
		t.Fatalf("member %+v", m)
	}
	if m.Key() != string(lid) || !slices.Equal(m.IDs(), []string{string(lid), string(phone)}) {
		t.Fatalf("key %q ids %v", m.Key(), m.IDs())
	}
	if p := MemberOf(phone); p.Key() != string(phone) || !slices.Equal(p.IDs(), []string{string(phone)}) {
		t.Fatalf("phone only: key %q ids %v", p.Key(), p.IDs())
	}
	if none := MemberOf(group, ""); none.Key() != "" || len(none.IDs()) != 0 {
		t.Fatalf("no person: %+v", none)
	}
}

// TestInviteCode: the code of an invite link, with or without a scheme, a
// query or a trailing slash, or a bare code; anything else is refused.
func TestInviteCode(t *testing.T) {
	for in, want := range map[string]string{
		"https://chat.whatsapp.com/AbCdEf123":       "AbCdEf123",
		"  https://chat.whatsapp.com/AbCdEf123/  ":  "AbCdEf123",
		"chat.whatsapp.com/AbCdEf123?mode=r_c":      "AbCdEf123",
		"http://chat.whatsapp.com/invite/AbCdEf123": "AbCdEf123",
		"AbCdEf123": "AbCdEf123",
		"https://chat.whatsapp.com/AbCdEf123?x=a/b.c@d": "AbCdEf123",
	} {
		if got, err := InviteCode(in); err != nil || got != want {
			t.Errorf("InviteCode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "https://chat.whatsapp.com/", "https://example.org/AbCdEf123",
		"t.me/AbCdEf123", "chat.whatsapp.com", "Ab Cd", "Ab:Cd", "someone@example"} {
		if got, err := InviteCode(in); err == nil || !strings.Contains(err.Error(), "is not a WhatsApp group invite link") {
			t.Errorf("InviteCode(%q) = %q, %v; want a refusal", in, got, err)
		}
	}
}

// TestMaskMentionsLongestFirst: each mentioned member's "@<user>" becomes
// the marker, a longer number first so a member whose number starts with
// another's is masked whole; text with no mentions is left as it is.
func TestMaskMentionsLongestFirst(t *testing.T) {
	// short's user is a prefix of long's (built from it: the scanner allows
	// only the listed IDs).
	long := lid
	short := JID(long.User()[:11] + "@" + long.Server())
	text := "hi @" + long.User() + " and @" + short.User() + ", also 447700900123 unmentioned"
	got := MaskMentions(text, []JID{short, long, device(phone, "1")})
	if want := "hi @mention and @mention, also 447700900123 unmentioned"; got != want {
		t.Fatalf("masked %q, want %q", got, want)
	}
	if got := MaskMentions(text, nil); got != text {
		t.Fatalf("no mentions changed the text: %q", got)
	}
	if got := MaskMentions(text, []JID{""}); got != text {
		t.Fatalf("an empty mention changed the text: %q", got)
	}
}

// TestDedupeKeys: a message (and an undecryptable one) is keyed by chat and
// ID; a group change by everything it says, so a different change at the
// same time is a different delivery; the bot's own join by group and time.
func TestDedupeKeys(t *testing.T) {
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if k := (&Message{Chat: group, ID: "M1", Sender: lid}).DedupeKey(); k != "msg|"+string(group)+"|M1" {
		t.Errorf("message key %q", k)
	}
	if a, b := (&Message{Chat: group, ID: "M1"}).DedupeKey(), (&Undecryptable{Chat: group, ID: "M1"}).DedupeKey(); a != b {
		t.Errorf("a message and its undecryptable delivery: %q, %q", a, b)
	}
	join := &GroupChange{Group: group, Time: at, Joined: []JID{lid}}
	leave := &GroupChange{Group: group, Time: at, Left: []JID{lid}}
	promote := &GroupChange{Group: group, Time: at, Promoted: []JID{lid}}
	byAdmin := &GroupChange{Group: group, Time: at, Joined: []JID{lid}, Actor: "99999000000555@lid"}
	keys := map[string]bool{}
	for _, g := range []*GroupChange{join, leave, promote, byAdmin} {
		keys[g.DedupeKey()] = true
	}
	if len(keys) != 4 {
		t.Errorf("group change keys collide: %v", keys)
	}
	if again := (&GroupChange{Group: group, Time: at.In(time.FixedZone("x", 3600)), Joined: []JID{lid}}); again.DedupeKey() != join.DedupeKey() {
		t.Errorf("the same change in another zone: %q vs %q", again.DedupeKey(), join.DedupeKey())
	}
	if k := (&JoinedGroup{Group: group, Time: at}).DedupeKey(); k != "joined|"+string(group)+"|2026-10-06T12:00:00Z" {
		t.Errorf("joined key %q", k)
	}
}

// TestFatalLifecycles: the kinds that end the process each name the human
// step; the others do not.
func TestFatalLifecycles(t *testing.T) {
	for _, k := range []LifecycleKind{LoggedOut, StreamReplaced, ClientOutdated, CATRefreshFailed, ConnectFailure} {
		if !k.Fatal() || k.NextStep() == "" {
			t.Errorf("%s: fatal %v, next step %q", k, k.Fatal(), k.NextStep())
		}
	}
	for _, k := range []LifecycleKind{Connected, Disconnected, CaughtUp, TemporaryBan, ""} {
		if k.Fatal() || k.NextStep() != "" {
			t.Errorf("%s: fatal %v, next step %q", k, k.Fatal(), k.NextStep())
		}
	}
}

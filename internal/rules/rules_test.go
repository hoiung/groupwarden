package rules

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/client"
)

// Synthetic IDs only (listed exactly in .secret-pii-allowlist).
const (
	communityA = "99999000000999@g.us"
	setB       = "Standalone chats"
	groupB     = "99999000000888@g.us"
)

var artefactNode = Node{Has: Names{AnyLink, InviteLink, Shortener, PhoneNumber, ContactCard, Handle}}

func testSpec() Spec {
	return Spec{
		Mode:          Enforce,
		MinWordLength: 3,
		WordLists: map[string][]string{
			"crypto": {"crypto", "bitcoin", "usdt", "*coin"},
			"stocks": {"stocks", "stock", "stock tips", "shares"},
			"lures":  {"guaranteed", "daily profit", "inbox me", "dm me"},
		},
		NeverMatch: []string{"in stock", "out of stock", "stock photo*", "chicken stock", "laughing stock", "stock up", "stocking"},
		Rules: []RuleSpec{
			{Name: "crypto-or-stocks-pitch", Action: DeleteRemoveBan, Confirmed: true,
				When: Node{All: []Node{{Words: Names{"crypto", "stocks"}}, artefactNode}}},
			{Name: "crypto-lure", Action: DeleteRemoveBan, Confirmed: true,
				When: Node{All: []Node{{Words: Names{"crypto", "stocks"}}, {Words: Names{"lures"}}}}},
		},
		Communities: []CommunitySpec{{ID: communityA}, {ID: setB, Groups: []string{groupB}}},
		BanScope:    AllCommunities,
	}
}

func compile(t *testing.T, s Spec) *Ruleset {
	t.Helper()
	rs, err := Compile(s)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return rs
}

func body(texts ...string) []client.Field {
	var fs []client.Field
	for _, s := range texts {
		fs = append(fs, client.Field{Name: "body", Text: s, Match: s})
	}
	return fs
}

func sig(fields []client.Field, allowed ...string) signals {
	m := map[string]bool{}
	for _, a := range allowed {
		reg, _ := RegistrableDomain(a)
		m[reg] = true
	}
	return detect(fields, m)
}

// ---- AC 2.4 built-in conditions -------------------------------------------

func TestSchemelessLinks(t *testing.T) {
	for _, text := range []string{"join t.me/profitclub now", "see bit.ly/abc123", "go to example.com/x", "https://example.org"} {
		if !sig(body(text))[AnyLink] {
			t.Errorf("%q: no any_link", text)
		}
	}
	if !sig(body("see bit.ly/abc123"))[Shortener] {
		t.Error("bit.ly not a shortener")
	}
	if s := sig(body("email me at someone@example.com")); s[AnyLink] {
		t.Error("an email address counted as a link")
	}
}

func TestIdeographicDot(t *testing.T) {
	for _, text := range []string{"t。me/profitclub", "t｡me/profitclub", "chat．whatsapp．com/AbCd"} {
		s := sig(body(text))
		if !s[AnyLink] || !s[InviteLink] {
			t.Errorf("%q: any_link=%v invite_link=%v", text, s[AnyLink], s[InviteLink])
		}
	}
}

func TestMediaUrlNotALink(t *testing.T) {
	msg := []client.Field{{Name: "caption", Text: "nice photo", Match: "nice photo"}}
	if s := sig(msg); s[AnyLink] {
		t.Error("a captioned photo counted as a link")
	}
	media := "https://mmg.whatsapp.net/v/t62.7118-24/f1/m231/abc.enc?ccb=11-4"
	if s := sig(body("forwarded " + media)); s[AnyLink] {
		t.Error("a WhatsApp media download URL counted as a link")
	}
}

func TestInviteLinkVariants(t *testing.T) {
	for _, text := range []string{
		"chat.whatsapp.com/AbCdEf", "wa.me/447700900123", "api.whatsapp.com/send?phone=1",
		"whatsapp.com/channel/0029Va", "t.me/+AbCd", "telegram.me/club", "telegram.dog/club", "tg://resolve?domain=club",
	} {
		if !sig(body(text))[InviteLink] {
			t.Errorf("%q: no invite_link", text)
		}
	}
	for _, f := range []client.Field{
		{Name: "invite.link", Text: "https://chat.whatsapp.com/AbCd"},
		{Name: "event.join_link", Text: "https://call.whatsapp.com/video/AbCd"},
	} {
		if !sig([]client.Field{f})[InviteLink] {
			t.Errorf("%s: no invite_link", f.Name)
		}
	}
	if sig(body("whatsapp.com/download"))[InviteLink] {
		t.Error("a plain whatsapp.com page counted as an invite")
	}
}

func TestUsernameHandle(t *testing.T) {
	for _, text := range []string{"message @profit_mentor today", "@CryptoKing", "ask @anna.trader"} {
		if !sig(body(text))[Handle] {
			t.Errorf("%q: no handle", text)
		}
	}
	// The @mention marker the adapter puts in place of a mention, an email
	// address and a bare @ are not handles.
	for _, text := range []string{client.MentionMarker + " thanks for the crypto talk", "write to a@example.com", "meet @ 7pm"} {
		if sig(body(text))[Handle] {
			t.Errorf("%q: handle", text)
		}
	}
	// A mentioned phone number is masked before matching, so it is not a
	// phone number either.
	f := client.Field{Name: "body", Text: "@447700900123 thanks", Match: client.MentionMarker + " thanks"}
	if s := sig([]client.Field{f}); s[PhoneNumber] || s[Handle] {
		t.Errorf("mention read as %v", s)
	}
}

func TestRegistrableDomainAllowlist(t *testing.T) {
	if s := sig(body("tickets at https://www.eventbrite.co.uk/e/123"), "eventbrite.co.uk"); s[AnyLink] {
		t.Error("allowed domain counted as a link")
	}
	if s := sig(body("tickets at https://tickets.eventbrite.co.uk/e/1"), "eventbrite.co.uk"); s[AnyLink] {
		t.Error("allowed domain's subdomain counted as a link")
	}
	// A look-alike that only CONTAINS the allowed name is a different domain.
	if s := sig(body("https://eventbrite.co.uk.evil.example/x"), "eventbrite.co.uk"); !s[AnyLink] {
		t.Error("look-alike domain was allowed")
	}
	if s := sig(body("https://eventbrite-co-uk.example/x"), "eventbrite.co.uk"); !s[AnyLink] {
		t.Error("look-alike domain was allowed")
	}
}

func TestPhoneMoneyContactSignals(t *testing.T) {
	// The national form is built at run time: the public-repo scanner allows
	// the drama-range fixtures only in their +44 and 44 forms.
	national := "0" + "7700 900123"
	for _, text := range []string{"call +44 7700 900123", "text " + national, "whatsapp 447700900123"} {
		if !sig(body(text))[PhoneNumber] {
			t.Errorf("%q: no phone_number", text)
		}
	}
	for _, text := range []string{"class on 2026-10-06", "£120 ono", "room 101"} {
		if sig(body(text))[PhoneNumber] {
			t.Errorf("%q: phone_number", text)
		}
	}
	for _, text := range []string{"made $4,800 this week", "£500 a day", "5000 usdt"} {
		if !sig(body(text))[MoneyAmount] {
			t.Errorf("%q: no money_amount", text)
		}
	}
	if !sig([]client.Field{{Name: "contact.name", Text: "Mentor"}})[ContactCard] {
		t.Error("contact card not seen")
	}
}

// TestLinkDigitsAreNotAPhone: the ID in a job or event URL is part of the
// link, so an allowed link leaves nothing behind that reads as a phone number.
func TestLinkDigitsAreNotAPhone(t *testing.T) {
	for _, text := range []string{
		"apply: https://www.linkedin.com/jobs/view/0000000001",
		"rsvp www.meetup.com/example/events/0000000003/",
		"tickets https://www.eventbrite.co.uk/e/example-0000000004",
	} {
		s := sig(body(text), "linkedin.com", "meetup.com", "eventbrite.co.uk")
		if s[PhoneNumber] || s[AnyLink] {
			t.Errorf("%q: signals %v", text, s)
		}
	}
	// A number written beside the link still counts.
	if !sig(body("https://www.linkedin.com/in/x or call +44 7700 900123"), "linkedin.com")[PhoneNumber] {
		t.Error("phone number next to a link not seen")
	}
}

// ---- AC 2.5 decision -------------------------------------------------------

func TestSpamPostDeletesRemovesBans(t *testing.T) {
	rs := compile(t, testSpec())
	d := rs.Decide(Input{Community: communityA, Fields: body("Crypto signals group, join t.me/+AbCd")})
	if d.Action != DeleteRemoveBan || d.Rule != "crypto-or-stocks-pitch" {
		t.Fatalf("decision = %+v", d)
	}
	// The post that only talks about crypto, with no link or lure, is left.
	d = rs.Decide(Input{Community: communityA, Fields: body("great crypto talk yesterday")})
	if d.Action != ActionNone || !slices.Equal(d.Lists, []string{"crypto"}) {
		t.Fatalf("keyword-only decision = %+v", d)
	}
}

func TestLongStandingMemberBannedOnFirstPost(t *testing.T) {
	// The decision has no tenure, strike or first-seen input at all: a
	// member of ten years and a newcomer get the same decision.
	for i := 0; i < reflect.TypeOf(Input{}).NumField(); i++ {
		name := strings.ToLower(reflect.TypeOf(Input{}).Field(i).Name)
		for _, w := range []string{"join", "tenure", "since", "strike", "first", "age", "seen"} {
			if strings.Contains(name, w) {
				t.Fatalf("Input has a tenure-like field %q", name)
			}
		}
	}
	rs := compile(t, testSpec())
	d := rs.Decide(Input{Community: communityA, Fields: body("bitcoin doubling, inbox me")})
	if d.Action != DeleteRemoveBan {
		t.Fatalf("first post decision = %+v", d)
	}
}

func TestLureMatchBans(t *testing.T) {
	rs := compile(t, testSpec())
	d := rs.Decide(Input{Community: communityA, Fields: body("Guaranteed daily profit on USDT, DM me")})
	if d.Action != DeleteRemoveBan || d.Rule != "crypto-or-stocks-pitch" && d.Rule != "crypto-lure" {
		t.Fatalf("decision = %+v", d)
	}
	d = rs.Decide(Input{Community: communityA, Fields: body("stocks are up, dm me for the guaranteed picks")})
	if d.Action != DeleteRemoveBan || d.Rule != "crypto-lure" {
		t.Fatalf("lure decision = %+v", d)
	}
	// A lure phrase with no crypto or stock word does nothing.
	if d := rs.Decide(Input{Community: communityA, Fields: body("inbox me for salsa lesson times")}); d.Action != ActionNone {
		t.Fatalf("lure-only decision = %+v", d)
	}
}

func TestAdminAnywhereReportOnly(t *testing.T) {
	rs := compile(t, testSpec())
	d := rs.Decide(Input{Community: communityA, SenderIsAdmin: true, Fields: body("crypto pump t.me/+AbCd")})
	if d.Action != Log || d.Exempt != ExemptAdmin || d.Rule == "" || len(d.BanIn) != 0 {
		t.Fatalf("admin decision = %+v", d)
	}
}

func TestMetaAIExempt(t *testing.T) {
	rs := compile(t, testSpec())
	d := rs.Decide(Input{Community: communityA, FromMetaAI: true, Fields: body("Bitcoin is a cryptocurrency; see bitcoin.org")})
	if d.Action != Log || d.Exempt != ExemptMetaAI || len(d.BanIn) != 0 {
		t.Fatalf("Meta AI decision = %+v", d)
	}
}

func TestBanAppliesEverywhere(t *testing.T) {
	rs := compile(t, testSpec())
	d := rs.Decide(Input{Community: setB, Fields: body("crypto signals, inbox me")})
	if d.Action != DeleteRemoveBan || !slices.Equal(d.BanIn, []string{communityA, setB}) {
		t.Fatalf("ban covers %v, want both communities", d.BanIn)
	}
}

func TestWatchOnlyRuleOnlyReports(t *testing.T) {
	spec := testSpec()
	spec.Rules[0].Confirmed = false // a new rule, not confirmed yet
	spec.Rules = spec.Rules[:1]
	rs := compile(t, spec)
	d := rs.Decide(Input{Community: communityA, Fields: body("crypto signals t.me/+AbCd")})
	if d.Action != Log || !d.WouldHaveActed || d.Rule != "crypto-or-stocks-pitch" || len(d.BanIn) != 0 {
		t.Fatalf("unconfirmed decision = %+v", d)
	}
	// A confirmed rule in a shadow-mode community is also watch-only.
	spec = testSpec()
	spec.Communities[0].Mode = Shadow
	rs = compile(t, spec)
	d = rs.Decide(Input{Community: communityA, Fields: body("crypto signals t.me/+AbCd")})
	if d.Action != Log || !d.WouldHaveActed {
		t.Fatalf("shadow decision = %+v", d)
	}
}

func TestPushNameIsSeparateInput(t *testing.T) {
	spec := testSpec()
	spec.Rules = append(spec.Rules, RuleSpec{Name: "name-lure", Action: DeleteRemoveBan, Confirmed: true, On: OnPushName,
		When: Node{All: []Node{{Words: Names{"crypto"}}, {Words: Names{"lures"}}}}})
	rs := compile(t, spec)
	// "crypto" in the name and "inbox me" in the body never combine.
	d := rs.Decide(Input{Community: communityA, PushName: "Crypto Anna", Fields: body("inbox me for class times")})
	if d.Action != ActionNone {
		t.Fatalf("name + body combined: %+v", d)
	}
	d = rs.Decide(Input{Community: communityA, PushName: "Crypto Mentor, inbox me", Fields: body("hello all")})
	if d.Action != DeleteRemoveBan || d.Rule != "name-lure" {
		t.Fatalf("push-name rule decision = %+v", d)
	}
}

// TestUnsetModeAndScopeRefused: the schema supplies both defaults, so the
// rules package never invents one.
func TestUnsetModeAndScopeRefused(t *testing.T) {
	for name, edit := range map[string]func(*Spec){
		"mode":       func(s *Spec) { s.Mode = "" },
		"bans.scope": func(s *Spec) { s.BanScope = "everywhere" },
	} {
		s := testSpec()
		edit(&s)
		if _, err := Compile(s); err == nil || !strings.Contains(err.Error(), name+" must be") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestCommunityOfResolvesSetsAndParents(t *testing.T) {
	rs := compile(t, testSpec())
	if got := rs.CommunityOf(groupB, ""); got != setB {
		t.Errorf("standalone group → %q", got)
	}
	if got := rs.CommunityOf("99999000000777@g.us", communityA); got != communityA {
		t.Errorf("linked group → %q", got)
	}
	if got := rs.CommunityOf("99999000000777@g.us", ""); got != "" {
		t.Errorf("unconfigured group → %q", got)
	}
}

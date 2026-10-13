package rules

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/normalise"
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
	// An "@" in a link's path, query or fragment, or a link the URL parser
	// refuses (a bad %-escape or port), is still a link: never invisible to a
	// rule. Without "//" the matcher keeps a "?" or "#" tail only after a
	// scheme like "bitcoin:".
	for _, text := range []string{
		"cheap crypto signals youtube.com/@cryptoguru", "tiktok.com/@spammer/video/123", "example.org/join?u=a@b",
		"pay bitcoin:bc1qexampleaddr?label=a@b", "pay bitcoin:bc1qexampleaddr#a@b",
		"https://promo.example/get-100%free-bonus", "promo.example/get-100%free-bonus", "http://example.com:abc/x",
	} {
		if !sig(body(text))[AnyLink] {
			t.Errorf("%q: no any_link", text)
		}
	}
	// The host of a refused link still decides allowed and shortener: read
	// past a user name, up to the port or path.
	if s := sig(body("https://www.eventbrite.co.uk/e/100%off"), "eventbrite.co.uk"); s[AnyLink] {
		t.Error("a refused link on an allowed domain counted as a link")
	}
	// The user name is joined at run time: written whole it reads as an email.
	for _, text := range []string{"bit.ly/100%x", "https://promo" + "@" + "bit.ly/100%x", "http://bit.ly:abc/x"} {
		if s := sig(body(text)); !s[AnyLink] || !s[Shortener] {
			t.Errorf("%q: a refused shortener link: %v", text, s)
		}
	}
	if _, ok := RegistrableDomain("someone@example.com"); ok {
		t.Error("an email address accepted as an allowed domain")
	}
}

// TestCryptoLinkRuleMatchesRefusedLinks: the shipped "keyword + any_link"
// shape acts on a link with an "@" path or a refused %-escape.
func TestCryptoLinkRuleMatchesRefusedLinks(t *testing.T) {
	rs := compile(t, testSpec())
	for _, text := range []string{"cheap crypto signals youtube.com/@cryptoguru", "cheap crypto signals https://promo.example/get-100%free-bonus"} {
		if d := rs.Decide(Input{Community: communityA, Fields: body(text)}); d.Action != DeleteRemoveBan {
			t.Errorf("%q: decision %+v", text, d)
		}
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
	// A mention ending a sentence is still a mention, whatever follows it.
	for _, after := range []string{".", "..", ". See you", ".See you", ",", "!", "?", ")"} {
		text := "great crypto talk, thanks " + client.MentionMarker + after
		if sig(body(text))[Handle] {
			t.Errorf("%q: handle", text)
		}
	}
	// A real handle ending a sentence is still a handle, and so is one that
	// merely starts with the marker's letters.
	for _, text := range []string{"message @profit_mentor.", "ask @anna.trader.", "ask @mention_trader for signals"} {
		if !sig(body(text))[Handle] {
			t.Errorf("%q: no handle", text)
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

// TestSentenceBreakIsNotALink: a missing space after a full stop ("again.Is",
// "5pm.So"), a file name ("notes.zip") or a share code ("SAP.DE") is not a
// link, so a keyword beside it never acts. A link written with a scheme,
// "www.", a path, a port or a query still is one, and so is an invite host or
// shortener (or a name under one), and a bare name on a two-part ending or
// one of bareEndings.
func TestSentenceBreakIsNotALink(t *testing.T) {
	for _, text := range []string{
		"Bitcoin is down again.Is it time to sell?", "I bought some stocks last year.It was a mistake",
		"The crypto talk was boring.So was the food", "Lost money on bitcoin.No more for me",
		"crypto is a scam.My advice", "crypto again.To be honest", "stocks are down.Now is not the time",
		"bitcoin chat.Today we dance", "bitcoin bores me.Love you all", "see crypto-notes.md and stocks.zip",
		"Bitcoin is down again.How do I sell", "crypto crashed again.You should sell", "bitcoin crashed.One more time",
		"I sold my stocks in 2025.So no more", "crypto again.lol",
		// The fix review's cases: a word with a digit, a hyphen or more parts
		// before the break, and endings that are other words.
		"Bitcoin pumped at 5pm.So crazy", "Crypto is banned in the U.S.So what", "Bitcoin hit 69k.Is it the top",
		"Bitcoin passed 100k.So happy", "Tesla Q3.So far so good on stocks", "web3.Is it worth it? crypto",
		"Bitcoin is a no-brainer.So buy", "ETH 2.0.So what", "Bitcoin came 1st.So happy",
		"Bitcoin crashed again.Buy the dip", "crypto is dead.The end", "Bitcoin is boring.Tech stocks are better",
		"crypto is dead.AI is next", "Crypto talk tonight.Info: room 5", "Crypto meetup moved.Club opens at 9",
		"Bought 0700.HK, SAP.DE and BHP.AX stocks today",
		// "s.id" is a shortener, but only its own host counts.
		"Crypto is banned in the U.S.Id say", "Stocks in the U.S.Ee",
		// A bare name on any other ending reads the same as a missing space.
		"bitcoin giveaway at elondrop.live", "crypto signals at earnbig.me", "crypto-profit.in for stocks",
	} {
		if s := sig(body(text)); s[AnyLink] {
			t.Errorf("%q: any_link", text)
		}
	}
	// A file can be named like a link on a listed ending ("setup.com").
	for _, name := range []string{"bitcoin-talk-recording.mov", "crypto-setup.com"} {
		if s := sig([]client.Field{{Name: "document.file_name", Text: name}}); s[AnyLink] {
			t.Errorf("file name %q read as a link: %v", name, s)
		}
	}
	for _, text := range []string{
		"https://again.is", "www.again.is", "again.is/join", "again.is?ref=1", "again.is:8080", "t.me", "wa.me", "bit.ly",
		"visit cryptoprofits.com", "join cryptoprofits.io", "visit earnbig.top", "cryptoprofits.xyz",
		"join cryptopumps.t.me", "x.telegram.me", "earn.wa.me", "promo.co.in", "deals.com.my",
	} {
		if !sig(body(text))[AnyLink] {
			t.Errorf("%q: no any_link", text)
		}
	}
	if !sig([]client.Field{{Name: "document.file_name", Text: "join t.me/example_pumps.pdf"}})[AnyLink] {
		t.Error("a link with a path in a file name not seen")
	}
}

// TestDatesAndDecimalsAreNotPhones: a date (with or without a time) and a
// decimal that starts "0." are not phone numbers, so a keyword beside them
// never acts. A number that goes on past a date-like start is still a phone.
func TestDatesAndDecimalsAreNotPhones(t *testing.T) {
	for _, text := range []string{
		"Free stock market talk at the town library on 07.11.2025 18:00, all welcome",
		"Stock market evening class starts 07-11-2025 18:00 at the college",
		"Sat 01.11.25 7.30pm stock market talk", "Bitcoin meetup 01.12.25 19.30 upstairs",
		"After the crash I only have 0.00012345 BTC left lol", "I hold 0.00050000 BTC", "ETH is up +0.12345678% today",
		"Stock market talk 18:00 07.11.2025 at the library",
		// Ranges and times around dates (the fix review's cases).
		"Stocks webinar 01.12.2025 - 05.12.2025 18:00", "Crypto bootcamp 07.11.2025 09.30 - 08.11.2025 17.00",
		"Stocks class 01.12.25 - 05.12.25 18.30", "Crypto course 01-12-2025 - 05-12-2025 18:00",
		"Crypto breakfast 08.30 07.11.2025", "Crypto meetup 01.11 - 07.11.2025", "Crypto session 02 - 07.11.2025 18:00",
	} {
		if s := sig(body(text)); s[PhoneNumber] {
			t.Errorf("%q: phone_number", text)
		}
	}
	// Built at run time, as in TestPhoneMoneyContactSignals.
	national := "0" + "7700 900123"
	// A number with digits written like a date keeps the rest of its
	// digits: "+999 803" before "12.11.23", "789" after "05.34.56".
	plus := "+" + "999 "
	for _, text := range []string{
		"call " + national + " on 07.11.2025 18:00", "ring 0" + "7.70.09.00.12.3", "+44 7700 900123 for 0.5 BTC",
		"call " + plus + "470 12.34.56", "call " + plus + "803 12.11.23", "call " + plus + "171 12-34-56",
		"call 0" + "5.34.56 789",
		// Digits or a time before the number, and a first group that is not
		// the number's ("Room 4").
		"Bitcoin signals 24/7 " + plus + "470 12.11.23", "open 9:00-17:00 " + plus + "470 12.11.23",
		"Contacts: 1) " + plus + "470 12.11.23", "Room 4 0" + "999 12.11.23", "Room 4 " + national,
		// A "+" is always a number; pairs going on past a date-like start.
		"call +" + "99 12.11.2023", "call 0" + "6.12.99.99.99",
	} {
		if !sig(body(text))[PhoneNumber] {
			t.Errorf("%q: no phone_number", text)
		}
	}
}

// TestManyLinksCountAsALink: a field with more links or email addresses than
// are searched counts as having a link, and the text after them is still
// read, so padding cannot hide an invite or a phone number.
func TestManyLinksCountAsALink(t *testing.T) {
	pad := strings.Repeat("a@example.com ", maxLinks)
	if s := sig(body("bitcoin " + pad + "join chat.whatsapp.com/AbCdEf")); !s[AnyLink] || !s[InviteLink] {
		t.Errorf("invite after %d email addresses not counted: %v", maxLinks, s)
	}
	if s := sig(body("bitcoin " + pad + pad + "join t.me/example_pumps")); !s[InviteLink] {
		t.Errorf("invite after %d email addresses not counted: %v", 2*maxLinks, s)
	}
	if s := sig(body("bitcoin " + pad + "call +44 7700 900123")); !s[PhoneNumber] {
		t.Errorf("phone number after %d email addresses not seen: %v", maxLinks, s)
	}
	if s := sig(body("bitcoin " + pad + "talk later")); s[AnyLink] {
		t.Errorf("%d email addresses counted as a link: %v", maxLinks, s)
	}
	// The text after the classified links is searched for every invite host.
	for h := range inviteHosts {
		if !inviteRe.MatchString("see " + h + " now") {
			t.Errorf("inviteRe misses %s", h)
		}
	}
}

// TestLinkSpansInvalidUTF8: raw text that is not UTF-8 (corpus add reads it
// before any normalising) is searched without a panic.
func TestLinkSpansInvalidUTF8(t *testing.T) {
	s := "a" + strings.Repeat("\x80", 3*linkWindow) + " t.me/x"
	if got, _ := LinkSpans(s, false, -1); len(got) != 1 {
		t.Errorf("got %v, want the one link", got)
	}
}

// TestLinkSpansMatchWholeTextSearch: searching in windows finds the same
// links as one search over the whole text, wherever a link falls against a
// window's edge, including each punctuation mark in it as a window's last
// byte (the matcher never ends a link on one, so "https://chat" was all it
// saw) and a link longer than a window; and a window's end inside plain text
// makes no link up ("dead.th" of "dead.The end").
func TestLinkSpansMatchWholeTextSearch(t *testing.T) {
	whole := func(text string) (want [][2]int) {
		for _, m := range linkRe.FindAllStringIndex(text, -1) {
			if !notALink(text[m[0]:m[1]], text[m[1]:], false) {
				want = append(want, [2]int{m[0], m[1]})
			}
		}
		return want
	}
	pad := func(n int) string { return strings.Repeat("word ", n/5) + strings.Repeat("x", n%5) }
	links := []string{"t.me/example_pumps", "https://example.com/" + strings.Repeat("a", 300), "bit.ly/x", "cryptoprofits.com",
		"https://chat.whatsapp.com/AbCdEf", "https://t.me/+AbCd", "https://bit.ly/x", "https://www.google.com/maps/place/x",
		"https://example.com/a!!!!b", "https://example.com/" + strings.Repeat("b", 2400), "cryptoprofits.com🚀"}
	for _, l := range links {
		// The link starts at byte at+1.
		ats := []int{0, linkWindow - 300, linkWindow - len(l)/2, linkWindow - 1, linkWindow, 2*linkWindow - linkOverlap - 3, 3 * linkWindow}
		for i, c := range l {
			// After every byte of a short link (an emoji right after one is
			// not part of it), and after each punctuation mark and inside the
			// last part of a long one (nor is the comma after it).
			if (len(l) < 40 || strings.ContainsRune(".,;!?()", c) || i >= len(l)-3) && i < linkWindow-2 {
				ats = append(ats, linkWindow-2-i)
			}
		}
		for _, at := range ats {
			if at < 0 {
				continue
			}
			text := pad(at) + " " + l + ", again.Is " + strings.Repeat("more words ", 30)
			want := whole(text)
			got, _ := LinkSpans(text, false, -1)
			if !reflect.DeepEqual(got, want) || len(got) != 1 {
				t.Errorf("link %.20q at %d: got %v, want %v", l, at, got, want)
			}
		}
	}
	for _, sentence := range []string{"crypto is dead.The end", "Bitcoin crashed again.Company news", "stocks at 5pm.So crazy"} {
		for k := 1; k <= len(sentence); k++ {
			text := pad(linkWindow-k) + sentence + " " + strings.Repeat("more words ", 30)
			if got := func() [][2]int { g, _ := LinkSpans(text, false, -1); return g }(); !reflect.DeepEqual(got, whole(text)) || len(got) != 0 {
				t.Errorf("%q with the window's end %d bytes in: got %v", sentence, k, got)
			}
		}
	}
}

// TestLongLinkRunStaysFast: one long run with no spaces holding many short
// links ("1.1.1.1…") is searched in bounded windows, so finding the signals
// of a 64,000-character post costs about as much as reading it (Prepare).
// Measured: reading 84 ms, finding the signals 164 ms; one search over the
// whole post per link took over ten minutes.
func TestLongLinkRunStaysFast(t *testing.T) {
	text := "bitcoin " + strings.Repeat("1.", 32000)
	start := time.Now()
	normalise.Prepare(text, nil)
	read := time.Since(start)
	done := make(chan signals, 1)
	start = time.Now()
	go func() { done <- detect(body(text), nil) }()
	select {
	case s := <-done:
		took := time.Since(start)
		if !s[AnyLink] {
			t.Errorf("the run's links not seen: %v", s)
		}
		if took > 10*read {
			t.Fatalf("finding the signals of a %d-character post took %v, over ten times reading it (%v)", len(text), took, read)
		}
		t.Logf("read a %d-character post in %v, found its signals in %v", len(text), read, took)
	case <-time.After(time.Minute):
		t.Fatalf("finding the signals of a %d-character post took over a minute", len(text))
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
	// Punctuation with no space between them still makes two words.
	for _, text := range []string{"crypto,dm me", "bitcoin/guaranteed"} {
		if d := rs.Decide(Input{Community: communityA, Fields: body(text)}); d.Action != DeleteRemoveBan || d.Rule != "crypto-lure" {
			t.Fatalf("%q decision = %+v", text, d)
		}
	}
	// A letter the look-alike reading turns into punctuation separates words
	// there too, and letters merged across an invisible character read as
	// the whole word.
	ko := testSpec()
	ko.WordLists["crypto"] = append(ko.WordLists["crypto"], "\ube44\ud2b8\ucf54\uc778")
	rk := compile(t, ko)
	for _, text := range []string{"crypto\u01c3dm me", "\ube44\ud2b8\u110f\u00ad\u1169\uc778 dm me"} {
		if d := rk.Decide(Input{Community: communityA, Fields: body(text)}); d.Action != DeleteRemoveBan || d.Rule != "crypto-lure" {
			t.Fatalf("%+q decision = %+v", text, d)
		}
	}
	// A "*" in each list reaches only as far as a break between words that
	// any reading shows, so a lure and a crypto word are two words when the
	// look-alike reading joins them ("|" reads as "l"), when the plain reading
	// does (U+01C3 is a letter there), and when each list reads its word only
	// in the joined reading (Cyrillic letters). With no break it is one word.
	wild := testSpec()
	wild.WordLists["lures"] = append(wild.WordLists["lures"], "daily profit*")
	rw := compile(t, wild)
	for _, text := range []string{"Daily Profit|Bitcoin Mining", "daily profit\u01c3bitcoin", "daily prof\u0456t|bitco\u0456n"} {
		if d := rw.Decide(Input{Community: communityA, Fields: body(text)}); d.Action != DeleteRemoveBan || d.Rule != "crypto-lure" {
			t.Fatalf("%+q decision = %+v", text, d)
		}
	}
	if d := rw.Decide(Input{Community: communityA, Fields: body("daily profitxbitcoin")}); d.Action != ActionNone {
		t.Fatalf("one word decision = %+v", d)
	}
	// A lure phrase with no crypto or stock word does nothing.
	if d := rs.Decide(Input{Community: communityA, Fields: body("inbox me for salsa lesson times")}); d.Action != ActionNone {
		t.Fatalf("lure-only decision = %+v", d)
	}
}

// TestOneKeywordNeverActsAlone: a lure needs keywords from two word lists in
// different words of the post. Each case is a way one keyword can satisfy
// both lists of a lure; in each, a second, different lure word in the post
// makes it act again. Every case is run with the keyword written every way
// the matcher reads (capitals, look-alike letters, digit swaps) and with each
// list opted into digit swaps, because two lists can match one word in two
// different readings.
func TestOneKeywordNeverActsAlone(t *testing.T) {
	addLure := func(entry string) func(*Spec) {
		return func(s *Spec) { s.WordLists["lures"] = append(s.WordLists["lures"], entry) }
	}
	spellings := []struct {
		name   string
		spell  func(string) string
		always bool // the crypto list reads it without digit swaps
	}{
		{"as written", func(w string) string { return w }, true},
		{"in capitals", strings.ToUpper, true},
		{"with look-alike letters", func(w string) string { return strings.ReplaceAll(w, "o", "о") }, true},
		{"with digit swaps", strings.NewReplacer("i", "1", "o", "0").Replace, false},
		{"with one digit swap", func(w string) string { return strings.Replace(w, "i", "1", 1) }, false},
		{"spaced out", func(w string) string { return strings.Join(strings.Split(w, ""), " ") }, true},
	}
	leets := map[string][]string{"no list": nil, "lures": {"lures"}, "crypto and lures": {"crypto", "lures"}}
	for name, tc := range map[string]struct {
		edit         func(*Spec)
		word         string
		alone, again Input
	}{
		"the same word in both lists": {edit: addLure("bitcoin"), word: "bitcoin",
			alone: Input{Fields: body("anyone still holding bitcoin?")},
			again: Input{Fields: body("anyone still holding bitcoin? guaranteed")}},
		"a community's own lure word": {edit: func(s *Spec) { s.Communities[0].WordLists = map[string][]string{"lures": {"bitcoin"}} }, word: "bitcoin",
			alone: Input{Fields: body("anyone still holding bitcoin?")},
			again: Input{Fields: body("anyone still holding bitcoin? guaranteed")}},
		"a wildcard of each list": {edit: addLure("doge*"), word: "dogecoin", // crypto has "*coin"
			alone: Input{Fields: body("anyone still holding dogecoin?")},
			again: Input{Fields: body("anyone still holding dogecoin? dm me")}},
		"a lure phrase holding the crypto word": {edit: addLure("bitcoin giveaway"), word: "bitcoin",
			alone: Input{Fields: body("bitcoin giveaway tonight")},
			again: Input{Fields: body("bitcoin giveaway tonight, guaranteed")}},
		"the push name": {edit: func(s *Spec) {
			addLure("bitcoin")(s)
			s.Rules = append(s.Rules, RuleSpec{Name: "name-lure", Action: DeleteRemoveBan, Confirmed: true, On: OnPushName,
				When: Node{All: []Node{{Words: Names{"crypto"}}, {Words: Names{"lures"}}}}})
		}, word: "Bitcoin",
			alone: Input{PushName: "Bitcoin", Fields: body("hello all")},
			again: Input{PushName: "Bitcoin Guaranteed", Fields: body("hello all")}},
	} {
		for leetName, leet := range leets {
			for _, sp := range spellings {
				t.Run(name+"/"+sp.name+"/digit swaps for "+leetName, func(t *testing.T) {
					spec := testSpec()
					tc.edit(&spec)
					spec.LeetWordLists = leet
					rs := compile(t, spec)
					word := sp.spell(tc.word)
					respell := func(in Input) Input {
						out := Input{Community: communityA, PushName: strings.ReplaceAll(in.PushName, tc.word, word)}
						for _, f := range in.Fields {
							out.Fields = append(out.Fields, body(strings.ReplaceAll(f.Text, tc.word, word))...)
						}
						return out
					}
					alone, again := respell(tc.alone), respell(tc.again)
					if d := rs.Decide(alone); d.Action != ActionNone {
						t.Fatalf("one keyword %q decision = %+v", word, d)
					}
					if acting, _ := rs.ActsOn(alone); len(acting) != 0 {
						t.Fatalf("corpus view of one keyword %q acts: %v", word, acting)
					}
					if sp.name == "as written" {
						if c, _ := rs.Closest(alone); len(c.Failed) != 1 || !strings.Contains(c.Failed[0], "different words") {
							t.Fatalf("closest = %+v, want the different-words reason", c)
						}
					}
					// The two-word control must act whenever the crypto list
					// reads this spelling (a link next to it is acted on). Two
					// spaces keep the link's letters out of a spaced-out word.
					read := rs.Decide(Input{Community: communityA, Fields: body(word + "  t.me/+AbCd")}).Action == DeleteRemoveBan
					if sp.always && !read {
						t.Fatalf("the crypto list does not read %q", word)
					}
					if d := rs.Decide(again); read && d.Action != DeleteRemoveBan {
						t.Fatalf("two lure words with %q decision = %+v", word, d)
					}
				})
			}
		}
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

// TestWatchOnlyMatchExempt: an admin or Meta AI is exempt from a watch-only
// match too (unconfirmed rule, or shadow mode): reported as exempt, never as a
// post the bot would have acted on.
func TestWatchOnlyMatchExempt(t *testing.T) {
	unconfirmed := testSpec()
	unconfirmed.Rules[0].Confirmed = false
	unconfirmed.Rules = unconfirmed.Rules[:1]
	shadow := testSpec()
	shadow.Communities[0].Mode = Shadow
	for _, tc := range []struct {
		name string
		spec Spec
		in   Input
		want Exemption
	}{
		{"meta ai, unconfirmed", unconfirmed, Input{FromMetaAI: true}, ExemptMetaAI},
		{"meta ai, shadow", shadow, Input{FromMetaAI: true}, ExemptMetaAI},
		{"admin, unconfirmed", unconfirmed, Input{SenderIsAdmin: true}, ExemptAdmin},
		{"admin, shadow", shadow, Input{SenderIsAdmin: true}, ExemptAdmin},
	} {
		rs := compile(t, tc.spec)
		tc.in.Community, tc.in.Fields = communityA, body("crypto signals t.me/+AbCd")
		d := rs.Decide(tc.in)
		if d.Action != Log || d.WouldHaveActed || d.Exempt != tc.want || d.Rule == "" || len(d.BanIn) != 0 {
			t.Errorf("%s: decision = %+v", tc.name, d)
		}
	}
	// Control: the same post from a member is one the bot would have acted on.
	if d := compile(t, shadow).Decide(Input{Community: communityA, Fields: body("crypto signals t.me/+AbCd")}); !d.WouldHaveActed || d.Exempt != "" {
		t.Fatalf("member decision = %+v", d)
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

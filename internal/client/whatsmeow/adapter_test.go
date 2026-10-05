package whatsmeow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wm "go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/pipeline"
	gwstore "github.com/hoiung/groupwarden/internal/store"
)

var ps = proto.String

func TestRevokeUsesSenderJID(t *testing.T) {
	f := &fakeWA{}
	a := newTestAdapter(t, f, nil)
	if err := a.Revoke(context.Background(), client.JID(groupJID.String()), client.JID(spammer.String()), "3EB0SPAM"); err != nil {
		t.Fatal(err)
	}
	if f.revokeChat != groupJID || f.revokeSender != spammer || f.revokeID != "3EB0SPAM" {
		t.Fatalf("BuildRevoke(%s, %s, %s), want the group, the SENDER and the message ID", f.revokeChat, f.revokeSender, f.revokeID)
	}
	if f.sentTo != groupJID || f.sent.GetProtocolMessage().GetKey().GetParticipant() != spammer.String() {
		t.Fatalf("revoke sent to %s for participant %q", f.sentTo, f.sent.GetProtocolMessage().GetKey().GetParticipant())
	}
}

func TestRemoveAlreadyGone(t *testing.T) {
	if CodeNotParticipant != 404 {
		t.Fatalf("CodeNotParticipant = %d; the live probe (AC 1.9 E8) pins it", CodeNotParticipant)
	}
	f := &fakeWA{participants: []types.GroupParticipant{
		{JID: spammer},
		{JID: member2, Error: CodeNotParticipant},
		{JID: member3, Error: 403},
	}}
	a := newTestAdapter(t, f, nil)
	missing := client.JID("99999000000888@lid")
	got, err := a.Remove(context.Background(), client.JID(groupJID.String()),
		[]client.JID{client.JID(spammer.String()), client.JID(member2.String()), client.JID(member3.String()), missing})
	if err != nil {
		t.Fatal(err)
	}
	want := []client.MemberResult{
		{Member: client.JID(spammer.String()), Status: client.MemberDone},
		{Member: client.JID(member2.String()), Status: client.MemberAlreadyGone, Code: 404},
		{Member: client.JID(member3.String()), Status: client.MemberFailed, Code: 403},
		{Member: missing, Status: client.MemberFailed, Code: -1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("results\n got %+v\nwant %+v", got, want)
	}
}

func TestExtractAllSenderFields(t *testing.T) {
	sink := &fakeSink{}
	a := newTestAdapter(t, &fakeWA{offline: true}, sink)
	cases := []struct {
		name string
		msg  *waE2E.Message
		want map[string][]string
	}{
		{"body", &waE2E.Message{Conversation: ps("plain body")}, map[string][]string{"body": {"plain body"}}},
		{"link preview", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: ps("see t.me/x"), Title: ps("Preview title"), Description: ps("Preview description"), MatchedText: ps("https://t.me/x")}},
			map[string][]string{"body": {"see t.me/x"}, "link.title": {"Preview title"}, "link.description": {"Preview description"}, "link.url": {"https://t.me/x"}}},
		{"image caption", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: ps("image caption")}},
			map[string][]string{"caption": {"image caption"}}},
		{"video caption", &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Caption: ps("video caption")}},
			map[string][]string{"caption": {"video caption"}}},
		{"document", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: ps("doc caption"), FileName: ps("profits.pdf"), Title: ps("Doc title")}},
			map[string][]string{"caption": {"doc caption"}, "document.file_name": {"profits.pdf"}, "document.title": {"Doc title"}}},
		{"poll", &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{Name: ps("Which coin?"),
			Options: []*waE2E.PollCreationMessage_Option{{OptionName: ps("Coin A")}, {OptionName: ps("Coin B")}}}},
			map[string][]string{"poll.question": {"Which coin?"}, "poll.option": {"Coin A", "Coin B"}}},
		{"contact card", &waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: ps("Trader Tom"),
			Vcard: ps("BEGIN:VCARD\nVERSION:3.0\nFN:Trader Tom\nitem1.TEL;waid=447700900456:+44 7700 900456\nTEL;TYPE=CELL:+44 7700 900789\nEND:VCARD")}},
			map[string][]string{"contact.name": {"Trader Tom"}, "contact.number": {"+44 7700 900456", "+44 7700 900789"}}},
		{"contacts array", &waE2E.Message{ContactsArrayMessage: &waE2E.ContactsArrayMessage{DisplayName: ps("2 contacts"),
			Contacts: []*waE2E.ContactMessage{{DisplayName: ps("Mentor")}}}},
			map[string][]string{"contact.name": {"2 contacts", "Mentor"}}},
		{"location", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{Name: ps("Office"), Address: ps("1 Street"), URL: ps("https://example.org/loc"), Comment: ps("come by")}},
			map[string][]string{"location.name": {"Office"}, "location.address": {"1 Street"}, "location.url": {"https://example.org/loc"}, "location.comment": {"come by"}}},
		{"event", &waE2E.Message{EventMessage: &waE2E.EventMessage{Name: ps("Webinar"), Description: ps("Learn trading"), JoinLink: ps("https://call.whatsapp.com/video/ABC")}},
			map[string][]string{"event.name": {"Webinar"}, "event.description": {"Learn trading"}, "event.join_link": {"https://call.whatsapp.com/video/ABC"}}},
		{"group invite", &waE2E.Message{GroupInviteMessage: &waE2E.GroupInviteMessage{GroupName: ps("VIP signals"), Caption: ps("join us"), InviteCode: ps("INVITECODE")}},
			map[string][]string{"invite.group_name": {"VIP signals"}, "invite.caption": {"join us"}, "invite.link": {"https://chat.whatsapp.com/INVITECODE"}}},
		{"list", &waE2E.Message{ListMessage: &waE2E.ListMessage{Title: ps("List title"), Description: ps("List desc"), ButtonText: ps("Open"), FooterText: ps("Footer"),
			Sections: []*waE2E.ListMessage_Section{{Title: ps("Section"), Rows: []*waE2E.ListMessage_Row{{Title: ps("Row"), Description: ps("Row desc")}}}}}},
			map[string][]string{"list.title": {"List title"}, "list.description": {"List desc"}, "list.button": {"Open"}, "list.footer": {"Footer"},
				"list.section": {"Section"}, "list.row": {"Row", "Row desc"}}},
		{"buttons", &waE2E.Message{ButtonsMessage: &waE2E.ButtonsMessage{ContentText: ps("Pick one"), FooterText: ps("btn footer"),
			Buttons: []*waE2E.ButtonsMessage_Button{{ButtonText: &waE2E.ButtonsMessage_Button_ButtonText{DisplayText: ps("Invest")}}}}},
			map[string][]string{"buttons.text": {"Pick one"}, "buttons.footer": {"btn footer"}, "buttons.button": {"Invest"}}},
	}
	for i, c := range cases {
		ev := convert(t, a, sink, groupMsg(fmt.Sprintf("M%d", i), c.msg))
		m, ok := ev.(*client.Message)
		if !ok {
			t.Fatalf("%s: persisted %T", c.name, ev)
		}
		if got := fieldsOf(m); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %v\nwant %v", c.name, got, c.want)
		}
		if m.Sender != client.JID(spammer.String()) || m.SenderAlt != client.JID(spammerPN.String()) || m.Chat != client.JID(groupJID.String()) {
			t.Errorf("%s: addressing %s / %s in %s", c.name, m.Sender, m.SenderAlt, m.Chat)
		}
	}
	// Nothing sender-written: a reaction, a delete, a DM, our own message.
	before := len(sink.events())
	for _, e := range []*events.Message{
		groupMsg("R", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: ps("👍")}}),
		groupMsg("D", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}}),
	} {
		if !a.handle(e) {
			t.Fatal("handler refused a non-content message")
		}
	}
	dm := groupMsg("DM", &waE2E.Message{Conversation: ps("hi")})
	dm.Info.IsGroup, dm.Info.Chat = false, spammerPN
	own := groupMsg("OWN", &waE2E.Message{Conversation: ps("hi")})
	own.Info.IsFromMe = true
	a.handle(dm)
	a.handle(own)
	if n := len(sink.events()) - before; n != 0 {
		t.Fatalf("%d non-content or non-group messages were persisted", n)
	}
}

func TestUnwrapWrappers(t *testing.T) {
	sink := &fakeSink{}
	a := newTestAdapter(t, &fakeWA{offline: true}, sink)
	doc := &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: ps("wrapped caption"), FileName: ps("a.pdf")}}
	fp := func(m *waE2E.Message) *waE2E.FutureProofMessage { return &waE2E.FutureProofMessage{Message: m} }
	wrapped := &waE2E.Message{EphemeralMessage: fp(&waE2E.Message{ViewOnceMessageV2: fp(&waE2E.Message{DocumentWithCaptionMessage: fp(doc)})})}
	cases := map[string]*waE2E.Message{
		"ephemeral+view-once+document-with-caption": wrapped,
		"view-once v2 extension":                    {ViewOnceMessageV2Extension: fp(doc)},
		"device-sent":                               {DeviceSentMessage: &waE2E.DeviceSentMessage{Message: doc}},
		"edited envelope":                           {EditedMessage: fp(doc)},
	}
	for name, msg := range cases {
		m := convert(t, a, sink, groupMsg(name, msg)).(*client.Message)
		got := fieldsOf(m)
		if !reflect.DeepEqual(got["caption"], []string{"wrapped caption"}) || !reflect.DeepEqual(got["document.file_name"], []string{"a.pdf"}) {
			t.Errorf("%s: fields %v", name, got)
		}
	}
	if got := unwrap(nestedEphemeral(20)); got.GetConversation() != "" {
		t.Fatal("unwrap followed a hostile envelope chain past its bound")
	}
}

func nestedEphemeral(depth int) *waE2E.Message {
	m := &waE2E.Message{Conversation: ps("deep")}
	for i := 0; i < depth; i++ {
		m = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: m}}
	}
	return m
}

func TestQuotedTextNotScanned(t *testing.T) {
	sink := &fakeSink{}
	a := newTestAdapter(t, &fakeWA{offline: true}, sink)
	quoted := &waE2E.Message{Conversation: ps("QUOTED crypto pitch wa.me/447700900123")}
	reply := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: ps("please stop posting this"), ContextInfo: &waE2E.ContextInfo{QuotedMessage: quoted, StanzaID: ps("SPAM1")}}}
	m := convert(t, a, sink, groupMsg("Q1", reply)).(*client.Message)
	if got := fieldsOf(m); !reflect.DeepEqual(got, map[string][]string{"body": {"please stop posting this"}}) {
		t.Fatalf("reply fields %v", got)
	}
	// An image reply quoting spam: the quote must not ride along with the attachment either.
	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: ps("look"), Mimetype: ps("image/jpeg"), FileLength: proto.Uint64(10),
		ContextInfo: &waE2E.ContextInfo{QuotedMessage: quoted}}}
	m = convert(t, a, sink, groupMsg("Q2", img)).(*client.Message)
	for _, f := range m.Fields {
		if strings.Contains(f.Text, "QUOTED") {
			t.Fatalf("quoted text extracted into %s", f.Name)
		}
	}
	if m.Media == nil || bytes.Contains(m.Media.Raw, []byte("QUOTED")) {
		t.Fatalf("attachment reference missing or carries the quoted text")
	}
}

func TestMentionIsNotAPhoneNumberOrHandle(t *testing.T) {
	sink := &fakeSink{}
	a := newTestAdapter(t, &fakeWA{offline: true}, sink)
	text := "@447700900456 and @99999000000555 thanks for the crypto talk"
	msg := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: ps(text),
		ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{"447700900456@s.whatsapp.net", "99999000000555@lid"}}}}
	m := convert(t, a, sink, groupMsg("MEN", msg)).(*client.Message)
	if len(m.Fields) != 1 {
		t.Fatalf("fields %+v", m.Fields)
	}
	f := m.Fields[0]
	if f.Text != text {
		t.Fatalf("original text changed: %q", f.Text)
	}
	if want := "@mention and @mention thanks for the crypto talk"; f.Match != want {
		t.Fatalf("match view %q, want %q", f.Match, want)
	}
	if len(m.Mentions) != 2 {
		t.Fatalf("mentions %v", m.Mentions)
	}
	// A number typed WITHOUT a mention stays in the match view.
	plain := convert(t, a, sink, groupMsg("NUM", &waE2E.Message{Conversation: ps("call +44 7700 900456")})).(*client.Message)
	if plain.Fields[0].Match != "call +44 7700 900456" {
		t.Fatalf("unmentioned number altered: %q", plain.Fields[0].Match)
	}
}

func TestPushNameSeparateInput(t *testing.T) {
	sink := &fakeSink{}
	a := newTestAdapter(t, &fakeWA{offline: true}, sink)
	e := groupMsg("PN", &waE2E.Message{Conversation: ps("hello all")})
	e.Info.PushName = "Crypto King Signals"
	m := convert(t, a, sink, e).(*client.Message)
	if m.PushName != "Crypto King Signals" {
		t.Fatalf("push name %q", m.PushName)
	}
	for _, f := range m.Fields {
		if strings.Contains(f.Text, "Crypto King") || strings.Contains(f.Match, "Crypto King") {
			t.Fatalf("push name leaked into field %s", f.Name)
		}
	}
}

func TestEditTargetsOriginalID(t *testing.T) {
	sink := &fakeSink{}
	a := newTestAdapter(t, &fakeWA{offline: true}, sink)
	edit := &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type:          waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
		Key:           &waCommon.MessageKey{ID: ps("ORIGINAL1")},
		EditedMessage: &waE2E.Message{Conversation: ps("now with a link t.me/x")},
	}}}}
	m := convert(t, a, sink, groupMsg("EDIT1", edit)).(*client.Message)
	if !m.IsEdit || m.TargetID != "ORIGINAL1" || m.ID != "EDIT1" {
		t.Fatalf("edit: IsEdit=%v TargetID=%q ID=%q", m.IsEdit, m.TargetID, m.ID)
	}
	if got := fieldsOf(m); !reflect.DeepEqual(got, map[string][]string{"body": {"now with a link t.me/x"}}) {
		t.Fatalf("edit fields %v", got)
	}
}

func TestCommentAndSecretEditDecrypt(t *testing.T) {
	sink := &fakeSink{}
	f := &fakeWA{offline: true,
		comment:    &waE2E.Message{Conversation: ps("reply under the announcement")},
		secretEdit: &waE2E.Message{Conversation: ps("secretly edited text")},
	}
	a := newTestAdapter(t, f, sink)
	c := convert(t, a, sink, groupMsg("C1", &waE2E.Message{EncCommentMessage: &waE2E.EncCommentMessage{
		TargetMessageKey: &waCommon.MessageKey{ID: ps("ANNOUNCE1")}, EncPayload: []byte{1}, EncIV: []byte{2}}})).(*client.Message)
	if !c.IsComment || c.TargetID != "C1" || fieldsOf(c)["body"][0] != "reply under the announcement" {
		t.Fatalf("comment: %+v", c)
	}
	s := convert(t, a, sink, groupMsg("S1", &waE2E.Message{SecretEncryptedMessage: &waE2E.SecretEncryptedMessage{
		SecretEncType: waE2E.SecretEncryptedMessage_MESSAGE_EDIT.Enum(), TargetMessageKey: &waCommon.MessageKey{ID: ps("ORIG2")}}})).(*client.Message)
	if !s.IsEdit || s.TargetID != "ORIG2" || fieldsOf(s)["body"][0] != "secretly edited text" {
		t.Fatalf("secret edit: %+v", s)
	}
	if f.calls[0] != "DecryptComment" || f.calls[1] != "DecryptSecretEncryptedMessage" {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestMissingParentSecretCountedNotAlerted(t *testing.T) {
	ctx := context.Background()
	st, err := gwstore.Open(ctx, filepath.Join(t.TempDir(), "g.db"), gwstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	inbox := pipeline.NewInbox(st)
	f := &fakeWA{offline: true, commentErr: fmt.Errorf("failed to decrypt comment: %w", wm.ErrOriginalMessageSecretNotFound)}
	a := newTestAdapter(t, f, sinkOver(inbox))
	comment := func(id string) *events.Message {
		return groupMsg(id, &waE2E.Message{EncCommentMessage: &waE2E.EncCommentMessage{TargetMessageKey: &waCommon.MessageKey{ID: ps("OLD")}}})
	}
	if !a.handle(comment("U1")) {
		t.Fatal("missing-secret reply not acknowledged")
	}
	f.commentErr = errors.New("gcm: message authentication failed")
	if !a.handle(comment("U2")) {
		t.Fatal("undecryptable reply not acknowledged")
	}
	rec := &alert.Recorder{}
	w := &pipeline.Worker{Store: st, Inbox: inbox, MaxReplayAge: 47 * time.Hour, Log: a.log,
		Decider: &pipeline.Moderator{Store: st, Alerter: rec, Log: a.log}}
	if err := w.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	n, err := st.Counter(ctx, time.Now().UTC().Format("2006-01-02"), pipeline.CounterMissingParent)
	if err != nil || n != 1 {
		t.Fatalf("missing-parent counter = %d (%v), want 1", n, err)
	}
	alerts := rec.All()
	if len(alerts) != 1 || alerts[0].Kind != alert.DecryptError || !alerts[0].Priority {
		t.Fatalf("alerts %+v, want exactly one priority decrypt_error (for U2 only)", alerts)
	}
}

// sinkOver adapts an inbox to client.Sink for tests.
func sinkOver(in *pipeline.Inbox) client.Sink { return inboxSink{in} }

type inboxSink struct{ in *pipeline.Inbox }

func (s inboxSink) Persist(ev client.Event) error { return s.in.Persist(ev) }
func (s inboxSink) Lifecycle(client.Lifecycle)    {}

func TestJoinReasonAndActor(t *testing.T) {
	sink := &fakeSink{}
	a := newTestAdapter(t, &fakeWA{offline: true}, sink)
	admin := types.NewJID("99999000000555", types.HiddenUserServer)
	adminPN := types.NewJID("447700900456", types.DefaultUserServer)
	at := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		evt       *events.GroupInfo
		actor     client.JID
		actorAlt  client.JID
		reason    string
		wantEvent bool
	}{
		{&events.GroupInfo{JID: groupJID, Timestamp: at, JoinReason: "invite", Join: []types.JID{spammer}}, "", "", "invite", true},
		{&events.GroupInfo{JID: groupJID, Timestamp: at, Sender: &admin, SenderPN: &adminPN, Join: []types.JID{spammer}},
			client.JID(admin.String()), client.JID(adminPN.String()), "", true},
		{&events.GroupInfo{JID: groupJID, Timestamp: at, Sender: &admin, Name: &types.GroupName{Name: "renamed"}}, "", "", "", false},
	}
	for i, c := range cases {
		before := len(sink.events())
		if !a.handle(c.evt) {
			t.Fatalf("case %d refused", i)
		}
		evs := sink.events()
		if !c.wantEvent {
			if len(evs) != before {
				t.Fatalf("case %d: a name change was persisted", i)
			}
			continue
		}
		g := evs[len(evs)-1].(*client.GroupChange)
		if g.Actor != c.actor || g.ActorAlt != c.actorAlt || g.JoinReason != c.reason || len(g.Joined) != 1 || g.Joined[0] != client.JID(spammer.String()) {
			t.Errorf("case %d: %+v", i, g)
		}
	}
}

func TestJoinPendingDerived(t *testing.T) {
	ctx := context.Background()
	self := types.GroupParticipant{JID: botLID, PhoneNumber: botPhone.ToNonAD()}
	other := types.GroupParticipant{JID: member2}
	cases := []struct {
		name        string
		info        *types.GroupInfo
		infoErr     error
		wantPending bool
		wantErr     bool
	}{
		{"joined", &types.GroupInfo{Participants: []types.GroupParticipant{other, self}}, nil, false, false},
		{"request only (not allowed to read)", nil, wm.ErrNotInGroup, true, false},
		{"request only (forbidden)", nil, wm.ErrIQForbidden, true, false},
		{"readable but not a member", &types.GroupInfo{Participants: []types.GroupParticipant{other}}, nil, true, false},
		{"lookup failed", nil, errors.New("timeout"), false, true},
	}
	for _, c := range cases {
		f := &fakeWA{joinResult: groupJID, info: c.info, infoErr: c.infoErr}
		g, pending, err := newTestAdapter(t, f, nil).JoinWithLink(ctx, "INVITECODE")
		if (err != nil) != c.wantErr || pending != c.wantPending {
			t.Errorf("%s: pending=%v err=%v", c.name, pending, err)
		}
		if !c.wantErr && g != client.JID(groupJID.String()) {
			t.Errorf("%s: group %s", c.name, g)
		}
	}
	// join_linked_group: a bare result is a join, an approval child is pending.
	f := &fakeWA{iqResp: &waBinary.Node{Tag: "iq"}}
	a := newTestAdapter(t, f, nil)
	joined, pending, err := a.JoinLinkedGroup(ctx, "99999000000333@g.us", client.JID(group2JID.String()))
	if err != nil || !joined || pending {
		t.Fatalf("bare result: joined=%v pending=%v err=%v", joined, pending, err)
	}
	if f.iqTo.String() != "99999000000333@g.us" || f.iqContent.Tag != "join_linked_group" || f.iqContent.Attrs["jid"] != group2JID {
		t.Fatalf("IQ to %s: %+v", f.iqTo, f.iqContent)
	}
	f.iqResp = &waBinary.Node{Tag: "iq", Content: []waBinary.Node{{Tag: "membership_approval_request"}}}
	joined, pending, err = a.JoinLinkedGroup(ctx, "99999000000333@g.us", client.JID(group2JID.String()))
	if err != nil || joined || !pending {
		t.Fatalf("approval child: joined=%v pending=%v err=%v", joined, pending, err)
	}
}

func TestDownloadMediaAttachment(t *testing.T) {
	sink := &fakeSink{}
	f := &fakeWA{}
	a := newTestAdapter(t, f, sink)
	cases := []struct {
		msg      *waE2E.Message
		kind     string
		fileName string
	}{
		{&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: ps("pic"), Mimetype: ps("image/jpeg"), FileLength: proto.Uint64(2048), DirectPath: ps("/v/img")}}, "image", ""},
		{&waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: ps("video/mp4"), FileLength: proto.Uint64(4096), DirectPath: ps("/v/vid")}}, "video", ""},
		{&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Mimetype: ps("application/pdf"), FileName: ps("plan.pdf"), FileLength: proto.Uint64(99), DirectPath: ps("/v/doc")}}, "document", "plan.pdf"},
		{&waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: ps("audio/ogg"), FileLength: proto.Uint64(77), DirectPath: ps("/v/aud")}}, "audio", ""},
	}
	for i, c := range cases {
		m := convert(t, a, sink, groupMsg(fmt.Sprintf("MED%d", i), c.msg)).(*client.Message)
		if m.Media == nil || m.Media.Kind != c.kind || m.Media.Size == 0 {
			t.Fatalf("%s: media %+v (size must be known before download)", c.kind, m.Media)
		}
		data, mime, name, err := a.DownloadMedia(context.Background(), m)
		if err != nil || string(data) != "file-bytes" || mime != m.Media.MimeType || name != c.fileName {
			t.Fatalf("%s: download %q %q %q %v", c.kind, data, mime, name, err)
		}
		dp, ok := f.downloaded.(interface{ GetDirectPath() string })
		if !ok || !strings.HasPrefix(dp.GetDirectPath(), "/v/") {
			t.Fatalf("%s: downloaded %T", c.kind, f.downloaded)
		}
	}
	if _, _, _, err := a.DownloadMedia(context.Background(), &client.Message{}); err == nil {
		t.Fatal("download of a message without an attachment succeeded")
	}
}

func TestHistorySyncDisabled(t *testing.T) {
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite", gwstore.DSN(filepath.Join(t.TempDir(), "whatsmeow.db")), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer container.Close()
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cli := wm.NewClient(device, nil)
	configure(cli)
	if !cli.ManualHistorySyncDownload {
		t.Fatal("history sync download is not disabled")
	}
	if !cli.SynchronousAck || !cli.EnableDecryptedEventBuffer || cli.EnableAutoReconnect {
		t.Fatalf("ack/buffer/reconnect settings: sync=%v buffer=%v autoreconnect=%v", cli.SynchronousAck, cli.EnableDecryptedEventBuffer, cli.EnableAutoReconnect)
	}
	sink := &fakeSink{}
	a := newTestAdapter(t, &fakeWA{offline: true}, sink)
	if !a.handle(&events.HistorySync{}) || len(sink.events()) != 0 {
		t.Fatal("a history sync blob was processed")
	}
}

func TestPermanentDisconnectClassification(t *testing.T) {
	cases := []struct {
		evt    events.PermanentDisconnect
		kind   client.LifecycleKind
		fatal  bool
		expiry time.Duration
	}{
		{&events.LoggedOut{Reason: events.ConnectFailureLoggedOut}, client.LoggedOut, true, 0},
		{&events.StreamReplaced{}, client.StreamReplaced, true, 0},
		{&events.ClientOutdated{}, client.ClientOutdated, true, 0},
		{&events.CATRefreshError{Error: errors.New("x")}, client.CATRefreshFailed, true, 0},
		{&events.ConnectFailure{Reason: events.ConnectFailureReason(418)}, client.ConnectFailure, true, 0},
		{&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: 2 * time.Hour}, client.TemporaryBan, false, 2 * time.Hour},
	}
	for _, c := range cases {
		l := classify(c.evt)
		if l.Kind != c.kind || l.Kind.Fatal() != c.fatal || l.Expiry != c.expiry || l.Detail == "" {
			t.Errorf("%T: %+v fatal=%v", c.evt, l, l.Kind.Fatal())
		}
		if c.fatal && l.Kind.NextStep() == "" {
			t.Errorf("%T: fatal kind without a next step", c.evt)
		}
	}
	// Through the handler: lifecycle only, never persisted.
	sink := &fakeSink{}
	a := newTestAdapter(t, &fakeWA{offline: true}, sink)
	a.handle(&events.StreamReplaced{})
	a.handle(&events.TemporaryBan{Expire: time.Hour})
	a.handle(&events.Disconnected{})
	a.handle(&events.Connected{})
	got := sink.lifecycles()
	want := []client.LifecycleKind{client.StreamReplaced, client.TemporaryBan, client.Disconnected, client.Connected}
	if len(got) != len(want) || len(sink.events()) != 0 {
		t.Fatalf("lifecycle %+v persisted %d", got, len(sink.events()))
	}
	for i := range want {
		if got[i].Kind != want[i] {
			t.Fatalf("lifecycle %d = %s, want %s", i, got[i].Kind, want[i])
		}
	}
}

func TestClientOutdatedTriesVersionUpdateOnce(t *testing.T) {
	sink := &fakeSink{}
	a := newTestAdapter(t, &fakeWA{offline: true}, sink)
	var tries atomic.Int32
	a.updateVersion = func(context.Context) error { tries.Add(1); return nil }
	waitFor := func(n int) []client.Lifecycle {
		deadline := time.Now().Add(5 * time.Second)
		for len(sink.lifecycles()) < n && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		return sink.lifecycles()
	}
	a.handle(&events.ClientOutdated{})
	if got := waitFor(1); len(got) != 1 || got[0].Kind != client.Disconnected {
		t.Fatalf("first refusal: %+v, want a reconnect after the update", got)
	}
	a.handle(&events.ClientOutdated{})
	if got := waitFor(2); len(got) != 2 || got[1].Kind != client.ClientOutdated || !got[1].Kind.Fatal() {
		t.Fatalf("second refusal: %+v, want fatal", got)
	}
	if n := tries.Load(); n != 1 {
		t.Fatalf("version update tried %d times, want once", n)
	}
	// A failed update is fatal straight away.
	sink2 := &fakeSink{}
	b := newTestAdapter(t, &fakeWA{offline: true}, sink2)
	b.handle(&events.ClientOutdated{})
	deadline := time.Now().Add(5 * time.Second)
	for len(sink2.lifecycles()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := sink2.lifecycles(); len(got) != 1 || got[0].Kind != client.ClientOutdated {
		t.Fatalf("failed update: %+v", got)
	}
}

func TestAckAfterInboxWrite(t *testing.T) {
	ctx := context.Background()
	st, err := gwstore.Open(ctx, filepath.Join(t.TempDir(), "g.db"), gwstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The library acknowledges after every handler returns true; at that
	// moment the event must already be committed to the inbox.
	var committedAtReturn int
	inbox := pipeline.NewInbox(st)
	a := newTestAdapter(t, &fakeWA{offline: true}, sinkOver(inbox))
	ack := a.handle(groupMsg("ACK1", &waE2E.Message{Conversation: ps("hello")}))
	committedAtReturn, _ = st.InboxLen(ctx)
	if !ack || committedAtReturn != 1 {
		t.Fatalf("handler returned %v with %d committed rows, want true after 1", ack, committedAtReturn)
	}
	// The real client is configured to send acks only after handlers succeed.
	cli := wm.NewClient(mustDevice(t), nil)
	configure(cli)
	if !cli.SynchronousAck || !cli.EnableDecryptedEventBuffer {
		t.Fatal("client acknowledges before the handler has run")
	}
}

func mustDevice(t *testing.T) *wmstore.Device {
	t.Helper()
	c, err := sqlstore.New(context.Background(), "sqlite", gwstore.DSN(filepath.Join(t.TempDir(), "w.db")), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	d, err := c.GetFirstDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestHandlerFailureHoldsAck(t *testing.T) {
	ctx := context.Background()
	sink := &fakeSink{persistErr: errors.New("disk full")}
	a := newTestAdapter(t, &fakeWA{offline: true}, sink)
	if a.handle(groupMsg("F1", &waE2E.Message{Conversation: ps("hello")})) {
		t.Fatal("handler acknowledged a message it failed to store")
	}
	// The same through a real store whose writes fail.
	st, err := gwstore.Open(ctx, filepath.Join(t.TempDir(), "g.db"), gwstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.Close() // a closed database fails every write
	b := newTestAdapter(t, &fakeWA{offline: true}, sinkOver(pipeline.NewInbox(st)))
	if b.handle(groupMsg("F2", &waE2E.Message{Conversation: ps("hello")})) {
		t.Fatal("handler acknowledged with a failed inbox write")
	}
	// No sink installed yet: nothing is acknowledged.
	c := newTestAdapter(t, &fakeWA{offline: true}, nil)
	if c.handle(groupMsg("F3", &waE2E.Message{Conversation: ps("hello")})) {
		t.Fatal("handler acknowledged with no sink")
	}
}

func TestHandlerNeverBlocks(t *testing.T) {
	ctx := context.Background()
	st, err := gwstore.Open(ctx, filepath.Join(t.TempDir(), "g.db"), gwstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// No worker drains the inbox, so its wake-up signal stays full; every
	// fake network call fails the test (offline).
	f := &fakeWA{offline: true, comment: &waE2E.Message{Conversation: ps("reply")}}
	a := newTestAdapter(t, f, sinkOver(pipeline.NewInbox(st)))
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := &waE2E.Message{Conversation: ps("hello")}
			if i%5 == 0 {
				msg = &waE2E.Message{EncCommentMessage: &waE2E.EncCommentMessage{TargetMessageKey: &waCommon.MessageKey{ID: ps("A")}}}
			}
			if !a.handle(groupMsg(fmt.Sprintf("NB%d", i), msg)) {
				t.Errorf("message %d not acknowledged", i)
			}
		}(i)
	}
	wg.Wait()
	a.handle(&events.ClientOutdated{}) // its version fetch runs off the handler
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("50 events took %s to handle", d)
	}
	if n, _ := st.InboxLen(ctx); n != 50 {
		t.Fatalf("inbox holds %d rows, want 50", n)
	}
	for _, c := range f.calls {
		if c != "DecryptComment" {
			t.Fatalf("handler made call %s", c)
		}
	}
}

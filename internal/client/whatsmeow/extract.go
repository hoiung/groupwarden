package whatsmeow

import (
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/hoiung/groupwarden/internal/client"
)

// maxUnwrap bounds wrapper unwrapping (a hostile message cannot loop us).
const maxUnwrap = 8

// futureProofName is the wrapper type WhatsApp uses for ephemeral, view-once,
// document-with-caption, edited and similar envelopes.
var futureProofName = (&waE2E.FutureProofMessage{}).ProtoReflect().Descriptor().FullName()

// unwrap peels envelope messages (ephemeral, view-once, document with
// caption, edited, device-sent, ...) until the content message is reached.
func unwrap(m *waE2E.Message) *waE2E.Message {
	for i := 0; i < maxUnwrap && m != nil; i++ {
		if inner := m.GetDeviceSentMessage().GetMessage(); inner != nil {
			m = inner
			continue
		}
		next := futureProofInner(m)
		if next == nil {
			return m
		}
		m = next
	}
	return m
}

// futureProofInner returns the message inside m when m's only content is a
// FutureProofMessage envelope.
func futureProofInner(m *waE2E.Message) *waE2E.Message {
	var inner *waE2E.Message
	set := 0
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Name() == "messageContextInfo" {
			return true // delivery metadata, not content
		}
		set++
		if fd.Kind() == protoreflect.MessageKind && fd.Message().FullName() == futureProofName {
			if fp, ok := v.Message().Interface().(*waE2E.FutureProofMessage); ok {
				inner = fp.GetMessage()
			}
		}
		return true
	})
	if set != 1 {
		return nil
	}
	return inner
}

// contextInfo finds the ContextInfo of m's content part (mentions live there).
func contextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	var ci *waE2E.ContextInfo
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind {
			return true
		}
		sub := v.Message()
		f := sub.Descriptor().Fields().ByName("contextInfo")
		if f != nil && sub.Has(f) {
			if c, ok := sub.Get(f).Message().Interface().(*waE2E.ContextInfo); ok {
				ci = c
				return false
			}
		}
		return true
	})
	return ci
}

// extracted is what one content message yields.
type extracted struct {
	fields   []client.Field
	mentions []client.JID
	media    *client.Media
}

// extract reads every sender-written field of m (already unwrapped). The
// quoted message of a reply (ContextInfo.QuotedMessage) is never read.
func extract(m *waE2E.Message) extracted {
	var raw []client.Field
	add := func(name, text string) {
		if strings.TrimSpace(text) != "" {
			raw = append(raw, client.Field{Name: name, Text: text})
		}
	}
	add("body", m.GetConversation())
	if et := m.GetExtendedTextMessage(); et != nil {
		add("body", et.GetText())
		add("link.title", et.GetTitle())
		add("link.description", et.GetDescription())
		add("link.url", et.GetMatchedText())
	}
	if im := m.GetImageMessage(); im != nil {
		add("caption", im.GetCaption())
	}
	if vm := m.GetVideoMessage(); vm != nil {
		add("caption", vm.GetCaption())
	}
	if vm := m.GetPtvMessage(); vm != nil {
		add("caption", vm.GetCaption())
	}
	if dm := m.GetDocumentMessage(); dm != nil {
		add("caption", dm.GetCaption())
		add("document.file_name", dm.GetFileName())
		add("document.title", dm.GetTitle())
	}
	// (V4 is a FutureProofMessage envelope, opened by unwrap.)
	for _, p := range []*waE2E.PollCreationMessage{
		m.GetPollCreationMessage(), m.GetPollCreationMessageV2(), m.GetPollCreationMessageV3(),
		m.GetPollCreationMessageV5(), m.GetPollCreationMessageV6(),
	} {
		if p == nil {
			continue
		}
		add("poll.question", p.GetName())
		for _, o := range p.GetOptions() {
			add("poll.option", o.GetOptionName())
		}
	}
	contacts := []*waE2E.ContactMessage{m.GetContactMessage()}
	if ca := m.GetContactsArrayMessage(); ca != nil {
		add("contact.name", ca.GetDisplayName())
		contacts = append(contacts, ca.GetContacts()...)
	}
	for _, c := range contacts {
		if c == nil {
			continue
		}
		add("contact.name", c.GetDisplayName())
		for _, n := range vcardNumbers(c.GetVcard()) {
			add("contact.number", n)
		}
	}
	if l := m.GetLocationMessage(); l != nil {
		add("location.name", l.GetName())
		add("location.address", l.GetAddress())
		add("location.url", l.GetURL())
		add("location.comment", l.GetComment())
	}
	if l := m.GetLiveLocationMessage(); l != nil {
		add("location.comment", l.GetCaption())
	}
	if e := m.GetEventMessage(); e != nil {
		add("event.name", e.GetName())
		add("event.description", e.GetDescription())
		add("event.join_link", e.GetJoinLink())
		if loc := e.GetLocation(); loc != nil {
			add("location.name", loc.GetName())
			add("location.address", loc.GetAddress())
		}
	}
	if g := m.GetGroupInviteMessage(); g != nil {
		add("invite.group_name", g.GetGroupName())
		add("invite.caption", g.GetCaption())
		if code := g.GetInviteCode(); code != "" {
			add("invite.link", "https://chat.whatsapp.com/"+code)
		}
	}
	if l := m.GetListMessage(); l != nil {
		add("list.title", l.GetTitle())
		add("list.description", l.GetDescription())
		add("list.button", l.GetButtonText())
		add("list.footer", l.GetFooterText())
		for _, s := range l.GetSections() {
			add("list.section", s.GetTitle())
			for _, r := range s.GetRows() {
				add("list.row", r.GetTitle())
				add("list.row", r.GetDescription())
			}
		}
	}
	if b := m.GetButtonsMessage(); b != nil {
		add("buttons.header", b.GetText())
		add("buttons.text", b.GetContentText())
		add("buttons.footer", b.GetFooterText())
		for _, btn := range b.GetButtons() {
			add("buttons.button", btn.GetButtonText().GetDisplayText())
		}
	}
	if im := m.GetInteractiveMessage(); im != nil {
		add("buttons.header", im.GetHeader().GetTitle())
		add("buttons.text", im.GetBody().GetText())
		add("buttons.footer", im.GetFooter().GetText())
	}

	var mentions []client.JID
	if ci := contextInfo(m); ci != nil {
		for _, j := range ci.GetMentionedJID() {
			mentions = append(mentions, client.JID(j))
		}
	}
	for i := range raw {
		raw[i].Match = client.MaskMentions(raw[i].Text, mentions)
	}
	return extracted{fields: raw, mentions: mentions, media: mediaOf(m)}
}

// vcardNumbers returns the phone numbers on the TEL lines of a vCard.
func vcardNumbers(vcard string) []string {
	var out []string
	for _, line := range strings.Split(vcard, "\n") {
		line = strings.TrimSpace(line)
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if k := strings.ToUpper(key); k == "TEL" || strings.HasPrefix(k, "TEL;") || strings.HasSuffix(k, ".TEL") ||
			strings.Contains(k, ".TEL;") {
			out = append(out, strings.TrimSpace(val))
		}
	}
	return out
}

// mediaOf describes m's attachment, keeping the part DownloadMedia needs.
func mediaOf(m *waE2E.Message) *client.Media {
	var md client.Media
	part := &waE2E.Message{}
	switch {
	case m.GetImageMessage() != nil:
		x := m.GetImageMessage()
		md = client.Media{Kind: "image", MimeType: x.GetMimetype(), Size: x.GetFileLength()}
		part.ImageMessage = x
	case m.GetVideoMessage() != nil:
		x := m.GetVideoMessage()
		md = client.Media{Kind: "video", MimeType: x.GetMimetype(), Size: x.GetFileLength()}
		part.VideoMessage = x
	case m.GetPtvMessage() != nil:
		x := m.GetPtvMessage()
		md = client.Media{Kind: "video", MimeType: x.GetMimetype(), Size: x.GetFileLength()}
		part.VideoMessage = x
	case m.GetDocumentMessage() != nil:
		x := m.GetDocumentMessage()
		md = client.Media{Kind: "document", MimeType: x.GetMimetype(), FileName: x.GetFileName(), Size: x.GetFileLength()}
		part.DocumentMessage = x
	case m.GetAudioMessage() != nil:
		x := m.GetAudioMessage()
		md = client.Media{Kind: "audio", MimeType: x.GetMimetype(), Size: x.GetFileLength()}
		part.AudioMessage = x
	default:
		return nil
	}
	// The part is copied without its ContextInfo: the quoted message must not
	// travel with the attachment either.
	part = proto.Clone(part).(*waE2E.Message)
	clearContextInfo(part)
	// A part that will not serialise keeps Raw empty; DownloadMedia then
	// fails loudly and the evidence records the attachment by type and size.
	if raw, err := proto.Marshal(part); err == nil {
		md.Raw = raw
	}
	return &md
}

func clearContextInfo(m *waE2E.Message) {
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() == protoreflect.MessageKind {
			sub := v.Message()
			if f := sub.Descriptor().Fields().ByName("contextInfo"); f != nil {
				sub.Clear(f)
			}
		}
		return true
	})
}

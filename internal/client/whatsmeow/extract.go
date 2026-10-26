package whatsmeow

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/hoiung/groupwarden/internal/client"
)

// maxUnwrap bounds wrapper unwrapping (a hostile message cannot loop us).
const maxUnwrap = 8

// futureProofName is the wrapper type WhatsApp uses for ephemeral, view-once,
// document-with-caption, edited and similar envelopes.
var futureProofName = (&waE2E.FutureProofMessage{}).ProtoReflect().Descriptor().FullName()

// messageName is the type every envelope holds its content in.
var messageName = (&waE2E.Message{}).ProtoReflect().Descriptor().FullName()

// keyFields carry nothing a sender wrote: the sender key a member's device
// hands the bot before its first post to a group (and again whenever it
// starts a new key), in a part of its own with the post's ID, and delivery
// metadata.
var keyFields = map[protoreflect.Name]bool{
	"senderKeyDistributionMessage": true, "fastRatchetKeySenderKeyDistributionMessage": true,
	"groupRootKeyShare": true, "messageContextInfo": true,
}

// noTextFields are content with no text the bot reads (a sticker, a pin, an
// RSVP, an album header whose pictures come as messages of their own), so
// reading nothing from them is expected.
var noTextFields = map[protoreflect.Name]bool{
	"stickerMessage": true, "locationMessage": true, "liveLocationMessage": true, "keepInChatMessage": true,
	"pinInChatMessage": true, "encEventResponseMessage": true, "requestPhoneNumberMessage": true,
	"callLogMesssage": true, "albumMessage": true, "messageHistoryNotice": true, "messageHistoryBundle": true,
}

// shape is a message's form for the logs, never its text.
type shape struct {
	wrappers []string // the envelopes around the content, outermost first
	fields   []string // the content message's set fields
	unknown  []string // field numbers this library version does not know: "N", or "<field>.N" one level down
}

func shapeOf(raw, content *waE2E.Message) shape {
	return shape{wrappers: wrapperPath(raw), fields: setFields(content), unknown: unknownFields(content)}
}

// only reports whether every set field is in allowed and no unknown field
// can hold content. A field this library version does not know inside a key
// or delivery field is not content: WhatsApp adds such fields often, and the
// key part must still be recognised.
func (s shape) only(allowed ...map[protoreflect.Name]bool) bool {
	for _, u := range s.unknown {
		if field, _, nested := strings.Cut(u, "."); !nested || !keyFields[protoreflect.Name(field)] {
			return false
		}
	}
next:
	for _, f := range s.fields {
		for _, a := range allowed {
			if a[protoreflect.Name(f)] {
				continue next
			}
		}
		return false
	}
	return true
}

// has reports whether a set field is in set.
func (s shape) has(set map[protoreflect.Name]bool) bool {
	return slices.ContainsFunc(s.fields, func(f string) bool { return set[protoreflect.Name(f)] })
}

// rangeOrdered calls fn for each set field of m in field-number order
// (Range's own order is unspecified), until fn returns false.
func rangeOrdered(m protoreflect.Message, fn func(protoreflect.FieldDescriptor, protoreflect.Value) bool) {
	type field struct {
		fd protoreflect.FieldDescriptor
		v  protoreflect.Value
	}
	var set []field
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		set = append(set, field{fd, v})
		return true
	})
	slices.SortFunc(set, func(a, b field) int { return cmp.Compare(a.fd.Number(), b.fd.Number()) })
	for _, f := range set {
		if !fn(f.fd, f.v) {
			return
		}
	}
}

// wrapperPath names the envelopes from m to its content, outermost first.
func wrapperPath(m *waE2E.Message) []string {
	var path []string
	for i := 0; i < maxUnwrap && m != nil; i++ {
		var next *waE2E.Message
		rangeOrdered(m.ProtoReflect(), func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
				return true
			}
			f := v.Message().Descriptor().Fields().ByName("message")
			if f == nil || f.Kind() != protoreflect.MessageKind || f.Message().FullName() != messageName || !v.Message().Has(f) {
				return true
			}
			if inner, ok := v.Message().Get(f).Message().Interface().(*waE2E.Message); ok {
				path, next = append(path, string(fd.Name())), inner
				return false
			}
			return true
		})
		m = next
	}
	return path
}

func setFields(m *waE2E.Message) []string {
	var out []string
	rangeOrdered(m.ProtoReflect(), func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		out = append(out, string(fd.Name()))
		return true
	})
	return out
}

// unknownFields lists the field numbers of m, and of each message set in it,
// that the pinned library's protobufs do not define.
func unknownFields(m *waE2E.Message) []string {
	out := unknownNumbers("", m.ProtoReflect(), nil)
	rangeOrdered(m.ProtoReflect(), func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() == protoreflect.MessageKind && !fd.IsList() && !fd.IsMap() {
			out = unknownNumbers(string(fd.Name())+".", v.Message(), out)
		}
		return true
	})
	return out
}

func unknownNumbers(prefix string, m protoreflect.Message, out []string) []string {
	b := m.GetUnknown()
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n >= 0 {
			b = b[n:]
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return appendOnce(out, prefix+"malformed")
		}
		b = b[n:]
		out = appendOnce(out, prefix+strconv.Itoa(int(num)))
	}
	return out
}

func appendOnce(out []string, s string) []string {
	if slices.Contains(out, s) {
		return out
	}
	return append(out, s)
}

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

// soleContent returns m's only set field that is not a sender key or
// delivery metadata (ok false when it has none or several).
func soleContent(m *waE2E.Message) (protoreflect.FieldDescriptor, protoreflect.Value, bool) {
	var sole protoreflect.FieldDescriptor
	var value protoreflect.Value
	set := 0
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if keyFields[fd.Name()] {
			return true // a sender key or delivery metadata, not content
		}
		set++
		sole, value = fd, v
		return true
	})
	return sole, value, set == 1
}

// futureProofInner returns the message inside m when m's only content is a
// FutureProofMessage envelope.
func futureProofInner(m *waE2E.Message) *waE2E.Message {
	fd, v, ok := soleContent(m)
	if !ok || fd.Kind() != protoreflect.MessageKind || fd.Message().FullName() != futureProofName {
		return nil
	}
	fp, _ := v.Message().Interface().(*waE2E.FutureProofMessage)
	return fp.GetMessage()
}

// commentInner returns the message a comment sent in the clear carries (a
// reply under a community announcement, as a decrypted one reads), when the
// comment is m's only content.
func commentInner(m *waE2E.Message) *waE2E.Message {
	if _, _, ok := soleContent(m); !ok {
		return nil
	}
	return m.GetCommentMessage().GetMessage()
}

// carriesKey reports whether m hands over a sender key: the fields a key part
// is made of, not the delivery data every message carries.
func carriesKey(m *waE2E.Message) bool {
	return m.GetSenderKeyDistributionMessage() != nil || m.GetFastRatchetKeySenderKeyDistributionMessage() != nil ||
		m.GetGroupRootKeyShare() != nil
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
		numbers, text := vcard(c.GetVcard(), c.GetDisplayName())
		for _, n := range numbers {
			add("contact.number", n)
		}
		for _, s := range text {
			add("contact.vcard", s)
		}
	}
	if sc := m.GetScheduledCallCreationMessage(); sc != nil {
		add("call.title", sc.GetTitle())
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

// vcard returns the phone numbers on the TEL lines of a vCard, and the text
// on its other lines (a business's name and description, a URL, an e-mail, a
// note: any can carry a pitch). Its structure (BEGIN, END, VERSION, the
// structured name N), the binary photo or logo, and a full name the same as
// the card's display name are left out. A long line folded onto the next
// (a line break then a space or tab) is joined first.
func vcard(card, displayName string) (numbers, text []string) {
	card = vcardUnfold.Replace(card)
	for _, line := range strings.Split(card, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		k := strings.ToUpper(key)
		k, _, _ = strings.Cut(k, ";") // its parameters ("TEL;TYPE=CELL")
		if i := strings.LastIndexByte(k, '.'); i >= 0 {
			k = k[i+1:] // its group ("item1.TEL")
		}
		switch k {
		case "TEL":
			numbers = append(numbers, val)
		case "BEGIN", "END", "VERSION", "N", "PHOTO", "LOGO", "SOUND", "KEY", "UID", "REV", "PRODID":
		case "FN":
			if val != strings.TrimSpace(displayName) {
				text = append(text, vcardUnescape(val))
			}
		default:
			text = append(text, vcardUnescape(val))
		}
	}
	return numbers, text
}

// vcardUnfold joins a vCard's folded lines (RFC 6350 3.2).
var vcardUnfold = strings.NewReplacer("\r\n ", "", "\r\n\t", "", "\n ", "", "\n\t", "")

// vcardUnescape undoes a vCard value's escapes: "\n" is a line break, and a
// comma, semicolon or backslash after a backslash stands for itself.
func vcardUnescape(s string) string {
	return strings.NewReplacer(`\n`, "\n", `\N`, "\n", `\,`, ",", `\;`, ";", `\\`, `\`).Replace(s)
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

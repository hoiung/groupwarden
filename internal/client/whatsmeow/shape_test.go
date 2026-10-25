package whatsmeow

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/pipeline"
	gwstore "github.com/hoiung/groupwarden/internal/store"
)

// The text and invite code are synthetic.
const (
	stockPitch  = "Synthetic Stock Hub: daily market alerts. Join link: https://chat.whatsapp.com/SYNTHINVITE0001"
	inviteLink  = "https://chat.whatsapp.com/SYNTHINVITE0001"
	hiddenWords = "SYNTHETIC-HIDDEN-WORDS"
)

// delivered builds a message event the way the library hands one over: the
// part as decrypted (RawMessage), then the library's own unwrapping.
func delivered(id string, raw *waE2E.Message) *events.Message {
	e := groupMsg(id, nil)
	e.RawMessage = raw
	return e.UnwrapRaw()
}

// keyPart is the part a member's device sends ahead of its first post to a
// group: its sender key, under the post's own ID.
func keyPart() *waE2E.Message {
	return &waE2E.Message{
		SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{GroupID: ps(groupJID.String()),
			AxolotlSenderKeyDistributionMessage: []byte{1, 2, 3}},
		MessageContextInfo: &waE2E.MessageContextInfo{},
	}
}

func invitePost() *waE2E.Message {
	return &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: ps(stockPitch), MatchedText: ps(inviteLink),
			Title: ps("Synthetic Stock Hub"), Description: ps("WhatsApp Group Invite")},
		MessageContextInfo: &waE2E.MessageContextInfo{},
	}
}

// withUnknown adds a field this library version does not define, as a newer
// WhatsApp client would send it.
func withUnknown[M proto.Message](m M, num protowire.Number, text string) M {
	b := protowire.AppendTag(nil, num, protowire.BytesType)
	b = protowire.AppendString(b, text)
	m.ProtoReflect().SetUnknown(append(m.ProtoReflect().GetUnknown(), b...))
	return m
}

func TestKeyPartDoesNotHideThePost(t *testing.T) {
	fastKey := &waE2E.Message{FastRatchetKeySenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{
		GroupID: ps(groupJID.String()), AxolotlSenderKeyDistributionMessage: []byte{4, 5, 6}}}
	bundled := keyPart()
	bundled.GroupMentionedMessage = &waE2E.FutureProofMessage{Message: &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: ps(stockPitch)}}}
	// A newer client's delivery data carries a field this library does not know.
	newerKey := keyPart()
	withUnknown(newerKey.MessageContextInfo, 4243, "synthetic delivery data")
	cases := []struct {
		name  string
		parts []*waE2E.Message
	}{
		{"key-then-post", []*waE2E.Message{keyPart(), invitePost()}},
		{"fast-ratchet-key-then-post", []*waE2E.Message{fastKey, invitePost()}},
		{"post-then-key", []*waE2E.Message{invitePost(), keyPart()}},
		{"key-bundled-with-group-mention", []*waE2E.Message{bundled}},
		{"key-with-newer-delivery-field-then-post", []*waE2E.Message{newerKey, invitePost()}},
		{"root-key-share-then-post", []*waE2E.Message{{GroupRootKeyShare: &waE2E.GroupRootKeyShare{}}, invitePost()}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := gwstore.Open(ctx, filepath.Join(t.TempDir(), "g.db"), gwstore.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			a := newTestAdapter(t, &fakeWA{offline: true}, sinkOver(pipeline.NewInbox(st)))
			for _, p := range c.parts {
				if !a.handle(delivered("3EB0SYNTH0001", p)) {
					t.Fatal("handler refused a part")
				}
			}
			rows, err := st.InboxOldest(ctx, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("%d inbox rows, want 1 (the post)", len(rows))
			}
			var m client.Message
			if err := json.Unmarshal(rows[0].Payload, &m); err != nil {
				t.Fatal(err)
			}
			if got := fieldsOf(&m)["body"]; !reflect.DeepEqual(got, []string{stockPitch}) {
				t.Fatalf("stored body %q, want the post's text: the key part took the post's ID", got)
			}
		})
	}
}

// loggedAdapter is a test adapter whose log lines (JSON, masked as in
// production) are kept at level and above.
func loggedAdapter(t *testing.T, sink client.Sink, level slog.Level) (*Adapter, *bytes.Buffer) {
	t.Helper()
	a := newTestAdapter(t, &fakeWA{offline: true}, sink)
	var buf bytes.Buffer
	a.log = slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level, ReplaceAttr: mask.ReplaceAttr}))
	return a, &buf
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestMessageShapeLogged(t *testing.T) {
	sink := &fakeSink{}
	a, buf := loggedAdapter(t, sink, slog.LevelDebug)
	post := invitePost()
	withUnknown(post.ExtendedTextMessage, 4242, hiddenWords)
	if !a.handle(delivered("3EB0SYNTH0002", &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: post}})) {
		t.Fatal("handler refused the message")
	}
	lines := logLines(t, buf)
	if len(lines) != 1 || lines[0]["msg"] != "group message" || lines[0]["level"] != "DEBUG" {
		t.Fatalf("log lines %v, want one debug \"group message\" line", lines)
	}
	l := lines[0]
	for key, want := range map[string][]string{
		"wrappers": {"ephemeralMessage"},
		"fields":   {"extendedTextMessage", "messageContextInfo"},
		"unknown":  {"extendedTextMessage.4242"},
		"read":     {"body", "link.title", "link.description", "link.url"},
	} {
		if got := strs(l[key]); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if l["id"] != "3EB0SYNTH0002" {
		t.Errorf("id = %v", l["id"])
	}
	for _, text := range []string{"Synthetic", hiddenWords, "SYNTHINVITE"} {
		if strings.Contains(buf.String(), text) {
			t.Fatalf("the log holds message text %q: %s", text, buf)
		}
	}
}

func TestUnreadableMessageWarns(t *testing.T) {
	const warn = "a group message had no content the bot can read"
	cases := []struct {
		name     string
		msg      *waE2E.Message
		warns    bool
		stored   bool
		fields   []string
		unknown  []string
		wrappers []string
	}{
		{"unknown-field", withUnknown(&waE2E.Message{MessageContextInfo: &waE2E.MessageContextInfo{}}, 140, hiddenWords),
			true, true, []string{"messageContextInfo"}, []string{"140"}, nil},
		{"known-field-not-read", &waE2E.Message{ScheduledCallCreationMessage: &waE2E.ScheduledCallCreationMessage{Title: ps(hiddenWords)}},
			true, true, []string{"scheduledCallCreationMessage"}, nil, nil},
		{"wrapper-beside-content", &waE2E.Message{
			GroupMentionedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{Conversation: ps(hiddenWords)}},
			SpoilerMessage:        &waE2E.FutureProofMessage{Message: &waE2E.Message{Conversation: ps(hiddenWords)}}},
			true, true, []string{"groupMentionedMessage", "spoilerMessage"}, nil, []string{"groupMentionedMessage"}},
		{"sticker", &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}, false, true, nil, nil, nil},
		// The commonest posts without text: an attachment with no caption, and
		// an album header whose pictures come as messages of their own.
		{"image-no-caption", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: ps("image/jpeg")}}, false, true, nil, nil, nil},
		{"voice-note", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: ps("audio/ogg"), PTT: proto.Bool(true)}},
			false, true, nil, nil, nil},
		{"album-header", &waE2E.Message{AlbumMessage: &waE2E.AlbumMessage{ExpectedImageCount: proto.Uint32(2)}}, false, true, nil, nil, nil},
		{"key-only", keyPart(), false, false, nil, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink := &fakeSink{}
			a, buf := loggedAdapter(t, sink, slog.LevelInfo) // the level the bot runs at
			if !a.handle(delivered("3EB0SYNTH0003", c.msg)) {
				t.Fatal("handler refused the message")
			}
			if stored := len(sink.events()) == 1; stored != c.stored {
				t.Fatalf("stored %v, want %v", stored, c.stored)
			}
			lines := logLines(t, buf)
			if !c.warns {
				if len(lines) != 0 {
					t.Fatalf("logged %v at info, want nothing", lines)
				}
				return
			}
			if len(lines) != 1 || lines[0]["msg"] != warn || lines[0]["level"] != "WARN" {
				t.Fatalf("log lines %v, want one %q warning", lines, warn)
			}
			l := lines[0]
			if got := strs(l["fields"]); !reflect.DeepEqual(got, c.fields) {
				t.Errorf("fields = %v, want %v", got, c.fields)
			}
			got := map[string][]string{}
			for _, key := range []string{"unknown", "wrappers", "read"} {
				if l[key] != nil {
					got[key] = strs(l[key])
				}
			}
			if !reflect.DeepEqual(got["unknown"], c.unknown) || !reflect.DeepEqual(got["wrappers"], c.wrappers) || len(got["read"]) != 0 {
				t.Errorf("unknown %v wrappers %v read %v, want %v, %v and nothing read", got["unknown"], got["wrappers"],
					got["read"], c.unknown, c.wrappers)
			}
			if strings.Contains(buf.String(), hiddenWords) {
				t.Fatalf("the log holds the message text: %s", buf)
			}
		})
	}
}

func TestKeyPartLoggedAtDebug(t *testing.T) {
	sink := &fakeSink{}
	a, buf := loggedAdapter(t, sink, slog.LevelDebug)
	if !a.handle(delivered("3EB0SYNTH0004", keyPart())) {
		t.Fatal("handler refused the key part")
	}
	lines := logLines(t, buf)
	if len(sink.events()) != 0 || len(lines) != 1 || lines[0]["msg"] != "a message part with no sender content, not stored" ||
		lines[0]["id"] != "3EB0SYNTH0004" || !reflect.DeepEqual(strs(lines[0]["fields"]), []string{"senderKeyDistributionMessage", "messageContextInfo"}) {
		t.Fatalf("stored %d events, log lines %v", len(sink.events()), lines)
	}
}

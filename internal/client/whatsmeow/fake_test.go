package whatsmeow

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	wm "go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/hoiung/groupwarden/internal/client"
)

// Synthetic IDs (see .secret-pii-allowlist).
var (
	groupJID  = types.NewJID("99999000000111", types.GroupServer)
	group2JID = types.NewJID("99999000000222", types.GroupServer)
	spammer   = types.NewJID("99999000000444", types.HiddenUserServer)
	spammerPN = types.NewJID("447700900123", types.DefaultUserServer)
	member2   = types.NewJID("99999000000555", types.HiddenUserServer)
	member3   = types.NewJID("99999000000666", types.HiddenUserServer)
	botPhone  = types.JID{User: "447700900789", Server: types.DefaultUserServer, Device: 7}
	botLID    = types.NewJID("99999000000777", types.HiddenUserServer)
)

var errNetwork = errors.New("network call made")

// fakeWA records every call. With offline set, any call that would reach
// WhatsApp's servers fails the test.
type fakeWA struct {
	t       *testing.T
	offline bool

	mu    sync.Mutex
	calls []string

	revokeChat, revokeSender types.JID
	revokeID                 types.MessageID
	sentTo                   types.JID
	sent                     *waE2E.Message

	participants []types.GroupParticipant
	joinResult   types.JID
	info         *types.GroupInfo
	infoErr      error
	iqResp       *waBinary.Node
	iqErr        error
	iqTo         types.JID
	iqContent    waBinary.Node

	comment, secretEdit       *waE2E.Message
	commentErr, secretEditErr error
	downloaded                wm.DownloadableMessage
	devices                   []types.JID
	subGroups                 []*types.GroupLinkTarget
	requests                  []types.GroupParticipantRequest
	requestAction             wm.ParticipantRequestChange
	onWhatsApp                []types.IsOnWhatsAppResponse
	lids                      map[string]types.JID // phone user -> LID in the local store
}

func (f *fakeWA) record(name string, network bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if network && f.offline {
		f.t.Errorf("network call %s made while handling an event", name)
	}
}

func (f *fakeWA) ConnectContext(context.Context) error { f.record("Connect", true); return nil }
func (f *fakeWA) Disconnect()                          { f.record("Disconnect", false) }
func (f *fakeWA) BuildRevoke(chat, sender types.JID, id types.MessageID) *waE2E.Message {
	f.record("BuildRevoke", false)
	f.revokeChat, f.revokeSender, f.revokeID = chat, sender, id
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{RemoteJID: proto.String(chat.String()), ID: proto.String(id), Participant: proto.String(sender.String())},
	}}
}
func (f *fakeWA) SendMessage(_ context.Context, to types.JID, m *waE2E.Message, _ ...wm.SendRequestExtra) (wm.SendResponse, error) {
	f.record("SendMessage", true)
	f.sentTo, f.sent = to, m
	return wm.SendResponse{}, nil
}
func (f *fakeWA) UpdateGroupParticipants(context.Context, types.JID, []types.JID, wm.ParticipantChange) ([]types.GroupParticipant, error) {
	f.record("UpdateGroupParticipants", true)
	return f.participants, nil
}
func (f *fakeWA) GetJoinedGroups(context.Context) ([]*types.GroupInfo, error) {
	f.record("GetJoinedGroups", true)
	return nil, nil
}
func (f *fakeWA) GetGroupInfo(context.Context, types.JID) (*types.GroupInfo, error) {
	f.record("GetGroupInfo", true)
	return f.info, f.infoErr
}
func (f *fakeWA) GetSubGroups(context.Context, types.JID) ([]*types.GroupLinkTarget, error) {
	f.record("GetSubGroups", true)
	return f.subGroups, nil
}
func (f *fakeWA) GetGroupRequestParticipants(context.Context, types.JID) ([]types.GroupParticipantRequest, error) {
	f.record("GetGroupRequestParticipants", true)
	return f.requests, nil
}
func (f *fakeWA) UpdateGroupRequestParticipants(_ context.Context, _ types.JID, _ []types.JID, action wm.ParticipantRequestChange) ([]types.GroupParticipant, error) {
	f.record("UpdateGroupRequestParticipants", true)
	f.requestAction = action
	return f.participants, nil
}
func (f *fakeWA) JoinGroupWithLink(context.Context, string) (types.JID, error) {
	f.record("JoinGroupWithLink", true)
	return f.joinResult, nil
}
func (f *fakeWA) GetGroupInfoFromLink(context.Context, string) (*types.GroupInfo, error) {
	f.record("GetGroupInfoFromLink", true)
	return f.info, f.infoErr
}
func (f *fakeWA) Download(_ context.Context, m wm.DownloadableMessage) ([]byte, error) {
	f.record("Download", true)
	f.downloaded = m
	return []byte("file-bytes"), nil
}
func (f *fakeWA) DecryptComment(context.Context, *events.Message) (*waE2E.Message, error) {
	f.record("DecryptComment", false) // a local session-store read
	return f.comment, f.commentErr
}
func (f *fakeWA) DecryptSecretEncryptedMessage(context.Context, *events.Message) (*waE2E.Message, error) {
	f.record("DecryptSecretEncryptedMessage", false)
	return f.secretEdit, f.secretEditErr
}
func (f *fakeWA) GetUserDevices(context.Context, []types.JID) ([]types.JID, error) {
	f.record("GetUserDevices", true)
	return f.devices, nil
}
func (f *fakeWA) IsOnWhatsApp(context.Context, []string) ([]types.IsOnWhatsAppResponse, error) {
	f.record("IsOnWhatsApp", true)
	if f.onWhatsApp == nil {
		return nil, errNetwork
	}
	return f.onWhatsApp, nil
}
func (f *fakeWA) ownIDs() (types.JID, types.JID) { return botPhone, botLID }
func (f *fakeWA) lidForPhone(_ context.Context, phone types.JID) (types.JID, error) {
	f.record("lidForPhone", false)
	return f.lids[phone.User], nil
}
func (f *fakeWA) sendGroupIQ(_ context.Context, to types.JID, content waBinary.Node) (*waBinary.Node, error) {
	f.record("sendGroupIQ", true)
	f.iqTo, f.iqContent = to, content
	return f.iqResp, f.iqErr
}

// fakeSink records what the handler persisted and signalled.
type fakeSink struct {
	mu         sync.Mutex
	persisted  []client.Event
	lifecycle  []client.Lifecycle
	persistErr error
}

func (s *fakeSink) Persist(ev client.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.persistErr != nil {
		return s.persistErr
	}
	s.persisted = append(s.persisted, ev)
	return nil
}

func (s *fakeSink) Lifecycle(l client.Lifecycle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycle = append(s.lifecycle, l)
}

func (s *fakeSink) events() []client.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]client.Event(nil), s.persisted...)
}

func (s *fakeSink) lifecycles() []client.Lifecycle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]client.Lifecycle(nil), s.lifecycle...)
}

func newTestAdapter(t *testing.T, f *fakeWA, sink client.Sink) *Adapter {
	t.Helper()
	f.t = t
	a := &Adapter{cli: f, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now,
		updateVersion: func(context.Context) error { return errors.New("no update in tests") }}
	if sink != nil {
		a.Events(sink)
	}
	return a
}

// groupMsg wraps content in a group message event from the spammer.
func groupMsg(id string, content *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: groupJID, Sender: spammer, SenderAlt: spammerPN, IsGroup: true},
			ID:            id, PushName: "Ann", Timestamp: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC),
		},
		Message: content,
	}
}

// convert runs one event through the handler and returns the persisted event.
func convert(t *testing.T, a *Adapter, sink *fakeSink, e *events.Message) client.Event {
	t.Helper()
	before := len(sink.events())
	if !a.handle(e) {
		t.Fatalf("handler refused %s", e.Info.ID)
	}
	evs := sink.events()
	if len(evs) != before+1 {
		t.Fatalf("handler persisted %d events for %s, want 1", len(evs)-before, e.Info.ID)
	}
	return evs[len(evs)-1]
}

func fieldsOf(m *client.Message) map[string][]string {
	out := map[string][]string{}
	for _, f := range m.Fields {
		out[f.Name] = append(out[f.Name], f.Text)
	}
	return out
}

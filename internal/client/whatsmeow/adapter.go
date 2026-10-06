// Package whatsmeow is the only package that imports the WhatsApp library
// (go.mau.fi/whatsmeow, pinned in go.mod). It implements client.Adapter.
package whatsmeow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"time"

	wm "go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite" // pure-Go SQLite driver "sqlite"

	"github.com/hoiung/groupwarden/internal/client"
	gwstore "github.com/hoiung/groupwarden/internal/store"
)

// CodeNotParticipant is the per-participant error WhatsApp returns when a
// remove targets someone who is not in the group: the member is already gone.
// The capability probe (AC 1.9 E8) confirms it live.
const CodeNotParticipant = 404

// localTimeout bounds local session-store work done inside the event handler.
const localTimeout = 10 * time.Second

// callError marks WhatsApp's rate-limit (429) and permission (401, 403)
// refusals with client.ErrRateLimited / client.ErrNotAdmin, so callers back
// off or mark a group uncovered without knowing the library's errors.
func callError(err error) error {
	var iq *wm.IQError
	if err == nil || !errors.As(err, &iq) {
		return err
	}
	switch iq.Code {
	case 429:
		return fmt.Errorf("%w: %w", client.ErrRateLimited, err)
	case 401, 403:
		return fmt.Errorf("%w: %w", client.ErrNotAdmin, err)
	}
	return err
}

// waClient is the slice of *whatsmeow.Client the adapter uses; tests fake it.
type waClient interface {
	ConnectContext(ctx context.Context) error
	Disconnect()
	BuildRevoke(chat, sender types.JID, id types.MessageID) *waE2E.Message
	SendMessage(ctx context.Context, to types.JID, message *waE2E.Message, extra ...wm.SendRequestExtra) (wm.SendResponse, error)
	UpdateGroupParticipants(ctx context.Context, jid types.JID, changes []types.JID, action wm.ParticipantChange) ([]types.GroupParticipant, error)
	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
	GetGroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error)
	GetSubGroups(ctx context.Context, community types.JID) ([]*types.GroupLinkTarget, error)
	GetGroupRequestParticipants(ctx context.Context, jid types.JID) ([]types.GroupParticipantRequest, error)
	UpdateGroupRequestParticipants(ctx context.Context, jid types.JID, changes []types.JID, action wm.ParticipantRequestChange) ([]types.GroupParticipant, error)
	JoinGroupWithLink(ctx context.Context, code string) (types.JID, error)
	GetGroupInfoFromLink(ctx context.Context, code string) (*types.GroupInfo, error)
	Download(ctx context.Context, msg wm.DownloadableMessage) ([]byte, error)
	DecryptComment(ctx context.Context, evt *events.Message) (*waE2E.Message, error)
	DecryptSecretEncryptedMessage(ctx context.Context, evt *events.Message) (*waE2E.Message, error)
	GetUserDevices(ctx context.Context, jids []types.JID) ([]types.JID, error)
	IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)

	// Added by realClient around the library's store and internal API.
	ownIDs() (phone, lid types.JID)
	lidForPhone(ctx context.Context, phone types.JID) (types.JID, error)
	sendGroupIQ(ctx context.Context, to types.JID, content waBinary.Node) (*waBinary.Node, error)
}

// realClient adds the store lookups and the internal group IQ to the library client.
type realClient struct{ *wm.Client }

func (r realClient) ownIDs() (types.JID, types.JID) {
	var phone types.JID
	if r.Store.ID != nil {
		phone = *r.Store.ID
	}
	return phone, r.Store.LID
}

func (r realClient) lidForPhone(ctx context.Context, phone types.JID) (types.JID, error) {
	return r.Store.LIDs.GetLIDForPN(ctx, phone)
}

func (r realClient) sendGroupIQ(ctx context.Context, to types.JID, content waBinary.Node) (*waBinary.Node, error) {
	// Joining a community's linked group has no public call; WhatsApp's own
	// "join_linked_group" request goes through the library's internal API.
	//lint:ignore SA1019 the internal API is the only route to join_linked_group (README "Known limits")
	return r.DangerousInternals().SendGroupIQ(ctx, "set", to, content) //nolint:staticcheck // see above
}

// Options configures Open.
type Options struct {
	DataDir string // holds whatsmeow.db
	Log     *slog.Logger
}

// Adapter implements client.Adapter on whatsmeow.
type Adapter struct {
	cli       waClient
	real      *wm.Client // nil in tests
	container *sqlstore.Container
	log       *slog.Logger

	sink atomic.Pointer[sinkBox]
	// updateVersion fetches WhatsApp's current web version once after a
	// client-outdated refusal.
	updateVersion func(ctx context.Context) error
	outdatedTried atomic.Bool
	now           func() time.Time
}

type sinkBox struct{ s client.Sink }

var _ client.Adapter = (*Adapter)(nil)

// Open opens the session store under opts.DataDir and builds the client.
func Open(ctx context.Context, opts Options) (*Adapter, error) {
	if err := gwstore.CheckLinuxFS(opts.DataDir); err != nil {
		return nil, err
	}
	path := filepath.Join(opts.DataDir, "whatsmeow.db")
	container, err := sqlstore.New(ctx, "sqlite", gwstore.DSN(path), newLogger(opts.Log).Sub("store"))
	if err != nil {
		return nil, fmt.Errorf("open WhatsApp session store: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = container.Close()
		return nil, fmt.Errorf("load WhatsApp device: %w", err)
	}
	cli := wm.NewClient(device, newLogger(opts.Log).Sub("client"))
	configure(cli)
	a := &Adapter{cli: realClient{cli}, real: cli, container: container, log: opts.Log, updateVersion: fetchLatestVersion, now: time.Now}
	cli.AddEventHandlerWithSuccessStatus(a.handle)
	return a, nil
}

// configure sets the library options groupwarden depends on.
func configure(cli *wm.Client) {
	// Acknowledge a message only after every handler returned, and only when
	// they succeeded: the inbox write happens first, so a crash never loses
	// an accepted message. The decrypted-event buffer lets a redelivery be
	// read again after a failed handler.
	cli.SynchronousAck = true
	cli.EnableDecryptedEventBuffer = true
	// History sync is never downloaded: groupwarden acts only on live messages.
	cli.ManualHistorySyncDownload = true
	// Reconnects are groupwarden's own (capped backoff, alerts).
	cli.EnableAutoReconnect = false
}

func fetchLatestVersion(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	v, err := wm.GetLatestVersion(ctx, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	store.SetWAVersion(*v)
	return nil
}

// Paired reports whether the session store holds a linked device.
func (a *Adapter) Paired() bool {
	phone, _ := a.cli.ownIDs()
	return !phone.IsEmpty()
}

// Events installs the sink the handler persists to.
func (a *Adapter) Events(sink client.Sink) { a.sink.Store(&sinkBox{s: sink}) }

// Connect opens the connection; the result arrives as a Lifecycle event.
func (a *Adapter) Connect(ctx context.Context) error {
	if !a.Paired() {
		return errors.New("this device is not paired: run `groupwarden pair` first")
	}
	return a.cli.ConnectContext(ctx)
}

// Disconnect closes the connection.
func (a *Adapter) Disconnect() { a.cli.Disconnect() }

// Close disconnects and closes the session store.
func (a *Adapter) Close() error {
	a.cli.Disconnect()
	if a.container != nil {
		return a.container.Close()
	}
	return nil
}

// Self is the bot's own account.
func (a *Adapter) Self() client.Self {
	phone, lid := a.cli.ownIDs()
	return client.Self{Phone: jid(phone), LID: jid(lid)}
}

func jid(j types.JID) client.JID {
	if j.IsEmpty() {
		return ""
	}
	return client.JID(j.ToNonAD().String())
}

// parse reads a WhatsApp ID. The library accepts a string with no "@" as a
// bare server name, so an ID without both a user and a server is refused here.
func parse(j client.JID) (types.JID, error) {
	p, err := types.ParseJID(string(j))
	if err != nil {
		return types.JID{}, fmt.Errorf("bad WhatsApp ID %q: %w", j, err)
	}
	if p.User == "" || p.Server == "" {
		return types.JID{}, fmt.Errorf("bad WhatsApp ID %q: needs user@server", j)
	}
	return p, nil
}

func parseAll(js []client.JID) ([]types.JID, error) {
	out := make([]types.JID, len(js))
	for i, j := range js {
		p, err := parse(j)
		if err != nil {
			return nil, err
		}
		out[i] = p
	}
	return out, nil
}

func group(info *types.GroupInfo) client.Group {
	g := client.Group{
		JID:            jid(info.JID),
		Name:           info.Name,
		IsCommunity:    info.IsParent,
		Parent:         jid(info.LinkedParentJID),
		IsAnnouncement: info.IsDefaultSubGroup,
	}
	for _, p := range info.Participants {
		g.Participants = append(g.Participants, client.Participant{
			JID: jid(p.JID), Phone: jid(p.PhoneNumber), LID: jid(p.LID),
			IsAdmin: p.IsAdmin || p.IsSuperAdmin, IsSuperAdmin: p.IsSuperAdmin,
		})
	}
	return g
}

// JoinedGroups lists every group and community the bot is in.
func (a *Adapter) JoinedGroups(ctx context.Context) ([]client.Group, error) {
	infos, err := a.cli.GetJoinedGroups(ctx)
	if err != nil {
		return nil, callError(err)
	}
	out := make([]client.Group, 0, len(infos))
	for _, info := range infos {
		out = append(out, group(info))
	}
	return out, nil
}

// GroupInfo fetches one group.
func (a *Adapter) GroupInfo(ctx context.Context, g client.JID) (client.Group, error) {
	j, err := parse(g)
	if err != nil {
		return client.Group{}, err
	}
	info, err := a.cli.GetGroupInfo(ctx, j)
	if err != nil {
		return client.Group{}, callError(err)
	}
	return group(info), nil
}

// SubGroups lists a community's linked groups.
func (a *Adapter) SubGroups(ctx context.Context, community client.JID) ([]client.GroupRef, error) {
	j, err := parse(community)
	if err != nil {
		return nil, err
	}
	targets, err := a.cli.GetSubGroups(ctx, j)
	if err != nil {
		return nil, callError(err)
	}
	out := make([]client.GroupRef, 0, len(targets))
	for _, t := range targets {
		out = append(out, client.GroupRef{JID: jid(t.JID), Name: t.Name, IsAnnouncement: t.IsDefaultSubGroup})
	}
	return out, nil
}

// JoinRequests lists pending join requests (polled; there is no typed event).
func (a *Adapter) JoinRequests(ctx context.Context, g client.JID) ([]client.JoinRequest, error) {
	j, err := parse(g)
	if err != nil {
		return nil, err
	}
	reqs, err := a.cli.GetGroupRequestParticipants(ctx, j)
	if err != nil {
		return nil, callError(err)
	}
	out := make([]client.JoinRequest, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, client.JoinRequest{JID: jid(r.JID), RequestedAt: r.RequestedAt})
	}
	return out, nil
}

// Revoke deletes msgID for everyone. As a group admin deleting someone
// else's message, the library needs that sender's JID.
func (a *Adapter) Revoke(ctx context.Context, chat, sender client.JID, msgID string) error {
	c, err := parse(chat)
	if err != nil {
		return err
	}
	s, err := parse(sender)
	if err != nil {
		return err
	}
	_, err = a.cli.SendMessage(ctx, c, a.cli.BuildRevoke(c, s, msgID))
	return callError(err)
}

// Remove removes members from a group, reporting each member's outcome.
func (a *Adapter) Remove(ctx context.Context, g client.JID, members []client.JID) ([]client.MemberResult, error) {
	j, err := parse(g)
	if err != nil {
		return nil, err
	}
	ms, err := parseAll(members)
	if err != nil {
		return nil, err
	}
	got, err := a.cli.UpdateGroupParticipants(ctx, j, ms, wm.ParticipantChangeRemove)
	if err != nil {
		return nil, callError(err)
	}
	return memberResults(members, got), nil
}

// RejectJoinRequests rejects pending join requests.
func (a *Adapter) RejectJoinRequests(ctx context.Context, g client.JID, members []client.JID) ([]client.MemberResult, error) {
	j, err := parse(g)
	if err != nil {
		return nil, err
	}
	ms, err := parseAll(members)
	if err != nil {
		return nil, err
	}
	got, err := a.cli.UpdateGroupRequestParticipants(ctx, j, ms, wm.ParticipantChangeReject)
	if err != nil {
		return nil, callError(err)
	}
	return memberResults(members, got), nil
}

// memberResults matches WhatsApp's per-participant answers to the request.
// A member WhatsApp did not answer for is reported as failed.
func memberResults(asked []client.JID, got []types.GroupParticipant) []client.MemberResult {
	byUser := map[string]types.GroupParticipant{}
	for _, p := range got {
		for _, id := range []types.JID{p.JID, p.LID, p.PhoneNumber} {
			if !id.IsEmpty() {
				byUser[id.User+"@"+id.Server] = p
			}
		}
	}
	out := make([]client.MemberResult, 0, len(asked))
	for _, m := range asked {
		p, ok := byUser[m.User()+"@"+m.Server()]
		switch {
		case !ok:
			out = append(out, client.MemberResult{Member: m, Status: client.MemberFailed, Code: -1})
		case p.Error == 0:
			out = append(out, client.MemberResult{Member: m, Status: client.MemberDone})
		case p.Error == CodeNotParticipant:
			out = append(out, client.MemberResult{Member: m, Status: client.MemberAlreadyGone, Code: p.Error})
		default:
			out = append(out, client.MemberResult{Member: m, Status: client.MemberFailed, Code: p.Error})
		}
	}
	return out
}

// JoinWithLink joins through an invite code. WhatsApp answers a join and a
// join request with the same group ID, so membership is re-queried.
func (a *Adapter) JoinWithLink(ctx context.Context, code string) (client.JID, bool, error) {
	g, err := a.cli.JoinGroupWithLink(ctx, code)
	if err != nil {
		return "", false, err
	}
	member, err := a.isMember(ctx, g)
	if err != nil {
		return jid(g), false, err
	}
	return jid(g), !member, nil
}

// isMember re-reads the group: not being allowed to read it means the bot
// only has a pending request.
func (a *Adapter) isMember(ctx context.Context, g types.JID) (bool, error) {
	info, err := a.cli.GetGroupInfo(ctx, g)
	if errors.Is(err, wm.ErrNotInGroup) || errors.Is(err, wm.ErrIQForbidden) || errors.Is(err, wm.ErrGroupNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check membership after join: %w", err)
	}
	phone, lid := a.cli.ownIDs()
	for _, p := range info.Participants {
		for _, id := range []types.JID{p.JID, p.PhoneNumber, p.LID} {
			if !id.IsEmpty() && (id.User == phone.User && id.Server == phone.Server || id.User == lid.User && id.Server == lid.Server) {
				return true, nil
			}
		}
	}
	return false, nil
}

// JoinLinkedGroup sends WhatsApp's "join_linked_group" request to the
// community: a bare result means joined, a membership_approval_request child
// means the join waits for an admin.
func (a *Adapter) JoinLinkedGroup(ctx context.Context, community, g client.JID) (bool, bool, error) {
	c, err := parse(community)
	if err != nil {
		return false, false, err
	}
	j, err := parse(g)
	if err != nil {
		return false, false, err
	}
	resp, err := a.cli.sendGroupIQ(ctx, c, waBinary.Node{Tag: "join_linked_group", Attrs: waBinary.Attrs{"jid": j}})
	if err != nil {
		return false, false, callError(err)
	}
	if resp != nil {
		if _, pending := resp.GetOptionalChildByTag("membership_approval_request"); pending {
			return false, true, nil
		}
	}
	return true, false, nil
}

// InviteInfo looks up an invite code without joining.
func (a *Adapter) InviteInfo(ctx context.Context, code string) (client.Group, client.JID, error) {
	info, err := a.cli.GetGroupInfoFromLink(ctx, code)
	if err != nil {
		return client.Group{}, "", err
	}
	g := group(info)
	return g, g.Parent, nil
}

// DownloadMedia fetches msg's attachment.
func (a *Adapter) DownloadMedia(ctx context.Context, msg *client.Message) ([]byte, string, string, error) {
	if msg.Media == nil || len(msg.Media.Raw) == 0 {
		return nil, "", "", errors.New("message has no downloadable attachment")
	}
	var part waE2E.Message
	if err := proto.Unmarshal(msg.Media.Raw, &part); err != nil {
		return nil, "", "", fmt.Errorf("read attachment reference: %w", err)
	}
	var d wm.DownloadableMessage
	switch {
	case part.GetImageMessage() != nil:
		d = part.GetImageMessage()
	case part.GetVideoMessage() != nil:
		d = part.GetVideoMessage()
	case part.GetDocumentMessage() != nil:
		d = part.GetDocumentMessage()
	case part.GetAudioMessage() != nil:
		d = part.GetAudioMessage()
	default:
		return nil, "", "", errors.New("attachment reference holds no media")
	}
	data, err := a.cli.Download(ctx, d)
	if err != nil {
		return nil, "", "", err
	}
	return data, msg.Media.MimeType, msg.Media.FileName, nil
}

// LinkedDevices lists the bot account's other linked devices.
func (a *Adapter) LinkedDevices(ctx context.Context) ([]client.JID, error) {
	phone, _ := a.cli.ownIDs()
	if phone.IsEmpty() {
		return nil, errors.New("not paired")
	}
	devices, err := a.cli.GetUserDevices(ctx, []types.JID{phone.ToNonAD()})
	if err != nil {
		return nil, err
	}
	var out []client.JID
	for _, d := range devices {
		if d.Device == 0 || d.Device == phone.Device {
			continue // the phone itself, or this device
		}
		out = append(out, client.JID(d.String()))
	}
	return out, nil
}

// ErrNoLID is returned when a phone number has no known WhatsApp LID.
var ErrNoLID = errors.New("no WhatsApp LID known for that number")

// ResolvePhoneToLID finds the LID of a phone number: the local store first,
// then one lookup on WhatsApp (which records the mapping).
func (a *Adapter) ResolvePhoneToLID(ctx context.Context, phone client.JID) (client.JID, error) {
	pn := types.NewJID(phone.User(), types.DefaultUserServer)
	if lid, err := a.cli.lidForPhone(ctx, pn); err != nil {
		return "", err
	} else if !lid.IsEmpty() {
		return jid(lid), nil
	}
	resp, err := a.cli.IsOnWhatsApp(ctx, []string{"+" + pn.User})
	if err != nil {
		return "", err
	}
	if len(resp) == 0 || !resp[0].IsIn {
		return "", fmt.Errorf("%s is not on WhatsApp", phone)
	}
	if resp[0].JID.Server == types.HiddenUserServer {
		return jid(resp[0].JID), nil
	}
	lid, err := a.cli.lidForPhone(ctx, pn)
	if err != nil {
		return "", err
	}
	if lid.IsEmpty() {
		return "", ErrNoLID
	}
	return jid(lid), nil
}

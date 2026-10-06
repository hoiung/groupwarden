// Package client is the boundary between groupwarden and WhatsApp. Everything
// outside internal/client/whatsmeow talks to WhatsApp only through Adapter and
// the plain types below, so no other package imports the WhatsApp library.
package client

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// JID is a WhatsApp address in its string form: a group "…@g.us", a member
// "…@lid" or "…@s.whatsapp.net". Only the whatsmeow adapter parses it.
type JID string

// User is the part before "@" with any ":device" suffix removed.
func (j JID) User() string {
	u, _, _ := strings.Cut(string(j), "@")
	u, _, _ = strings.Cut(u, ":")
	return u
}

// Server is the part after "@" ("g.us", "lid", "s.whatsapp.net").
func (j JID) Server() string {
	_, s, _ := strings.Cut(string(j), "@")
	return s
}

// Bare drops any ":device" suffix ("user:3@lid" → "user@lid").
func (j JID) Bare() JID {
	if j == "" {
		return ""
	}
	return JID(j.User() + "@" + j.Server())
}

// Member is one person by both of their addresses.
type Member struct {
	LID   JID // "…@lid", when known
	Phone JID // "…@s.whatsapp.net", when known
}

// MemberOf sorts a person's addresses (as addressed, and the alternate one)
// into LID and phone number, dropping device suffixes.
func MemberOf(addrs ...JID) Member {
	var m Member
	for _, a := range addrs {
		switch a.Server() {
		case "lid":
			if m.LID == "" {
				m.LID = a.Bare()
			}
		case "s.whatsapp.net":
			if m.Phone == "" {
				m.Phone = a.Bare()
			}
		}
	}
	return m
}

// Key is the ban-list key: the LID, or the phone number while no LID is known.
func (m Member) Key() string {
	if m.LID != "" {
		return string(m.LID)
	}
	return string(m.Phone)
}

// IDs lists the known addresses.
func (m Member) IDs() []string {
	var out []string
	for _, j := range []JID{m.LID, m.Phone} {
		if j != "" {
			out = append(out, string(j))
		}
	}
	return out
}

// InviteCode takes the code out of a group invite link
// ("https://chat.whatsapp.com/<code>") or a bare code.
func InviteCode(link string) (string, error) {
	s := strings.TrimSpace(link)
	s, _, _ = strings.Cut(s, "?")
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		if !strings.Contains(s[:i], "chat.whatsapp.com") {
			return "", fmt.Errorf("%q is not a WhatsApp group invite link", link)
		}
		s = s[i+1:]
	}
	if s == "" || strings.ContainsAny(s, " .:@") {
		return "", fmt.Errorf("%q is not a WhatsApp group invite link", link)
	}
	return s, nil
}

// ErrRateLimited is returned (wrapped) when WhatsApp refuses a call for going
// over its rate limit; the caller backs off.
var ErrRateLimited = errors.New("WhatsApp rate limit")

// ErrNotAdmin is returned (wrapped) when WhatsApp refuses a call because the
// bot is not an admin there.
var ErrNotAdmin = errors.New("the bot is not an admin there")

// MentionMarker replaces every @-mention of a listed member in a field's
// matching view, so a mention is never read as a phone number or a handle.
const MentionMarker = "@mention"

// MaskMentions replaces "@<user>" for every mentioned member with
// MentionMarker, longest user first so one ID that prefixes another is not
// split. It builds a field's matching view.
func MaskMentions(text string, mentions []JID) string {
	if len(mentions) == 0 {
		return text
	}
	users := make([]string, 0, len(mentions))
	for _, j := range mentions {
		if u := j.User(); u != "" {
			users = append(users, u)
		}
	}
	sort.Slice(users, func(a, b int) bool { return len(users[a]) > len(users[b]) })
	for _, u := range users {
		text = strings.ReplaceAll(text, "@"+u, MentionMarker)
	}
	return text
}

// Field is one piece of text the sender wrote.
type Field struct {
	Name  string `json:"name"`  // e.g. "body", "caption", "link.url", "poll.option"
	Text  string `json:"text"`  // exactly as sent (kept for evidence)
	Match string `json:"match"` // matching view: @-mentions replaced by MentionMarker
}

// Media describes an attachment. Its size is known before any download.
type Media struct {
	Kind     string `json:"kind"` // image, video, document, audio, sticker
	MimeType string `json:"mime_type"`
	FileName string `json:"file_name,omitempty"`
	Size     uint64 `json:"size"`
	// Raw is adapter-private: the serialized media part DownloadMedia needs.
	Raw []byte `json:"raw"`
}

// Message is one sender-written message in a group, already unwrapped.
type Message struct {
	Chat      JID       `json:"chat"`
	Sender    JID       `json:"sender"`               // as addressed (a LID in LID-addressed groups)
	SenderAlt JID       `json:"sender_alt,omitempty"` // the other address of the sender, when known
	ID        string    `json:"id"`                   // this message's ID
	TargetID  string    `json:"target_id"`            // the message to act on: ID, or the original's ID for an edit
	Time      time.Time `json:"time"`                 // server timestamp of this message
	IsEdit    bool      `json:"is_edit,omitempty"`
	// IsComment marks a reply in a community announcement group (decrypted).
	IsComment bool `json:"is_comment,omitempty"`
	// PushName is the sender's own display name: a separate rule input that
	// never combines with the body fields.
	PushName string `json:"push_name,omitempty"`
	// FromMetaAI: the sender is WhatsApp's Meta AI participant (exempt from
	// actions; reported only).
	FromMetaAI bool    `json:"from_meta_ai,omitempty"`
	Fields     []Field `json:"fields"`
	Mentions   []JID   `json:"mentions,omitempty"`
	Media      *Media  `json:"media,omitempty"`
}

// UndecryptableReason says why a reply or secret edit could not be read.
type UndecryptableReason string

const (
	// ReasonMissingParentSecret: the parent message's secret is not stored
	// (the announcement predates the bot or was purged). Counted in the daily
	// summary, never alerted.
	ReasonMissingParentSecret UndecryptableReason = "missing_parent_secret"
	// ReasonDecryptError: any other decryption failure. Alerted.
	ReasonDecryptError UndecryptableReason = "decrypt_error"
)

// Undecryptable is a reply or secret edit the adapter could not decrypt.
type Undecryptable struct {
	Chat   JID                 `json:"chat"`
	Sender JID                 `json:"sender"`
	ID     string              `json:"id"`
	Time   time.Time           `json:"time"`
	Reason UndecryptableReason `json:"reason"`
	Detail string              `json:"detail"`
}

// GroupChange is a membership or admin change in a group, as WhatsApp sent it.
type GroupChange struct {
	Group JID `json:"group"`
	// Actor is whoever made the change; empty when WhatsApp did not say.
	Actor      JID       `json:"actor,omitempty"`
	ActorAlt   JID       `json:"actor_alt,omitempty"`
	JoinReason string    `json:"join_reason,omitempty"` // "invite" for an invite-link join
	Time       time.Time `json:"time"`
	Joined     []JID     `json:"joined,omitempty"`
	Left       []JID     `json:"left,omitempty"`
	Promoted   []JID     `json:"promoted,omitempty"`
	Demoted    []JID     `json:"demoted,omitempty"`
}

// JoinedGroup is the bot itself joining or being added to a group.
type JoinedGroup struct {
	Group  JID       `json:"group"`
	Reason string    `json:"reason,omitempty"`
	Actor  JID       `json:"actor,omitempty"`
	Time   time.Time `json:"time"`
}

// Event is something the moderation pipeline decides on. It is persisted to
// the inbox before WhatsApp is acknowledged.
type Event interface {
	// DedupeKey identifies a delivery, so a redelivered event is processed once.
	DedupeKey() string
}

// DedupeKey of a message is its chat and ID (an edit has its own ID).
func (m *Message) DedupeKey() string { return "msg|" + string(m.Chat) + "|" + m.ID }

// DedupeKey of an undecryptable message is its chat and ID.
func (u *Undecryptable) DedupeKey() string { return "msg|" + string(u.Chat) + "|" + u.ID }

// DedupeKey of a group change is built from everything it says (WhatsApp
// gives these notifications no ID), so an identical redelivery is processed once.
func (g *GroupChange) DedupeKey() string {
	return "grp|" + string(g.Group) + "|" + g.Time.UTC().Format(time.RFC3339Nano) + "|" + string(g.Actor) +
		"|j" + joinJIDs(g.Joined) + "|l" + joinJIDs(g.Left) + "|p" + joinJIDs(g.Promoted) + "|d" + joinJIDs(g.Demoted)
}

// DedupeKey of the bot joining a group is the group and time.
func (j *JoinedGroup) DedupeKey() string {
	return "joined|" + string(j.Group) + "|" + j.Time.UTC().Format(time.RFC3339Nano)
}

func joinJIDs(js []JID) string {
	parts := make([]string, len(js))
	for i, j := range js {
		parts[i] = string(j)
	}
	return strings.Join(parts, ",")
}

// LifecycleKind is a connection state change. Lifecycle events are never
// persisted; they drive reconnects, alerts and shutdown.
type LifecycleKind string

const (
	Connected    LifecycleKind = "connected"
	Disconnected LifecycleKind = "disconnected" // transient: reconnect with backoff
	// TemporaryBan: stay disconnected until Expiry, then reconnect paused.
	TemporaryBan LifecycleKind = "temporary_ban"
	// The kinds below are fatal: no reconnect, a priority alert, exit.
	LoggedOut        LifecycleKind = "logged_out"
	StreamReplaced   LifecycleKind = "stream_replaced"
	ClientOutdated   LifecycleKind = "client_outdated"
	CATRefreshFailed LifecycleKind = "cat_refresh_failed"
	ConnectFailure   LifecycleKind = "connect_failure"
)

// Lifecycle is one connection event.
type Lifecycle struct {
	Kind   LifecycleKind
	Detail string
	// Expiry is how long a temporary ban lasts (zero when WhatsApp did not say).
	Expiry time.Duration
}

// Fatal reports whether the kind ends the process (a human must act).
func (k LifecycleKind) Fatal() bool {
	switch k {
	case LoggedOut, StreamReplaced, ClientOutdated, CATRefreshFailed, ConnectFailure:
		return true
	}
	return false
}

// NextStep is the human action a fatal kind needs, quoted in its alert.
func (k LifecycleKind) NextStep() string {
	switch k {
	case LoggedOut:
		return "the bot was unlinked: run `groupwarden pair` and link the bot phone again"
	case StreamReplaced:
		return "another copy is using this session: stop it, then start groupwarden again"
	case ClientOutdated:
		return "WhatsApp rejected this client version: update the whatsmeow pin (docs/runbook.md) and reinstall"
	case CATRefreshFailed:
		return "the connection token could not be refreshed: restart groupwarden; if it repeats, re-pair"
	case ConnectFailure:
		return "WhatsApp refused the connection: check the bot phone and restart groupwarden"
	}
	return ""
}

// Sink receives events from the adapter's WhatsApp event handler.
type Sink interface {
	// Persist durably stores ev and returns only after the write committed.
	// WhatsApp is acknowledged only after Persist returns nil; an error holds
	// the acknowledgement so WhatsApp redelivers. It must not block on the
	// network or on rate limits.
	Persist(ev Event) error
	// Lifecycle delivers a connection event. It must not block.
	Lifecycle(l Lifecycle)
}

// Participant is a group member.
type Participant struct {
	JID          JID  `json:"jid"`
	Phone        JID  `json:"phone,omitempty"`
	LID          JID  `json:"lid,omitempty"`
	IsAdmin      bool `json:"is_admin"`
	IsSuperAdmin bool `json:"is_super_admin"`
}

// Group is a group or community as WhatsApp describes it.
type Group struct {
	JID  JID    `json:"jid"`
	Name string `json:"name"`
	// IsCommunity: this is a community (parent) rather than a chat group.
	IsCommunity bool `json:"is_community"`
	// Parent is the community a linked group belongs to (empty if none).
	Parent JID `json:"parent,omitempty"`
	// IsAnnouncement: the community's default announcement group.
	IsAnnouncement bool          `json:"is_announcement"`
	Participants   []Participant `json:"participants,omitempty"`
}

// GroupRef is a linked group as listed by its community.
type GroupRef struct {
	JID            JID
	Name           string
	IsAnnouncement bool
}

// JoinRequest is a pending request to join a group.
type JoinRequest struct {
	JID         JID
	RequestedAt time.Time
}

// MemberStatus is the outcome for one member of a remove or reject call.
type MemberStatus string

const (
	MemberDone        MemberStatus = "done"
	MemberAlreadyGone MemberStatus = "already_gone"
	MemberFailed      MemberStatus = "failed"
)

// MemberResult is the per-member outcome of Remove or RejectJoinRequests.
type MemberResult struct {
	Member JID
	Status MemberStatus
	Code   int // WhatsApp's per-participant error code (0 when none)
}

// Self is the bot's own account.
type Self struct {
	Phone JID // the bot number's JID with this device
	LID   JID
}

// Adapter is everything groupwarden asks of WhatsApp.
type Adapter interface {
	// Events installs the sink. Call it before Connect.
	Events(sink Sink)
	// Connect opens the connection; the outcome arrives as a Lifecycle event.
	Connect(ctx context.Context) error
	// Disconnect closes the connection (no Lifecycle event follows).
	Disconnect()
	Self() Self
	JoinedGroups(ctx context.Context) ([]Group, error)
	GroupInfo(ctx context.Context, group JID) (Group, error)
	SubGroups(ctx context.Context, community JID) ([]GroupRef, error)
	JoinRequests(ctx context.Context, group JID) ([]JoinRequest, error)
	// Revoke deletes msgID for everyone; sender is the message's author.
	Revoke(ctx context.Context, chat, sender JID, msgID string) error
	Remove(ctx context.Context, group JID, members []JID) ([]MemberResult, error)
	RejectJoinRequests(ctx context.Context, group JID, members []JID) ([]MemberResult, error)
	// JoinWithLink joins through an invite code; pendingApproval is true
	// when it only created a join request.
	JoinWithLink(ctx context.Context, code string) (group JID, pendingApproval bool, err error)
	// JoinLinkedGroup joins a community's linked group without an invite link.
	JoinLinkedGroup(ctx context.Context, community, group JID) (joined, pendingApproval bool, err error)
	// InviteInfo looks up an invite code without joining.
	InviteInfo(ctx context.Context, code string) (group Group, parentCommunity JID, err error)
	// DownloadMedia fetches msg's attachment.
	DownloadMedia(ctx context.Context, msg *Message) (file []byte, mimeType, fileName string, err error)
	// LinkedDevices lists the bot account's OTHER linked devices (the phone
	// and this device excluded).
	LinkedDevices(ctx context.Context) ([]JID, error)
	ResolvePhoneToLID(ctx context.Context, phone JID) (JID, error)
	Close() error
}

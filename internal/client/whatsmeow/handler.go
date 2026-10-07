package whatsmeow

import (
	"context"
	"errors"
	"fmt"

	wm "go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/mask"
)

// handle is the whatsmeow event handler. It converts the event and persists
// it through the sink; the return value tells the library whether to
// acknowledge (false holds the ack, so WhatsApp redelivers). It never makes
// a network call and never waits on a rate limit.
func (a *Adapter) handle(evt any) bool {
	box := a.sink.Load()
	if box == nil {
		return false // no sink yet: do not acknowledge what nobody stored
	}
	sink := box.s
	switch e := evt.(type) {
	case *events.Message:
		ev := a.convertMessage(e)
		if ev == nil {
			return true
		}
		return a.persist(sink, ev)
	case *events.GroupInfo:
		if ev := groupChange(e); ev != nil {
			return a.persist(sink, ev)
		}
	case *events.JoinedGroup:
		return a.persist(sink, &client.JoinedGroup{Group: jid(e.JID), Reason: e.Reason, Actor: ptrJID(e.Sender), Time: a.now(),
			Info: group(&e.GroupInfo)})
	case *events.Connected:
		sink.Lifecycle(client.Lifecycle{Kind: client.Connected})
	case *events.OfflineSyncCompleted:
		// The library handles WhatsApp's notifications one at a time, in the
		// order they arrive, and this marker comes after the offline ones. It
		// is not ordered with Connected, which the library sends from another
		// goroutine after two round trips: either may come first.
		sink.Lifecycle(client.Lifecycle{Kind: client.CaughtUp, Detail: fmt.Sprintf("%d offline events", e.Count)})
	case *events.Disconnected:
		sink.Lifecycle(client.Lifecycle{Kind: client.Disconnected, Detail: "connection closed by the server"})
	case *events.ClientOutdated:
		// The version fetch is a network call: never inside the handler.
		go a.clientOutdated(sink)
	case events.PermanentDisconnect:
		sink.Lifecycle(classify(e))
	case *events.HistorySync:
		// History sync is disabled (ManualHistorySyncDownload); nothing to do.
	}
	return true
}

func (a *Adapter) persist(sink client.Sink, ev client.Event) bool {
	if err := sink.Persist(ev); err != nil {
		a.log.Error("inbox write failed; WhatsApp will redeliver", "err", mask.IDs(err.Error()))
		return false
	}
	return true
}

// classify maps a library permanent-disconnect event to a lifecycle kind.
// Anything unrecognised is a connect failure: fatal, never a reconnect loop.
func classify(e events.PermanentDisconnect) client.Lifecycle {
	l := client.Lifecycle{Detail: e.PermanentDisconnectDescription()}
	switch ev := e.(type) {
	case *events.LoggedOut:
		l.Kind = client.LoggedOut
	case *events.StreamReplaced:
		l.Kind = client.StreamReplaced
	case *events.ClientOutdated:
		l.Kind = client.ClientOutdated
	case *events.CATRefreshError:
		l.Kind = client.CATRefreshFailed
	case *events.TemporaryBan:
		l.Kind = client.TemporaryBan
		l.Expiry = ev.Expire
	default:
		l.Kind = client.ConnectFailure
	}
	return l
}

// clientOutdated tries WhatsApp's latest web version once per process; if
// that works the connection is retried, otherwise the refusal is fatal.
func (a *Adapter) clientOutdated(sink client.Sink) {
	if a.outdatedTried.CompareAndSwap(false, true) {
		err := a.updateVersion(context.Background())
		if err == nil {
			sink.Lifecycle(client.Lifecycle{Kind: client.Disconnected, Detail: "client outdated: updated to WhatsApp's current web version, reconnecting"})
			return
		}
		a.log.Error("client version update failed", "err", err)
	}
	sink.Lifecycle(client.Lifecycle{Kind: client.ClientOutdated, Detail: "client outdated (WhatsApp refused the connection)"})
}

func ptrJID(j *types.JID) client.JID {
	if j == nil {
		return ""
	}
	return jid(*j)
}

func jids(js []types.JID) []client.JID {
	if len(js) == 0 {
		return nil
	}
	out := make([]client.JID, len(js))
	for i, j := range js {
		out[i] = jid(j)
	}
	return out
}

// groupChange keeps membership and admin changes; other metadata is ignored.
func groupChange(e *events.GroupInfo) *client.GroupChange {
	if len(e.Join)+len(e.Leave)+len(e.Promote)+len(e.Demote) == 0 {
		return nil
	}
	return &client.GroupChange{
		Group: jid(e.JID), Actor: ptrJID(e.Sender), ActorAlt: ptrJID(e.SenderPN), JoinReason: e.JoinReason, Time: e.Timestamp,
		Joined: jids(e.Join), Left: jids(e.Leave), Promoted: jids(e.Promote), Demoted: jids(e.Demote),
	}
}

// convertMessage turns a group message into a client event, or nil when it
// carries nothing a sender wrote (receipts, reactions, deletes, votes).
func (a *Adapter) convertMessage(e *events.Message) client.Event {
	info := e.Info
	if !info.IsGroup || info.Chat.Server != types.GroupServer || info.IsFromMe || e.Message == nil {
		return nil
	}
	m := &client.Message{
		Chat: jid(info.Chat), Sender: jid(info.Sender), SenderAlt: jid(info.SenderAlt),
		ID: info.ID, TargetID: info.ID, Time: info.Timestamp, PushName: info.PushName,
		FromMetaAI: info.Sender.IsBot() || info.SenderAlt.IsBot(),
	}
	content := unwrap(e.Message)
	switch {
	case content.GetEncCommentMessage() != nil:
		dec, err := a.decrypt(a.cli.DecryptComment, e)
		if err != nil {
			return undecryptable(e, err)
		}
		content, m.IsComment = unwrap(dec), true
	case content.GetSecretEncryptedMessage() != nil:
		dec, err := a.decrypt(a.cli.DecryptSecretEncryptedMessage, e)
		if err != nil {
			return undecryptable(e, err)
		}
		m.IsEdit, m.TargetID = true, content.GetSecretEncryptedMessage().GetTargetMessageKey().GetID()
		content = unwrap(dec)
		if pm := content.GetProtocolMessage(); pm.GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT && pm.GetEditedMessage() != nil {
			content = unwrap(pm.GetEditedMessage())
		}
	case content.GetProtocolMessage() != nil:
		pm := content.GetProtocolMessage()
		if pm.GetType() != waE2E.ProtocolMessage_MESSAGE_EDIT || pm.GetEditedMessage() == nil {
			return nil // a delete, a setting change: nothing sender-written
		}
		m.IsEdit, m.TargetID = true, pm.GetKey().GetID()
		content = unwrap(pm.GetEditedMessage())
	case content.GetReactionMessage() != nil, content.GetEncReactionMessage() != nil, content.GetPollUpdateMessage() != nil:
		return nil
	}
	x := extract(content)
	m.Fields, m.Mentions, m.Media = x.fields, x.mentions, x.media
	return m
}

func (a *Adapter) decrypt(fn func(context.Context, *events.Message) (*waE2E.Message, error), e *events.Message) (*waE2E.Message, error) {
	ctx, cancel := context.WithTimeout(context.Background(), localTimeout)
	defer cancel()
	return fn(ctx, e)
}

// undecryptable records a reply or secret edit that could not be read: a
// missing parent secret is counted, any other failure alerts.
func undecryptable(e *events.Message, err error) *client.Undecryptable {
	u := &client.Undecryptable{
		Chat: jid(e.Info.Chat), Sender: jid(e.Info.Sender), ID: e.Info.ID, Time: e.Info.Timestamp,
		Reason: client.ReasonDecryptError, Detail: err.Error(),
	}
	if errors.Is(err, wm.ErrOriginalMessageSecretNotFound) {
		u.Reason = client.ReasonMissingParentSecret
	}
	return u
}

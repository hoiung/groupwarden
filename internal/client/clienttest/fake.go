// Package clienttest provides a fake client.Adapter for tests above the
// WhatsApp layer.
package clienttest

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/hoiung/groupwarden/internal/client"
)

// Fake is a scriptable client.Adapter that records every call.
type Fake struct {
	mu   sync.Mutex
	sink client.Sink

	SelfIDs client.Self
	Groups  []client.Group
	// Invites maps an invite code to the group it points at.
	Invites map[string]client.Group
	Devices []client.JID
	// ConnectErr is returned by Connect; when nil Connect reports Connected
	// unless OnConnect says otherwise.
	ConnectErr error
	// OnConnect, when set, replaces the default Connected lifecycle event.
	OnConnect func(f *Fake)

	calls []string
}

var _ client.Adapter = (*Fake)(nil)

func (f *Fake) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

// Calls returns every recorded call ("Connect", "InviteInfo CODE", ...).
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Count counts recorded calls whose name starts with prefix.
func (f *Fake) Count(prefix string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// Emit delivers a lifecycle event as the WhatsApp handler would.
func (f *Fake) Emit(l client.Lifecycle) {
	f.mu.Lock()
	s := f.sink
	f.mu.Unlock()
	s.Lifecycle(l)
}

// Deliver persists an event as the WhatsApp handler would.
func (f *Fake) Deliver(ev client.Event) error {
	f.mu.Lock()
	s := f.sink
	f.mu.Unlock()
	return s.Persist(ev)
}

func (f *Fake) Events(s client.Sink) {
	f.mu.Lock()
	f.sink = s
	f.mu.Unlock()
}

// SetConnectErr changes what Connect returns (safe while the fake is in use).
func (f *Fake) SetConnectErr(err error) {
	f.mu.Lock()
	f.ConnectErr = err
	f.mu.Unlock()
}

func (f *Fake) Connect(context.Context) error {
	f.record("Connect")
	f.mu.Lock()
	err := f.ConnectErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if f.OnConnect != nil {
		f.OnConnect(f)
		return nil
	}
	f.Emit(client.Lifecycle{Kind: client.Connected})
	return nil
}

func (f *Fake) Disconnect()       { f.record("Disconnect") }
func (f *Fake) Close() error      { f.record("Close"); return nil }
func (f *Fake) Self() client.Self { return f.SelfIDs }

func (f *Fake) JoinedGroups(context.Context) ([]client.Group, error) {
	f.record("JoinedGroups")
	return f.Groups, nil
}

func (f *Fake) GroupInfo(_ context.Context, g client.JID) (client.Group, error) {
	f.record("GroupInfo " + string(g))
	for _, x := range f.Groups {
		if x.JID == g {
			return x, nil
		}
	}
	return client.Group{}, errors.New("group not found")
}

func (f *Fake) SubGroups(_ context.Context, community client.JID) ([]client.GroupRef, error) {
	f.record("SubGroups " + string(community))
	var out []client.GroupRef
	for _, x := range f.Groups {
		if x.Parent == community {
			out = append(out, client.GroupRef{JID: x.JID, Name: x.Name, IsAnnouncement: x.IsAnnouncement})
		}
	}
	return out, nil
}

func (f *Fake) JoinRequests(_ context.Context, g client.JID) ([]client.JoinRequest, error) {
	f.record("JoinRequests " + string(g))
	return nil, nil
}

func (f *Fake) Revoke(_ context.Context, chat, sender client.JID, id string) error {
	f.record("Revoke " + string(chat) + " " + string(sender) + " " + id)
	return nil
}

func (f *Fake) Remove(_ context.Context, g client.JID, members []client.JID) ([]client.MemberResult, error) {
	f.record("Remove " + string(g))
	out := make([]client.MemberResult, len(members))
	for i, m := range members {
		out[i] = client.MemberResult{Member: m, Status: client.MemberDone}
	}
	return out, nil
}

func (f *Fake) RejectJoinRequests(_ context.Context, g client.JID, members []client.JID) ([]client.MemberResult, error) {
	f.record("RejectJoinRequests " + string(g))
	out := make([]client.MemberResult, len(members))
	for i, m := range members {
		out[i] = client.MemberResult{Member: m, Status: client.MemberDone}
	}
	return out, nil
}

func (f *Fake) JoinWithLink(_ context.Context, code string) (client.JID, bool, error) {
	f.record("JoinWithLink " + code)
	return f.Invites[code].JID, false, nil
}

func (f *Fake) JoinLinkedGroup(_ context.Context, community, g client.JID) (bool, bool, error) {
	f.record("JoinLinkedGroup " + string(community) + " " + string(g))
	return true, false, nil
}

func (f *Fake) InviteInfo(_ context.Context, code string) (client.Group, client.JID, error) {
	f.record("InviteInfo " + code)
	g, ok := f.Invites[code]
	if !ok {
		return client.Group{}, "", errors.New("invite link not found")
	}
	return g, g.Parent, nil
}

func (f *Fake) DownloadMedia(context.Context, *client.Message) ([]byte, string, string, error) {
	f.record("DownloadMedia")
	return nil, "", "", errors.New("no media in the fake")
}

func (f *Fake) LinkedDevices(context.Context) ([]client.JID, error) {
	f.record("LinkedDevices")
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]client.JID(nil), f.Devices...), nil
}

// SetDevices replaces the linked-device list.
func (f *Fake) SetDevices(d []client.JID) {
	f.mu.Lock()
	f.Devices = d
	f.mu.Unlock()
}

func (f *Fake) ResolvePhoneToLID(_ context.Context, phone client.JID) (client.JID, error) {
	f.record("ResolvePhoneToLID " + string(phone))
	return "", errors.New("unknown number")
}

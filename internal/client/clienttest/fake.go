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
	// Requests are the pending join requests per group.
	Requests map[client.JID][]client.JoinRequest
	// LIDs maps a phone JID to its LID for ResolvePhoneToLID.
	LIDs map[client.JID]client.JID
	// Results sets a member's outcome in Remove and RejectJoinRequests
	// (default done).
	Results map[client.JID]client.MemberStatus
	// OnCall, when set, runs at the start of every recorded call, before the
	// fake answers (tests check what was stored by then).
	OnCall func(call string)
	// Download answers DownloadMedia (default: an error).
	Download func(ctx context.Context, msg *client.Message) ([]byte, string, string, error)
	// Linked answers SubGroups for a community (default: the Groups whose
	// parent it is), so a community can list a group the bot is not in.
	Linked map[client.JID][]client.GroupRef

	errs  map[string][]error
	calls []string
}

var _ client.Adapter = (*Fake)(nil)

func (f *Fake) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	hook := f.OnCall
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}
}

// FailNext makes the next calls of method ("Revoke", "Remove", ...) return
// errs, one per call, in order.
func (f *Fake) FailNext(method string, errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errs == nil {
		f.errs = map[string][]error{}
	}
	f.errs[method] = append(f.errs[method], errs...)
}

func (f *Fake) nextErr(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.errs[method]
	if len(q) == 0 {
		return nil
	}
	f.errs[method] = q[1:]
	return q[0]
}

func (f *Fake) results(members []client.JID) []client.MemberResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]client.MemberResult, len(members))
	for i, m := range members {
		st := client.MemberDone
		if s, ok := f.Results[m]; ok {
			st = s
		}
		out[i] = client.MemberResult{Member: m, Status: st}
	}
	return out
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
	if refs, ok := f.Linked[community]; ok {
		return refs, nil
	}
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
	if err := f.nextErr("JoinRequests"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]client.JoinRequest(nil), f.Requests[g]...), nil
}

func (f *Fake) Revoke(_ context.Context, chat, sender client.JID, id string) error {
	f.record("Revoke " + string(chat) + " " + string(sender) + " " + id)
	return f.nextErr("Revoke")
}

func (f *Fake) Remove(_ context.Context, g client.JID, members []client.JID) ([]client.MemberResult, error) {
	f.record("Remove " + string(g) + " " + joinJIDs(members))
	if err := f.nextErr("Remove"); err != nil {
		return nil, err
	}
	return f.results(members), nil
}

func (f *Fake) RejectJoinRequests(_ context.Context, g client.JID, members []client.JID) ([]client.MemberResult, error) {
	f.record("RejectJoinRequests " + string(g) + " " + joinJIDs(members))
	if err := f.nextErr("RejectJoinRequests"); err != nil {
		return nil, err
	}
	return f.results(members), nil
}

func joinJIDs(js []client.JID) string {
	parts := make([]string, len(js))
	for i, j := range js {
		parts[i] = string(j)
	}
	return strings.Join(parts, ",")
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

func (f *Fake) DownloadMedia(ctx context.Context, msg *client.Message) ([]byte, string, string, error) {
	f.record("DownloadMedia " + msg.ID)
	if f.Download != nil {
		return f.Download(ctx, msg)
	}
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
	if err := f.nextErr("ResolvePhoneToLID"); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if lid, ok := f.LIDs[phone]; ok {
		return lid, nil
	}
	return "", errors.New("unknown number")
}

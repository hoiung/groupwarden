package pipeline

import (
	"encoding/json"
	"fmt"

	"github.com/hoiung/groupwarden/internal/client"
)

// Inbox row kinds.
const (
	kindMessage       = "message"
	kindUndecryptable = "undecryptable"
	kindGroupChange   = "group_change"
	kindJoinedGroup   = "joined_group"
)

func encode(ev client.Event) (string, []byte, error) {
	var kind string
	switch ev.(type) {
	case *client.Message:
		kind = kindMessage
	case *client.Undecryptable:
		kind = kindUndecryptable
	case *client.GroupChange:
		kind = kindGroupChange
	case *client.JoinedGroup:
		kind = kindJoinedGroup
	default:
		return "", nil, fmt.Errorf("inbox: unknown event type %T", ev)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return "", nil, fmt.Errorf("inbox: encode %s: %w", kind, err)
	}
	return kind, b, nil
}

func decode(kind string, payload []byte) (client.Event, error) {
	var ev client.Event
	switch kind {
	case kindMessage:
		ev = &client.Message{}
	case kindUndecryptable:
		ev = &client.Undecryptable{}
	case kindGroupChange:
		ev = &client.GroupChange{}
	case kindJoinedGroup:
		ev = &client.JoinedGroup{}
	default:
		return nil, fmt.Errorf("inbox: unknown row kind %q", kind)
	}
	if err := json.Unmarshal(payload, ev); err != nil {
		return nil, fmt.Errorf("inbox: decode %s: %w", kind, err)
	}
	return ev, nil
}

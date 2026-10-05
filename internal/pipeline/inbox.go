// Package pipeline moves each WhatsApp event from the inbox to a recorded
// decision: the handler only persists, a worker decides.
package pipeline

import (
	"context"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/store"
)

// persistTimeout bounds how long the WhatsApp handler waits for the local
// database; past it the event is not acknowledged and WhatsApp redelivers.
const persistTimeout = 10 * time.Second

// Inbox persists events from the WhatsApp handler and wakes the worker.
type Inbox struct {
	st   *store.Store
	wake chan struct{}
}

// NewInbox returns an inbox over st.
func NewInbox(st *store.Store) *Inbox {
	return &Inbox{st: st, wake: make(chan struct{}, 1)}
}

// Persist stores ev durably, then nudges the worker without waiting for it.
// It does no network call and never waits on a rate limit.
func (in *Inbox) Persist(ev client.Event) error {
	kind, payload, err := encode(ev)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	if _, err := in.st.InboxPut(ctx, ev.DedupeKey(), kind, payload); err != nil {
		return err
	}
	select {
	case in.wake <- struct{}{}:
	default: // a wake-up is already pending
	}
	return nil
}

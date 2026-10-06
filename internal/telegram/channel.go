package telegram

import (
	"context"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/store"
)

// Alert stores a as a report for the admin chat (the alert's kind is the
// report's kind) and wakes delivery, so an alert survives a restart like any
// report. A fatal or stopping alert is delivered before Alert returns, within
// ctx's deadline: the process is about to exit.
func (c *Chat) Alert(ctx context.Context, a alert.Alert) error {
	priority := a.Priority || a.Kind.Priority()
	if a.Kind == alert.StorageFailure {
		// The database cannot be written, and the failing write may be one
		// made by a delivery that holds the slot: send around the store, in
		// the background.
		go c.sendDirect(context.WithoutCancel(ctx), a.Text)
		return nil
	}
	if _, err := c.Store.AddReport(ctx, store.Report{Kind: string(a.Kind), Priority: priority, Text: a.Text,
		Buttons: a.Buttons}, nil); err != nil {
		c.Log.Error("could not store an alert; sending it directly", "kind", string(a.Kind), "err", err)
		go c.sendDirect(context.WithoutCancel(ctx), a.Text)
		return err
	}
	c.Wake()
	if a.Kind == alert.FatalDisconnect || a.Kind == alert.Stopping {
		return c.Flush(ctx)
	}
	return nil
}

// sendDirect posts text without the store: no record, no retry. It runs past
// the caller (whose deadline is the alert's), so ctx carries no cancel.
func (c *Chat) sendDirect(ctx context.Context, text string) {
	ctx, cancel := context.WithTimeout(ctx, directDeadline)
	defer cancel()
	if _, err := c.sendText(ctx, true, c.labelText(text), quote{}, 0, nil); err != nil {
		c.Log.Error("could not send an alert directly", "err", err.Error())
		return
	}
	c.Log.Info("alert sent directly (not stored)")
}

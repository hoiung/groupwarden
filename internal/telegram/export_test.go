package telegram

import (
	"context"

	"github.com/go-telegram/bot/models"
)

// Handle passes one update to the chat as the update poller would.
func (c *Chat) Handle(ctx context.Context, u *models.Update) { c.handle(ctx, c.api, u) }

// ButtonCodes exposes the button → callback code table.
var ButtonCodes = buttonCodes

// Split exposes the message splitter; MaxUnits its limit.
var Split = split

const MaxUnits = maxUnits

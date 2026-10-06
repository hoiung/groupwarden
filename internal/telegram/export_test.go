package telegram

import (
	"context"
	"time"

	"github.com/go-telegram/bot/models"
)

// Handle passes one update to the chat as the update poller would.
func (c *Chat) Handle(ctx context.Context, u *models.Update) { c.handle(ctx, c.api, u) }

// ButtonCodes exposes the button → callback code table.
var ButtonCodes = buttonCodes

// Split exposes the message splitter; MaxUnits its limit.
var Split = split

// CommandList is the pinned command list (and the reply to an unknown command).
var CommandList = commandList

// SetupLoop runs the loop that keeps the menu and pinned list in place;
// SetPinRetry shortens its wait after a refused pin.
func (c *Chat) SetupLoop(ctx context.Context) { c.setupLoop(ctx) }
func (c *Chat) SetPinRetry(d time.Duration)   { c.pinRetry = d }

const MaxUnits = maxUnits

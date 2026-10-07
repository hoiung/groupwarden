package telegram

import (
	"context"
	"errors"
	"net/http"
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

// RequestTimeout is the timeout of the client the bot's default HTTP client
// sends a request for method with (a URL shaped as the library makes it).
func RequestTimeout(method string) time.Duration {
	h := newHTTPClient()
	var used time.Duration
	for _, c := range []*http.Client{h.requests, h.uploads} {
		c.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
			used = c.Timeout
			return nil, errors.New("not sent")
		})
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.telegram.org/bot0:x/"+method, nil)
	if err != nil {
		panic(err)
	}
	_, _ = h.Do(req)
	return used
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

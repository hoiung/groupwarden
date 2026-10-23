package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/hoiung/groupwarden/internal/config"
)

// Handle passes one update to the chat as the update poller would.
func (c *Chat) Handle(ctx context.Context, u *models.Update) { c.handle(ctx, c.api, u) }

// ButtonCodes exposes the button → callback code table.
var ButtonCodes = buttonCodes

// Split exposes the message splitter; MaxUnits its limit.
var Split = split

// CommandList is the pinned command list (and the reply to an unknown command).
var CommandList = commandList

// ActionNeeds is the role each command and button needs under acl:.
var ActionNeeds = actionNeeds

// AdminCommandNames lists the admin commands, without the "/".
func AdminCommandNames() []string {
	out := make([]string, 0, len(adminCommands))
	for _, c := range adminCommands {
		out = append(out, c.name)
	}
	return out
}

// Decide is the role function: whether userID may do action under acl, the
// role it found and the role the action needs.
func (c *Chat) Decide(ctx context.Context, acl *config.ACL, userID int64, action string) (allowed bool, role, needs Role) {
	d := c.decide(ctx, acl, userID, action)
	return d.allowed, d.role, d.needs
}

// SetupLoop runs the loop that keeps the menu and pinned list in place;
// SetPinRetry shortens its wait after a refused pin.
func (c *Chat) SetupLoop(ctx context.Context) { c.setupLoop(ctx) }
func (c *Chat) SetPinRetry(d time.Duration)   { c.pinRetry = d }

const MaxUnits = maxUnits

// Header is a report's text before the member's message.
var Header = header

// PostAttachment runs one try of uploadLoop's work.
func (c *Chat) PostAttachment(ctx context.Context) (bool, error) { return c.postAttachment(ctx) }

// RequestTimeout is the timeout of the client a chat built by New with no
// Options.HTTPClient (as the bot runs) sends a request for method with (a URL
// shaped as the library makes it).
func RequestTimeout(method string) time.Duration {
	c, err := New(Options{Token: strconv.Itoa(1) + ":x"}, &Chat{}) // secret-allow (built at run time; never sent)
	if err != nil {
		panic(err)
	}
	h, ok := c.client.(httpClient)
	if !ok {
		panic(fmt.Sprintf("the bot's client is a %T, not the package's httpClient", c.client))
	}
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

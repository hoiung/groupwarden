// Package telegram is the admin chat: one private Telegram group where the
// bot delivers every stored report and alert, posts (then takes down) a
// deleted post's attachment, removes members' message text once the evidence
// window ends, keeps a list of its commands pinned at the top, and takes
// buttons and commands from that group's admins only.
//
// Everything it still has to do lives in groupwarden.db (undelivered reports,
// queued edits, posted messages), so a restart picks up where it stopped.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"golang.org/x/time/rate"

	"github.com/hoiung/groupwarden/internal/action"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/store"
)

// Fixed timings and sizes (Telegram's limits, not tunables).
const (
	// groupPerMinute is Telegram's limit for messages to one group.
	groupPerMinute = 20
	// priorityHeadroom is how many of those a minute routine messages leave
	// free, so an alert never waits behind a backlog.
	priorityHeadroom = 5
	// digestAfter: with more routine reports than this waiting, those that
	// can be combined go out as one digest.
	digestAfter = 10
	// digestMax is how many reports one digest combines at most.
	digestMax = 20
	// refusedAfter: Telegram refusing the bot (401/403) for this long is not
	// a blip: the admins cannot reach [Undo], so removals and bans pause.
	refusedAfter = 10 * time.Minute
	// idlePoll: with nothing to do, look again this often (a wake comes sooner).
	idlePoll = 30 * time.Second
	// pollTimeout is the long-poll wait for button presses and commands (and
	// how long any other request may take, as the library has it).
	pollTimeout = time.Minute
	// uploadTimeout bounds a file upload (sendDocument, editMessageMedia): the
	// largest attachment kept (50 MiB, the schema's maximum for
	// evidence.max_attachment_mb) goes within it at 1.5 Mbit/s, as its
	// download does.
	uploadTimeout = 5 * time.Minute
	// directDeadline bounds a message sent around the store (the database
	// cannot be written).
	directDeadline = 30 * time.Second
	// errorLogEvery: the update poller retries a failing request every few
	// seconds; the same error is logged at most this often.
	errorLogEvery = time.Minute
)

// Options says how to reach the admin chat.
type Options struct {
	Token  string // from the secrets file; never logged
	ChatID int64  // from the secrets file; a migration recorded in the store wins
	// ServerURL and HTTPClient replace api.telegram.org and the default
	// client (tests point them at a fake Bot API).
	ServerURL  string
	HTTPClient bot.HttpClient
}

// GroupNamer gives a WhatsApp group's name for reports ("" when unknown).
type GroupNamer interface {
	GroupName(jid string) string
}

// Chat is the admin chat.
type Chat struct {
	Store    *store.Store
	Config   *config.Holder
	Groups   GroupNamer
	Controls Controls
	Log      *slog.Logger
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error

	api   *bot.Bot
	token string
	// sem lets one delivery (or a synchronous Flush) post at a time, so a
	// report is never sent twice and parts stay in order. It is a channel,
	// not a mutex, so a Flush with a deadline can give up waiting for it.
	sem    chan struct{}
	shared *rate.Limiter // every message (≤ 20 a minute)
	wake   chan struct{}
	// setupWake reruns the command menu and pinned list setup (the chat
	// moved); pinRetry is how long it waits after Telegram refused the pin.
	setupWake chan struct{}
	pinRetry  time.Duration

	// owed: store writes recording posts the store would not take; settle
	// makes them before anything else is posted (see record).
	owedMu sync.Mutex
	owed   []owedWrite

	mu     sync.Mutex // guards the fields below
	chatID int64
	// blockedUntil: Telegram said retry_after; nothing is sent before it.
	blockedUntil time.Time
	// refusedSince: the first 401/403 of the current refusal episode.
	refusedSince time.Time
	refused      bool // the episode crossed refusedAfter (paused, unhealthy)
	okRecorded   bool // tg_ok=1 is written for the current healthy stretch
	lastErr      string
	lastErrAt    time.Time
	menuFor      int64 // the chat the command menu was set for
	// routineSent: when the last routine messages went (at most
	// groupPerMinute-priorityHeadroom in any minute, counted on actual send
	// times so the headroom holds whatever the shared bucket adds).
	routineSent []time.Time
	// uploads: attachments whose upload failed, by evidence ID (postAttachment).
	uploads map[int64]uploadRetry
}

// New builds the admin chat (no network call yet).
func New(o Options, c *Chat) (*Chat, error) {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = action.Sleep
	}
	c.token, c.chatID = o.Token, o.ChatID
	c.sem = make(chan struct{}, 1)
	c.shared = rate.NewLimiter(rate.Every(time.Minute/groupPerMinute), 1)
	c.wake = make(chan struct{}, 1)
	c.setupWake = make(chan struct{}, 1)
	c.pinRetry = pinRetryDefault
	c.uploads = map[int64]uploadRetry{}
	client := o.HTTPClient
	if client == nil {
		client = newHTTPClient()
	}
	opts := []bot.Option{
		bot.WithHTTPClient(pollTimeout, client),
		bot.WithSkipGetMe(),
		bot.WithNotAsyncHandlers(),
		bot.WithDefaultHandler(c.handle),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "callback_query"}),
		// The poller reports its errors (with the raw update on a decode
		// failure) here: redacted, noted for the refusal episode, throttled.
		bot.WithErrorsHandler(c.pollError),
	}
	if o.ServerURL != "" {
		opts = append(opts, bot.WithServerURL(o.ServerURL))
	}
	b, err := bot.New(o.Token, opts...)
	if err != nil {
		return nil, c.redact(err)
	}
	c.api = b
	return c, nil
}

// Run takes button presses and commands, keeps the command list pinned and
// delivers everything queued, until ctx ends.
func (c *Chat) Run(ctx context.Context) {
	if err := c.restoreChatID(ctx); err != nil {
		c.Log.Error("read the admin chat ID", "err", err)
	}
	if n, err := c.Store.ReleaseUnfinishedPresses(ctx); err != nil {
		c.Log.Error("could not release the presses the last run left unfinished", "err", err)
	} else if n > 0 {
		c.Log.Warn("released presses the last run claimed but never finished; they can be pressed again", "presses", n)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		c.api.Start(ctx)
	}()
	go func() {
		defer wg.Done()
		c.setupLoop(ctx)
	}()
	c.deliverLoop(ctx)
	wg.Wait()
}

// Wake asks the chat to deliver now.
func (c *Chat) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// restoreChatID applies a chat migration recorded earlier.
func (c *Chat) restoreChatID(ctx context.Context) error {
	st, err := c.Store.Status(ctx)
	if err != nil {
		return err
	}
	if v := st[store.StatusTelegramChat].Value; v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id == 0 {
			return fmt.Errorf("stored admin chat ID %q is not a number", v)
		}
		c.mu.Lock()
		c.chatID = id
		c.mu.Unlock()
	}
	return nil
}

// ChatID is the admin chat's current ID.
func (c *Chat) ChatID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chatID
}

// slotKey marks a context whose caller holds the delivery slot.
type slotKey struct{}

// slotHold is that mark: held is false while a routine wait has let the slot
// go (and stays false if taking it back failed).
type slotHold struct{ held bool }

// lock takes the delivery slot, or gives up when ctx ends. The context it
// returns carries the hold; pass it on and give it to unlock.
func (c *Chat) lock(ctx context.Context) (context.Context, error) {
	if err := c.acquire(ctx); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, slotKey{}, &slotHold{held: true}), nil
}

func (c *Chat) acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unlock gives the slot back, if ctx (from lock) still holds it.
func (c *Chat) unlock(ctx context.Context) {
	if h, ok := ctx.Value(slotKey{}).(*slotHold); ok && h.held {
		h.held = false
		<-c.sem
	}
}

// waitTurn waits until a message may go: past any retry_after, (a routine
// message) until fewer than groupPerMinute-priorityHeadroom routine messages
// went in the last minute, then through the shared bucket.
func (c *Chat) waitTurn(ctx context.Context, priority bool) error {
	const routineMax = groupPerMinute - priorityHeadroom
	for {
		now := c.Now()
		c.mu.Lock()
		wait := c.blockedUntil.Sub(now)
		if n := len(c.routineSent); !priority && n >= routineMax {
			wait = max(wait, c.routineSent[n-routineMax].Add(time.Minute).Sub(now))
		}
		c.mu.Unlock()
		if wait <= 0 {
			break
		}
		if err := c.pause(ctx, priority, wait); err != nil {
			return err
		}
	}
	now := c.Now()
	if d := c.shared.ReserveN(now, 1).DelayFrom(now); d > 0 {
		if err := c.pause(ctx, priority, d); err != nil {
			return err
		}
	}
	if !priority {
		c.mu.Lock()
		c.routineSent = append(c.routineSent, c.Now())
		if len(c.routineSent) > routineMax {
			c.routineSent = c.routineSent[len(c.routineSent)-routineMax:]
		}
		c.mu.Unlock()
	}
	return nil
}

// pause sleeps d before a send. A routine send holding the delivery slot lets
// it go meanwhile and takes it back after, so a priority report (a fatal
// alert's flush) never waits behind a routine message's wait: the routine
// budget, its turn in the shared bucket, or a retry_after.
func (c *Chat) pause(ctx context.Context, priority bool, d time.Duration) error {
	h, ok := ctx.Value(slotKey{}).(*slotHold)
	if priority || !ok || !h.held {
		return c.Sleep(ctx, d)
	}
	h.held = false
	<-c.sem
	if err := c.Sleep(ctx, d); err != nil {
		return err
	}
	if err := c.acquire(ctx); err != nil {
		return err
	}
	h.held = true
	return nil
}

// call makes one request that posts to, edits or deletes in the admin chat,
// in its turn, and applies what Telegram's answer says about the chat: wait
// (retry_after), its new ID (migration), or the bot being refused (401/403).
// fn gets the chat ID to use. Errors come back redacted.
func (c *Chat) call(ctx context.Context, priority bool, fn func(ctx context.Context, chatID int64) error) error {
	for migrated := false; ; migrated = true {
		if err := c.waitTurn(ctx, priority); err != nil {
			return err
		}
		err := fn(ctx, c.ChatID())
		if err == nil {
			c.noteOK(ctx)
			return nil
		}
		err = c.redact(err)
		if d, ok := retryAfter(err); ok {
			c.mu.Lock()
			c.blockedUntil = c.Now().Add(d)
			c.mu.Unlock()
			c.Log.Warn("telegram asked the bot to wait", "retry_after", d)
			return err
		}
		if to, ok := migratedTo(err); ok && !migrated {
			c.migrate(ctx, to)
			continue
		}
		if refusal(err) {
			c.noteRefused(ctx, err)
		}
		return err
	}
}

// migrate follows the group to its new ID (it became a supergroup).
func (c *Chat) migrate(ctx context.Context, to int64) {
	c.mu.Lock()
	from := c.chatID
	c.chatID = to
	c.mu.Unlock()
	if from == to {
		return
	}
	c.Log.Warn("the admin chat moved to a new ID; following it", "from", from, "to", to)
	if err := c.Store.SetStatus(context.WithoutCancel(ctx), map[string]string{
		store.StatusTelegramChat: strconv.FormatInt(to, 10)}); err != nil {
		c.Log.Error("could not record the admin chat's new ID", "err", err)
	}
	c.wakeSetup() // the menu and the pinned list belong to the old ID
}

// noteOK ends a refusal episode (and its pause) at the first request that
// went through: the first after a refusal, and the first of a run (an
// episode the last run paused for, and a restart then ended, is over too).
func (c *Chat) noteOK(ctx context.Context) {
	c.mu.Lock()
	wasRefused, recorded := c.refused, c.okRecorded
	c.refusedSince, c.refused, c.okRecorded = time.Time{}, false, true
	c.mu.Unlock()
	if recorded && !wasRefused {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if err := c.Store.SetStatus(ctx, map[string]string{store.StatusTelegramOK: "1"}); err != nil {
		c.Log.Error("write status", "err", err)
	}
	if err := c.Store.ClearPause(ctx, store.SourceTelegram); err != nil {
		c.Log.Error("could not lift the Telegram pause", "err", err)
		return
	}
	c.Log.Info("telegram accepts the bot; any Telegram pause is lifted", "after_refusal", wasRefused)
}

// noteRefused counts a 401/403. Refused for refusedAfter, the bot is
// unhealthy and removals and bans pause: no admin could press [Undo].
func (c *Chat) noteRefused(ctx context.Context, err error) {
	now := c.Now()
	c.mu.Lock()
	if c.refusedSince.IsZero() {
		c.refusedSince = now
	}
	since := c.refusedSince
	cross := !c.refused && now.Sub(since) >= refusedAfter
	if cross {
		c.refused, c.okRecorded = true, false
	}
	c.mu.Unlock()
	if !cross {
		return
	}
	ctx = context.WithoutCancel(ctx)
	reason := fmt.Sprintf("Telegram has refused the bot since %s (%v): no admin can press [Undo]. "+
		"Fix the bot token or add the bot back to the admin chat.", since.UTC().Format(time.RFC3339), err)
	if perr := c.Store.SetPause(ctx, store.Pause{Source: store.SourceTelegram, Scope: store.ScopeRemoveBan,
		Reason: reason, Since: since}); perr != nil {
		c.Log.Error("could not pause removals and bans", "err", perr)
	}
	if serr := c.Store.SetStatus(ctx, map[string]string{store.StatusTelegramOK: "0"}); serr != nil {
		c.Log.Error("write status", "err", serr)
	}
	c.Log.Error("telegram keeps refusing the bot: removals and bans PAUSED, healthcheck unhealthy", "since", since,
		"err", err.Error())
}

// pollError receives the update poller's errors.
func (c *Chat) pollError(err error) {
	err = c.redact(err)
	if refusal(err) {
		c.noteRefused(context.Background(), err)
	}
	now := c.Now()
	msg := err.Error()
	c.mu.Lock()
	quiet := msg == c.lastErr && now.Sub(c.lastErrAt) < errorLogEvery
	if !quiet {
		c.lastErr, c.lastErrAt = msg, now
	}
	c.mu.Unlock()
	if !quiet {
		c.Log.Warn("telegram updates", "err", mask.IDs(msg))
	}
}

// redactedError hides the bot token from an error's text and keeps the
// original reachable for errors.Is / errors.As.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redact replaces the bot token wherever it appears in err's text.
func (c *Chat) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if c.token != "" {
		msg = strings.ReplaceAll(msg, c.token, "<token>")
	}
	return &redactedError{msg: msg, err: err}
}

// retryAfter is Telegram's "too many requests, retry after N seconds".
func retryAfter(err error) (time.Duration, bool) {
	var tm *bot.TooManyRequestsError
	if errors.As(err, &tm) {
		return time.Duration(max(tm.RetryAfter, 1)) * time.Second, true
	}
	return 0, false
}

// migratedTo is Telegram's "the group became a supergroup with a new ID".
func migratedTo(err error) (int64, bool) {
	var me *bot.MigrateError
	if errors.As(err, &me) && me.MigrateToChatID != 0 {
		return int64(me.MigrateToChatID), true
	}
	return 0, false
}

// refusal: Telegram refuses the bot (token revoked, or the bot is no longer
// in the group).
func refusal(err error) bool {
	return errors.Is(err, bot.ErrorForbidden) || errors.Is(err, bot.ErrorUnauthorized)
}

// badRequest: Telegram will never accept this request (the message is gone,
// too old to delete, or already in that state).
func badRequest(err error) bool { return errors.Is(err, bot.ErrorBadRequest) }

// gone: Telegram will never let the bot edit or delete this posted message: a
// bad request, or the chat it was posted in became a supergroup (the old
// chat's messages stay where they are; the library's MigrateError is a 400
// that does not wrap ErrorBadRequest).
func gone(err error) bool {
	_, moved := migratedTo(err)
	return badRequest(err) || moved
}

// httpClient gives a file upload (sendDocument, editMessageMedia)
// uploadTimeout and every other request pollTimeout (the library's default).
type httpClient struct{ requests, uploads *http.Client }

func newHTTPClient() httpClient {
	return httpClient{requests: &http.Client{Timeout: pollTimeout}, uploads: &http.Client{Timeout: uploadTimeout}}
}

// Do sends r with the client for its method (the last element of the
// library's ".../bot<token>/<method>" URL).
func (h httpClient) Do(r *http.Request) (*http.Response, error) {
	switch path.Base(r.URL.Path) {
	case "sendDocument", "editMessageMedia":
		return h.uploads.Do(r) // #nosec G704 -- the library builds r for api.telegram.org (a local fake only in tests); no message picks the host
	}
	return h.requests.Do(r) // #nosec G704 -- as above
}

// fileRefused: Telegram will never take this file, a 400 (an empty file) or
// a 413 (over its upload limit; the library has no error kind for it, so the
// code is read from its "<method>, <code> <description>" message).
func fileRefused(err error) bool {
	return badRequest(err) || strings.Contains(err.Error(), ", 413 ")
}

package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/store"
)

// adminCommand is one admin command (updates.go command runs them).
type adminCommand struct {
	name string // without the "/"
	args string // what follows it ("" for none)
	does string
}

// adminCommands is the one list of commands, shown three ways: the reply to
// an unknown command, the message pinned at the top of the admin chat, and
// the bot's command menu in that chat (typing "/" shows it).
var adminCommands = []adminCommand{
	{"status", "", "what the bot covers in each community, pauses, config version and timers"},
	{"pause", "", "stop every action, deletes included, until /resume"},
	{"resume", "", "end every pause; queued actions are checked again before they run"},
	{"reload", "", "load the config again (a rejected config leaves the old one running)"},
	{"join", "<invite link>", "join the group the link points to (only in a configured community)"},
	{"ban", "", "send as a reply to a \"would have acted\" report: delete that post, remove and ban its sender"},
	{"unban", "<report number>", "unban the person in that report everywhere"},
}

// commandList is the list as text: the pinned message, and the reply to a
// command the bot does not know.
func commandList() string {
	var b strings.Builder
	b.WriteString("groupwarden commands (admins of this chat only; type / to pick one):")
	for _, cmd := range adminCommands {
		b.WriteString("\n/" + cmd.name)
		if cmd.args != "" {
			b.WriteString(" " + cmd.args)
		}
		b.WriteString(" — " + cmd.does)
	}
	return b.String()
}

// menu is the list as the bot's command menu.
func menu() []models.BotCommand {
	out := make([]models.BotCommand, 0, len(adminCommands))
	for _, cmd := range adminCommands {
		does := cmd.does
		if cmd.args != "" {
			does = cmd.args + ": " + does
		}
		out = append(out, models.BotCommand{Command: cmd.name, Description: does})
	}
	return out
}

// pinRetryDefault: while Telegram refuses the pin, try again this often (an
// admin may have granted the right since).
const pinRetryDefault = time.Hour

// The pinned list's state in the store (StatusTelegramPinState).
const (
	pinPinned  = "pinned"
	pinRefused = "refused"
)

// errPinRefused: the list is posted, but Telegram would not pin it.
var errPinRefused = errors.New("telegram refused to pin the command list")

// setupLoop keeps the command menu and the pinned list in place: at start,
// when the chat moves to a new ID, and every pinRetry while the pin is
// refused. A request that failed otherwise (Telegram unreachable) is tried
// again after idlePoll.
func (c *Chat) setupLoop(ctx context.Context) {
	for {
		var wait time.Duration
		err := c.Setup(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errPinRefused):
			wait = c.pinRetry
		case err != nil:
			c.Log.Warn("could not set up the command list; retrying", "err", mask.IDs(err.Error()))
			wait = idlePoll
		}
		var retry <-chan time.Time
		var t *time.Timer
		if wait > 0 {
			t = time.NewTimer(wait)
			retry = t.C
		}
		select {
		case <-ctx.Done():
		case <-c.setupWake:
		case <-retry:
		}
		if t != nil {
			t.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// wakeSetup asks setupLoop to run again (the chat moved).
func (c *Chat) wakeSetup() {
	select {
	case c.setupWake <- struct{}{}:
	default:
	}
}

// Setup sets the command menu for the admin chat and makes sure the command
// list is posted, current and pinned there. It returns errPinRefused when the
// list is posted but Telegram would not pin it.
func (c *Chat) Setup(ctx context.Context) error {
	if err := c.settle(ctx); err != nil {
		return err // the list's own record may be among them
	}
	chatID := c.ChatID()
	if err := c.setMenu(ctx, chatID); err != nil {
		return err
	}
	return c.pinList(ctx, chatID)
}

// setMenu makes typing "/" in the admin chat show the commands. It is set
// for that chat only, so nobody else sees them.
func (c *Chat) setMenu(ctx context.Context, chatID int64) error {
	c.mu.Lock()
	done := c.menuFor == chatID
	c.mu.Unlock()
	if done {
		return nil
	}
	err := c.call(ctx, false, func(ctx context.Context, chatID int64) error {
		_, err := c.api.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: menu(),
			Scope: &models.BotCommandScopeChat{ChatID: chatID}})
		return err
	})
	switch {
	case badRequest(err):
		// Telegram will never take this list; the pinned message still shows it.
		c.Log.Error("telegram refused the command menu", "err", err.Error())
	case err != nil:
		return fmt.Errorf("set the command menu: %w", err)
	default:
		c.Log.Info("command menu set for the admin chat", "commands", len(adminCommands))
	}
	c.mu.Lock()
	c.menuFor = chatID
	c.mu.Unlock()
	return nil
}

// pinList posts the command list once and pins it (without a notification).
// A later start edits that message when the list changed, and posts a new
// one only when that message is gone or the chat moved. An admin who deletes
// the list gets it back the next time the list changes.
func (c *Chat) pinList(ctx context.Context, chatID int64) error {
	text := commandList()
	sum := sha256.Sum256([]byte(text))
	hash := hex.EncodeToString(sum[:8])
	st, err := c.Store.Status(ctx)
	if err != nil {
		return err
	}
	pinChat, msgID := parsePin(st[store.StatusTelegramPin].Value)
	state := st[store.StatusTelegramPinState].Value
	if pinChat != chatID {
		msgID = 0
	}
	if msgID != 0 && st[store.StatusTelegramPinHash].Value != hash {
		err := c.call(ctx, false, func(ctx context.Context, chatID int64) error {
			_, err := c.api.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: chatID, MessageID: msgID, Text: text,
				LinkPreviewOptions: noPreview()})
			return err
		})
		switch {
		case badRequest(err):
			c.Log.Warn("the pinned command list is gone; posting it again", "message", msgID, "err", err.Error())
			msgID = 0
		case err != nil:
			return fmt.Errorf("update the pinned command list: %w", err)
		default:
			c.Log.Info("pinned command list updated", "message", msgID)
			if err := c.record(ctx, "command list update", func(ctx context.Context) error {
				return c.Store.SetStatus(ctx, map[string]string{store.StatusTelegramPinHash: hash})
			}); err != nil {
				return err
			}
		}
	}
	if msgID == 0 {
		msg, err := c.sendText(ctx, false, text, quote{}, 0, nil)
		if err != nil {
			return fmt.Errorf("post the command list: %w", err)
		}
		chatID, msgID, state = msg.Chat.ID, msg.ID, ""
		kv := map[string]string{
			store.StatusTelegramPin:      strconv.FormatInt(chatID, 10) + ":" + strconv.Itoa(msgID),
			store.StatusTelegramPinHash:  hash,
			store.StatusTelegramPinState: state,
		}
		if err := c.record(ctx, "command list post", func(ctx context.Context) error {
			return c.Store.SetStatus(ctx, kv)
		}); err != nil {
			return err
		}
		c.Log.Info("command list posted", "message", msgID)
	}
	if state == pinPinned {
		return nil
	}
	return c.pin(ctx, msgID, state)
}

// pin pins message msgID. A refusal (the bot lacks the "Pin messages" right)
// is reported once per refusal episode; the list stays posted.
func (c *Chat) pin(ctx context.Context, msgID int, state string) error {
	err := c.call(ctx, false, func(ctx context.Context, chatID int64) error {
		_, err := c.api.PinChatMessage(ctx, &bot.PinChatMessageParams{ChatID: chatID, MessageID: msgID,
			DisableNotification: true})
		return err
	})
	switch {
	case err == nil:
		c.Log.Info("command list pinned", "message", msgID)
		return c.record(ctx, "command list pin", func(ctx context.Context) error {
			return c.Store.SetStatus(ctx, map[string]string{store.StatusTelegramPinState: pinPinned})
		})
	case !badRequest(err):
		return fmt.Errorf("pin the command list: %w", err)
	}
	c.Log.Warn("telegram refused to pin the command list; the bot needs the \"Pin messages\" admin right",
		"message", msgID, "err", err.Error(), "reported_before", state == pinRefused)
	if state != pinRefused {
		if aerr := c.Alert(ctx, alert.Alert{Kind: alert.PinRefused, Text: "The list of commands is posted, but " +
			"Telegram would not pin it (" + err.Error() + "). Give the bot the \"Pin messages\" admin right in this " +
			"chat; the bot tries again every hour."}); aerr != nil {
			return aerr
		}
		if serr := c.record(ctx, "pin refusal report", func(ctx context.Context) error {
			return c.Store.SetStatus(ctx, map[string]string{store.StatusTelegramPinState: pinRefused})
		}); serr != nil {
			return serr
		}
	}
	return errPinRefused
}

// parsePin reads "<chat ID>:<message ID>" (zeros when unset or unreadable).
func parsePin(v string) (int64, int) {
	chat, msg, ok := strings.Cut(v, ":")
	if !ok {
		return 0, 0
	}
	chatID, err1 := strconv.ParseInt(chat, 10, 64)
	msgID, err2 := strconv.Atoi(msg)
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return chatID, msgID
}

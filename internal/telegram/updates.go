package telegram

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/store"
)

// buttonCodes are the report buttons' callback codes ("<code>:<report ID>").
var buttonCodes = map[string]string{
	ledger.ButtonUndo:           "undo",
	ledger.ButtonBan:            "ban",
	ledger.ButtonResume:         "resume",
	ledger.ButtonAddToBanList:   "addban",
	ledger.ButtonNo:             "no",
	ledger.ButtonShowAttachment: "show",
	ledger.ButtonDone:           "done",
}

// buttonNames maps a callback code back to its button.
var buttonNames = func() map[string]string {
	out := make(map[string]string, len(buttonCodes))
	for name, code := range buttonCodes {
		out[code] = name
	}
	return out
}()

// answerMax is the longest text a button press is answered with (Telegram's
// limit is 200 characters); the full reply goes in the chat.
const answerMax = 190

// handle takes one update. Only the configured chat counts; direct messages
// and other chats are ignored.
func (c *Chat) handle(ctx context.Context, _ *bot.Bot, u *models.Update) {
	switch {
	case u.CallbackQuery != nil:
		c.press(ctx, u.CallbackQuery)
	case u.Message != nil:
		c.message(ctx, u.Message)
	}
}

// message handles a message: the group moving to a new ID, or a command.
func (c *Chat) message(ctx context.Context, m *models.Message) {
	if m.Chat.ID != c.ChatID() {
		c.Log.Info("ignored a message from another chat", "chat", m.Chat.ID, "type", string(m.Chat.Type))
		return
	}
	if m.MigrateToChatID != 0 {
		c.migrate(ctx, m.MigrateToChatID)
		return
	}
	cmd, args, ok := parseCommand(m.Text)
	if !ok || m.From == nil {
		return // admins talking among themselves
	}
	if !c.isAdmin(ctx, m.From.ID) {
		c.Log.Warn("command from a non-admin ignored", "user", m.From.ID, "command", cmd)
		c.reply(ctx, m.ID, 0, "Only admins of this chat can use groupwarden commands.")
		return
	}
	by := actorOf(*m.From)
	c.Log.Info("admin command", "command", cmd, "by", by.String())
	text, reportID := c.command(ctx, cmd, args, m, by)
	c.reply(ctx, m.ID, reportID, text)
}

// parseCommand splits "/cmd@botname args" into "cmd" and "args".
func parseCommand(text string) (cmd, args string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	head, rest, _ := strings.Cut(text[1:], " ")
	head, _, _ = strings.Cut(head, "@")
	return strings.ToLower(head), strings.TrimSpace(rest), head != ""
}

// command runs one admin command and returns the reply and the report it is
// about (0 when none).
func (c *Chat) command(ctx context.Context, cmd, args string, m *models.Message, by Actor) (string, int64) {
	call := func(f func() (string, error)) string {
		text, err := f()
		if err != nil {
			c.Log.Error("admin command failed", "command", cmd, "by", by.String(), "err", err.Error())
			return "Could not do that: " + err.Error()
		}
		return text
	}
	switch cmd {
	case "status":
		return call(func() (string, error) { return c.Controls.Status(ctx) }), 0
	case "pause":
		return call(func() (string, error) { return c.Controls.Pause(ctx, by) }), 0
	case "resume":
		return call(func() (string, error) { return c.Controls.Resume(ctx, by) }), 0
	case "reload":
		return call(func() (string, error) { return c.Controls.Reload(ctx, by) }), 0
	case "join":
		if args == "" {
			return "Usage: /join <invite link>", 0
		}
		return call(func() (string, error) { return c.Controls.Join(ctx, args, by) }), 0
	case "ban":
		if m.ReplyToMessage == nil {
			return "Send /ban as a reply to a report.", 0
		}
		id, ok, err := c.Store.ReportOfTGMessage(ctx, m.Chat.ID, m.ReplyToMessage.ID)
		if err != nil {
			c.Log.Error("read the report a /ban replies to", "message", m.ReplyToMessage.ID, "err", err)
			return "Could not read the report store; try again.", 0
		}
		if !ok {
			return "That message is not a report I posted.", 0
		}
		return c.act(ctx, id, ledger.ButtonBan, by, false), id
	case "unban":
		id, err := strconv.ParseInt(strings.TrimPrefix(args, "#"), 10, 64)
		if err != nil {
			return "Usage: /unban <report number> (the #number in a report or digest).", 0
		}
		return c.act(ctx, id, ledger.ButtonUndo, by, false), id
	}
	return commandList(), 0
}

// press handles a button press on a report.
func (c *Chat) press(ctx context.Context, q *models.CallbackQuery) {
	chatID, msgID := pressedIn(q.Message)
	if chatID != c.ChatID() {
		c.Log.Info("ignored a button press from another chat", "chat", chatID)
		c.answer(ctx, q.ID, "This chat is not the groupwarden admin chat.")
		return
	}
	if !c.isAdmin(ctx, q.From.ID) {
		c.Log.Warn("button press from a non-admin refused", "user", q.From.ID, "data", q.Data)
		c.answer(ctx, q.ID, "Only admins of this chat can press these buttons.")
		return
	}
	code, rawID, _ := strings.Cut(q.Data, ":")
	button, known := buttonNames[code]
	id, err := strconv.ParseInt(rawID, 10, 64)
	if !known || err != nil {
		c.Log.Warn("unknown button data", "data", q.Data)
		c.answer(ctx, q.ID, "Unknown button.")
		return
	}
	by := actorOf(q.From)
	c.Log.Info("admin pressed a button", "report", id, "button", button, "by", by.String())
	text := c.act(ctx, id, button, by, true)
	c.answer(ctx, q.ID, text)
	c.reply(ctx, msgID, id, text)
}

// pressedIn is the chat and message a pressed button was on.
func pressedIn(m models.MaybeInaccessibleMessage) (int64, int) {
	switch {
	case m.Message != nil:
		return m.Message.Chat.ID, m.Message.ID
	case m.InaccessibleMessage != nil:
		return m.InaccessibleMessage.Chat.ID, m.InaccessibleMessage.MessageID
	}
	return 0, 0
}

// act does what a button (or its command) asks for report reportID. Every
// press except [Show attachment] is claimed first, keyed report + button: a
// repeat is answered with the first press, never acted on twice. A press
// whose action failed is released so it can be tried again. viaButton: the
// report must carry that button.
func (c *Chat) act(ctx context.Context, reportID int64, button string, by Actor, viaButton bool) string {
	r, ok, err := c.Store.Report(ctx, reportID)
	if err != nil {
		c.Log.Error("read report", "report", reportID, "err", err)
		return "Could not read report #" + strconv.FormatInt(reportID, 10) + "."
	}
	if !ok {
		return fmt.Sprintf("There is no report #%d (it may have been purged).", reportID)
	}
	if viaButton && !slices.Contains(r.Buttons, button) {
		c.Log.Warn("button not on the report", "report", reportID, "button", button, "by", by.String())
		return fmt.Sprintf("Report #%d has no [%s] button.", reportID, button)
	}
	if button == ledger.ButtonShowAttachment {
		return c.show(ctx, r)
	}
	first, claimed, err := c.Store.ClaimPress(ctx, store.Press{ReportID: reportID, Button: button, UserID: by.ID,
		UserName: by.Name})
	if err != nil {
		c.Log.Error("record the press", "report", reportID, "button", button, "err", err)
		return "Could not record the press; try again."
	}
	if !claimed {
		return fmt.Sprintf("[%s] on #%d was already done by %s at %s: %s", button, reportID, first.UserName,
			first.CreatedAt.UTC().Format("2006-01-02 15:04 MST"), first.Result)
	}
	result, err := c.do(ctx, r, button, by)
	if err != nil {
		c.Log.Error("admin action failed", "report", reportID, "button", button, "by", by.String(), "err", err.Error())
		if rerr := c.Store.ReleasePress(ctx, reportID, button); rerr != nil {
			c.Log.Error("release the press", "report", reportID, "button", button, "err", rerr)
		}
		return "Could not do that: " + err.Error()
	}
	if err := c.Store.SetPressResult(ctx, reportID, button, result); err != nil {
		c.Log.Error("record the press result", "report", reportID, "button", button, "err", err)
	}
	c.Log.Info("admin action done", "report", reportID, "button", button, "by", by.String())
	return result
}

// do runs a claimed press.
func (c *Chat) do(ctx context.Context, r store.Report, button string, by Actor) (string, error) {
	switch button {
	case ledger.ButtonUndo:
		return c.Controls.Undo(ctx, r.ID, by)
	case ledger.ButtonBan:
		return c.Controls.Ban(ctx, r.ID, by)
	case ledger.ButtonAddToBanList:
		return c.Controls.AddToBanList(ctx, r.ID, by)
	case ledger.ButtonNo:
		return "Not added to the ban list (" + by.Name + ").", nil
	case ledger.ButtonResume:
		return c.Controls.Resume(ctx, by)
	case ledger.ButtonDone:
		now := c.Now()
		if err := c.Store.SetStatus(ctx, map[string]string{
			store.StatusPhoneDone: strconv.FormatInt(now.UnixMilli(), 10)}); err != nil {
			return "", err
		}
		return "Thanks, " + by.Name + ": the next reminder to open WhatsApp on the bot phone is in 7 days.", nil
	}
	return "", fmt.Errorf("no action for [%s]", button)
}

// show posts a report's attachment again while the evidence copy is kept.
func (c *Chat) show(ctx context.Context, r store.Report) string {
	if r.EvidenceID == 0 {
		return "This report has no attachment."
	}
	ev, ok, err := c.Store.Evidence(ctx, r.EvidenceID)
	if err != nil {
		c.Log.Error("read evidence", "report", r.ID, "err", err)
		return "Could not read the evidence copy."
	}
	if !ok || ev.MediaState != store.MediaSaved {
		return "The attachment is no longer kept (the evidence copy was purged, forgotten or never saved)."
	}
	posted, err := c.showAttachment(ctx, r, ev)
	switch {
	case err != nil:
		c.Log.Error("post the attachment again", "report", r.ID, "err", err.Error())
		return "Could not post the attachment: " + err.Error()
	case !posted:
		return "The attachment is no longer kept (its file is gone)."
	}
	return "Attachment posted again; it is taken down after " +
		strconv.Itoa(c.Config.Current().Config.Report.AttachmentShowHours) + " hours."
}

// isAdmin asks Telegram, at press time, whether userID is an admin of the
// admin chat. An error counts as no (fail closed).
func (c *Chat) isAdmin(ctx context.Context, userID int64) bool {
	m, err := c.api.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: c.ChatID(), UserID: userID})
	if err != nil {
		c.Log.Warn("could not check an admin-chat member; treating them as not an admin", "user", userID,
			"err", c.redact(err).Error())
		return false
	}
	return m.Type == models.ChatMemberTypeOwner || m.Type == models.ChatMemberTypeAdministrator
}

// answer answers a button press (Telegram shows the text to the presser),
// identifiers masked as in reply.
func (c *Chat) answer(ctx context.Context, queryID, text string) {
	text = mask.IDs(text)
	if r := []rune(text); len(r) > answerMax {
		text = string(r[:answerMax-1]) + "…"
	}
	if _, err := c.api.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: queryID,
		Text: text}); err != nil {
		c.Log.Warn("could not answer a button press", "err", c.redact(err).Error())
	}
}

// reply posts text in the admin chat as a reply to message replyTo. The text is
// masked here, once for every reply: a reply can carry an error or a config
// rejection that names a member or a community by its ID.
func (c *Chat) reply(ctx context.Context, replyTo int, reportID int64, text string) {
	parts, _ := split(mask.IDs(text), "")
	for _, part := range parts {
		msg, err := c.sendText(ctx, true, part, quote{}, replyTo, nil)
		if err != nil {
			c.Log.Error("could not reply in the admin chat", "report", reportID, "err", err.Error())
			return
		}
		if err := c.Store.AddTGMessage(ctx, store.TGMessage{ReportID: reportID, Role: store.RoleReply,
			ChatID: msg.Chat.ID, MessageID: msg.ID, SentAt: c.Now()}); err != nil {
			c.Log.Error("record a reply", "err", err)
		}
	}
}

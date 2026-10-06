package telegram

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
)

// pendingWindow is how many undelivered reports one step looks at (enough to
// fill a digest).
const pendingWindow = digestAfter + digestMax + 1

// placeholderName is the file an attachment post becomes when Telegram no
// longer lets the bot delete it (after 48 hours).
const placeholderName = "attachment-removed.txt"

// deliverLoop runs Step until ctx ends: straight on while there is work,
// otherwise until woken, until a retry_after ends, or every idlePoll.
func (c *Chat) deliverLoop(ctx context.Context) {
	for {
		did, err := c.Step(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.Log.Warn("telegram delivery failed; retrying", "err", mask.IDs(err.Error()))
		}
		if did && err == nil {
			continue
		}
		wait := idlePoll
		c.mu.Lock()
		if d := c.blockedUntil.Sub(c.Now()); d > 0 && d < wait {
			wait = d
		}
		c.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-c.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// Step does one piece of work, most urgent first: a priority report, a
// queued edit (`member forget`), a routine report (or a digest of them when
// backlogged), an attachment to post or take down, a message whose text is
// due to be removed, the daily summary. It reports whether it did any.
func (c *Chat) Step(ctx context.Context) (bool, error) {
	pending, err := c.Store.UnsentReportsExcept(ctx, ledger.KindLog, pendingWindow)
	if err != nil {
		return false, err
	}
	if len(pending) > 0 && pending[0].Priority {
		return c.deliverReport(ctx, pending[0].ID)
	}
	if did, err := c.reportEdit(ctx); did || err != nil {
		return did, err
	}
	if len(pending) > 0 {
		backlog, err := c.Store.CountUnsent(ctx, ledger.KindLog)
		if err != nil {
			return false, err
		}
		if backlog > digestAfter {
			var group []store.Report
			for _, r := range pending {
				if digestible(r) && len(group) < digestMax {
					group = append(group, r)
				}
			}
			if len(group) > 1 {
				return c.digest(ctx, group)
			}
		}
		return c.deliverReport(ctx, pending[0].ID)
	}
	for _, work := range []func(context.Context) (bool, error){c.postAttachment, c.takeDownDue, c.stripDue, c.summary} {
		if did, err := work(ctx); did || err != nil {
			return did, err
		}
	}
	return false, nil
}

// Flush delivers every undelivered priority report now (the process is about
// to exit). It gives up when ctx ends.
func (c *Chat) Flush(ctx context.Context) error {
	for {
		reps, err := c.Store.UnsentReportsExcept(ctx, ledger.KindLog, 1)
		if err != nil {
			return err
		}
		if len(reps) == 0 || !reps[0].Priority {
			return nil
		}
		if _, err := c.deliverReport(ctx, reps[0].ID); err != nil {
			if _, wait := retryAfter(err); wait && ctx.Err() == nil {
				continue // the next turn waits out retry_after, within ctx
			}
			return err
		}
	}
}

// digestible: a report that may be combined into a digest. It carries no
// button (a digest would lose it, and [Undo], [Ban], [Add to ban list], [No],
// [Resume] and [Done] must keep working), no member's message (each keeps its
// full text, attachment and later removal of that text), and is not about a
// removal or a ban. That is a superset of what AC 4.1 excludes (remove/ban
// reports and reports carrying [Ban]) because a button lost in a digest is
// lost for good.
func digestible(r store.Report) bool {
	if r.Priority || len(r.Buttons) > 0 || r.EvidenceID != 0 {
		return false
	}
	switch r.Kind {
	case ledger.KindAction, ledger.KindBannedRejoin, ledger.KindBanCLI, ledger.KindBanLifted, ledger.KindUnknownActor:
		return false
	}
	return true
}

// deliverReport posts one report: the report message (with its buttons),
// then any follow-ups the member's text did not fit in, as replies. Parts
// already posted by an earlier attempt are not posted again.
func (c *Chat) deliverReport(ctx context.Context, id int64) (bool, error) {
	if err := c.lock(ctx); err != nil {
		return false, err
	}
	defer c.unlock()
	r, ok, err := c.Store.Report(ctx, id)
	if err != nil || !ok || !r.SentAt.IsZero() {
		return false, err
	}
	var ev *store.Evidence
	if r.EvidenceID != 0 {
		e, ok, err := c.Store.Evidence(ctx, r.EvidenceID)
		if err != nil {
			return false, err
		}
		if ok {
			ev = &e
		}
	}
	r.Text = c.labelText(r.Text)
	group := ""
	if ev != nil && c.Groups != nil {
		group = c.Groups.GroupName(ev.Chat)
	}
	out := render(r, ev, group)
	posted, err := c.Store.TGMessagesFor(ctx, r.ID)
	if err != nil {
		return false, err
	}
	head, done := 0, 0
	for _, m := range posted {
		switch m.Role {
		case store.RoleReport:
			head = m.MessageID
			done++
		case store.RoleFollowup:
			done++
		}
	}
	for i := done; i < len(out.parts); i++ {
		role, markup, replyTo := store.RoleFollowup, models.ReplyMarkup(nil), head
		if i == 0 {
			role, markup, replyTo = store.RoleReport, keyboard(r), 0
		}
		msg, err := c.sendText(ctx, r.Priority, out.parts[i], replyTo, markup)
		if err != nil {
			if badRequest(err) {
				// Telegram will never take this message: record why and go
				// on, so one bad report never holds up the rest.
				c.Log.Error("telegram refused a report; skipping it", "report", r.ID, "kind", r.Kind, "part", i,
					"err", err.Error())
				return true, c.Store.MarkReportSent(ctx, r.ID)
			}
			return false, err
		}
		if i == 0 {
			head = msg.ID
		}
		if err := c.Store.AddTGMessage(ctx, store.TGMessage{ReportID: r.ID, Role: role, ChatID: msg.Chat.ID,
			MessageID: msg.ID, Stripped: out.stripped[i], SentAt: c.Now()}); err != nil {
			return false, err
		}
	}
	if err := c.Store.MarkReportSent(ctx, r.ID); err != nil {
		return false, err
	}
	c.Log.Info("report delivered", "report", r.ID, "kind", r.Kind, "priority", r.Priority, "parts", len(out.parts))
	return true, nil
}

// digest posts several routine reports as one message.
func (c *Chat) digest(ctx context.Context, reps []store.Report) (bool, error) {
	if err := c.lock(ctx); err != nil {
		return false, err
	}
	defer c.unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "%d reports while the chat was busy:", len(reps))
	ids := make([]int64, 0, len(reps))
	for _, r := range reps {
		line := fmt.Sprintf("\n\n#%d %s", r.ID, c.labelText(r.Text))
		if units(b.String()+line) > maxUnits {
			break // the rest go in the next digest
		}
		b.WriteString(line)
		ids = append(ids, r.ID)
	}
	msg, err := c.sendText(ctx, false, b.String(), 0, nil)
	if err != nil {
		return false, err
	}
	if err := c.Store.AddTGMessage(ctx, store.TGMessage{ReportID: ids[0], Role: store.RoleSummary, ChatID: msg.Chat.ID,
		MessageID: msg.ID, SentAt: c.Now()}); err != nil {
		return false, err
	}
	if err := c.Store.MarkReportsSent(ctx, ids); err != nil {
		return false, err
	}
	c.Log.Info("digest delivered", "reports", len(ids))
	return true, nil
}

// reportEdit applies one queued edit from `member forget`: remove the
// member's text from every message of the report, or take down its
// attachment posts.
func (c *Chat) reportEdit(ctx context.Context) (bool, error) {
	edits, err := c.Store.ReportEdits(ctx)
	if err != nil || len(edits) == 0 {
		return false, err
	}
	e := edits[0]
	msgs, err := c.Store.TGMessagesFor(ctx, e.ReportID)
	if err != nil {
		return false, err
	}
	for _, m := range msgs {
		if !m.GoneAt.IsZero() {
			continue
		}
		switch {
		case e.Op == store.EditStripText && m.Stripped != "" && m.StrippedAt.IsZero():
			err = c.strip(ctx, m)
		case e.Op == store.EditDeleteAttachment && m.Role == store.RoleAttachment:
			err = c.takeDown(ctx, m)
		}
		if err != nil {
			return false, err
		}
	}
	if err := c.Store.DeleteReportEdit(ctx, e.ReportID, e.Op); err != nil {
		return false, err
	}
	c.Log.Info("report edit applied", "report", e.ReportID, "op", e.Op)
	return true, nil
}

// postAttachment posts the saved attachment of a delivered action report as
// a reply to it.
func (c *Chat) postAttachment(ctx context.Context) (bool, error) {
	due, err := c.Store.AttachmentsToPost(ctx, ledger.KindAction, 1)
	if err != nil || len(due) == 0 {
		return false, err
	}
	_, err = c.showAttachment(ctx, due[0].Report, due[0].Evidence)
	return true, err
}

// showAttachment posts ev's saved file as a reply to report r. ok is false
// when the file is no longer there (the evidence copy was purged).
func (c *Chat) showAttachment(ctx context.Context, r store.Report, ev store.Evidence) (bool, error) {
	if err := c.lock(ctx); err != nil {
		return false, err
	}
	defer c.unlock()
	f, err := os.Open(ev.MediaPath) // #nosec G304 -- a path the attachment fetcher wrote under data_dir
	if err != nil {
		c.Log.Error("the saved attachment is missing", "report", r.ID, "evidence", ev.ID, "err", err)
		return false, c.Store.SetMedia(ctx, ev.ID, store.MediaFailed, "", "the saved file is missing: "+err.Error())
	}
	defer f.Close()
	head, err := c.reportMessage(ctx, r.ID)
	if err != nil {
		return false, err
	}
	name := ev.MediaName
	if name == "" {
		name = "attachment" + strings.ToLower(extOf(ev.MediaPath))
	}
	var msg *models.Message
	err = c.call(ctx, false, func(ctx context.Context, chatID int64) error {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		var err error
		msg, err = c.api.SendDocument(ctx, &bot.SendDocumentParams{ChatID: chatID,
			Document:        &models.InputFileUpload{Filename: name, Data: f},
			ReplyParameters: replyToParams(head)})
		return err
	})
	if err != nil {
		return false, err
	}
	if err := c.Store.AddTGMessage(ctx, store.TGMessage{ReportID: r.ID, Role: store.RoleAttachment, ChatID: msg.Chat.ID,
		MessageID: msg.ID, SentAt: c.Now()}); err != nil {
		return false, err
	}
	c.Log.Info("attachment posted", "report", r.ID, "evidence", ev.ID)
	return true, nil
}

func extOf(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 && !strings.Contains(path[i:], "/") {
		return path[i:]
	}
	return ""
}

// takeDownDue takes down one attachment post older than
// report.attachment_show_hours.
func (c *Chat) takeDownDue(ctx context.Context) (bool, error) {
	hours := c.Config.Current().Config.Report.AttachmentShowHours
	due, err := c.Store.AttachmentsShownSince(ctx, c.Now().Add(-time.Duration(hours)*time.Hour))
	if err != nil || len(due) == 0 {
		return false, err
	}
	if err := c.lock(ctx); err != nil {
		return false, err
	}
	defer c.unlock()
	return true, c.takeDown(ctx, due[0])
}

// takeDown deletes an attachment post. Telegram lets a bot delete its own
// messages only within 48 hours; past that the post is replaced by a
// placeholder file instead. Called with the delivery slot held.
func (c *Chat) takeDown(ctx context.Context, m store.TGMessage) error {
	err := c.call(ctx, false, func(ctx context.Context, _ int64) error {
		_, err := c.api.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: m.ChatID, MessageID: m.MessageID})
		return err
	})
	if err == nil {
		c.Log.Info("attachment post deleted", "report", m.ReportID, "message", m.MessageID)
		return c.Store.MarkTGGone(ctx, m.ID)
	}
	if !badRequest(err) {
		return err
	}
	c.Log.Warn("attachment post could not be deleted; replacing it with a placeholder", "report", m.ReportID,
		"message", m.MessageID, "err", err.Error())
	hours := c.Config.Current().Config.Report.AttachmentShowHours
	note := fmt.Sprintf("This attachment was taken down after %d hours. [Show attachment] on the report posts it again "+
		"while the evidence copy is kept.\n", hours)
	err = c.call(ctx, false, func(ctx context.Context, _ int64) error {
		_, err := c.api.EditMessageMedia(ctx, &bot.EditMessageMediaParams{ChatID: m.ChatID, MessageID: m.MessageID,
			Media: &models.InputMediaDocument{Media: "attach://" + placeholderName,
				MediaAttachment: strings.NewReader(note)}})
		return err
	})
	if err != nil && !badRequest(err) {
		return err
	}
	if err != nil {
		c.Log.Error("attachment post could be neither deleted nor replaced", "report", m.ReportID,
			"message", m.MessageID, "err", err.Error())
	}
	return c.Store.MarkTGGone(ctx, m.ID)
}

// stripDue removes the member's text from one posted message whose report is
// older than its community's retention.evidence_days.
func (c *Chat) stripDue(ctx context.Context) (bool, error) {
	cfg := c.Config.Current().Config
	shortest := cfg.Retention.EvidenceDays
	for id := range cfg.Communities {
		shortest = min(shortest, cfg.RetentionFor(id).EvidenceDays)
	}
	now := c.Now()
	due, err := c.Store.StripsDue(ctx, now.AddDate(0, 0, -shortest))
	if err != nil {
		return false, err
	}
	for _, m := range due {
		r, ok, err := c.Store.Report(ctx, m.ReportID)
		if err != nil {
			return false, err
		}
		if ok && r.CreatedAt.After(now.AddDate(0, 0, -cfg.RetentionFor(r.Community).EvidenceDays)) {
			continue // this community keeps evidence longer
		}
		if err := c.lock(ctx); err != nil {
			return false, err
		}
		err = c.strip(ctx, m)
		c.unlock()
		return true, err
	}
	return false, nil
}

// strip edits a posted message to its text without the member's message,
// keeping a report's buttons. Called with the delivery slot held.
func (c *Chat) strip(ctx context.Context, m store.TGMessage) error {
	var markup models.ReplyMarkup
	if m.Role == store.RoleReport {
		r, ok, err := c.Store.Report(ctx, m.ReportID)
		if err != nil {
			return err
		}
		if ok {
			markup = keyboard(r)
		}
	}
	err := c.call(ctx, false, func(ctx context.Context, _ int64) error {
		_, err := c.api.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: m.ChatID, MessageID: m.MessageID,
			Text: m.Stripped, LinkPreviewOptions: noPreview(), ReplyMarkup: markup})
		return err
	})
	if err != nil && !badRequest(err) {
		return err
	}
	if err != nil {
		c.Log.Warn("message text could not be removed (message gone or unchanged)", "report", m.ReportID,
			"message", m.MessageID, "err", err.Error())
	} else {
		c.Log.Info("member text removed from a report", "report", m.ReportID, "message", m.MessageID, "role", m.Role)
	}
	return c.Store.MarkTGStripped(ctx, m.ID)
}

// summary sends the daily summary once a UTC day has ended: keyword-only
// posts no rule acted on, log-rule matches, and the day's other counters.
// Log-rule reports are never sent on their own.
func (c *Chat) summary(ctx context.Context) (bool, error) {
	st, err := c.Store.Status(ctx)
	if err != nil {
		return false, err
	}
	now := c.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	last, err := time.Parse(time.DateOnly, st[store.StatusSummaryDay].Value)
	if err != nil {
		// First run (or an unreadable value): the summaries start with today.
		return false, c.Store.SetStatus(ctx, map[string]string{store.StatusSummaryDay: today.AddDate(0, 0, -1).Format(time.DateOnly)})
	}
	if !last.AddDate(0, 0, 1).Before(today) {
		return false, nil
	}
	logs, err := c.Store.UnsentReportsOf(ctx, ledger.KindLog, 10000)
	if err != nil {
		return false, err
	}
	var b strings.Builder
	var ids []int64
	for d := last.AddDate(0, 0, 1); d.Before(today); d = d.AddDate(0, 0, 1) {
		counters, err := c.Store.Counters(ctx, d.Format(time.DateOnly))
		if err != nil {
			return false, err
		}
		matches := map[string]int{}
		for _, r := range logs {
			if !r.CreatedAt.Before(d) && r.CreatedAt.Before(d.AddDate(0, 0, 1)) {
				matches[c.labelText(r.Text)]++
				ids = append(ids, r.ID)
			}
		}
		if lines := c.summaryLines(counters, matches); len(lines) > 0 {
			fmt.Fprintf(&b, "Daily summary for %s (UTC):\n%s\n\n", d.Format(time.DateOnly), strings.Join(lines, "\n"))
		}
	}
	sent := false
	if text := strings.TrimSpace(b.String()); text != "" {
		if err := c.lock(ctx); err != nil {
			return false, err
		}
		for _, part := range split(text, "") {
			msg, err := c.sendText(ctx, false, part, 0, nil)
			if err != nil {
				c.unlock()
				return false, err
			}
			if err := c.Store.AddTGMessage(ctx, store.TGMessage{Role: store.RoleSummary, ChatID: msg.Chat.ID,
				MessageID: msg.ID, SentAt: c.Now()}); err != nil {
				c.unlock()
				return false, err
			}
		}
		c.unlock()
		sent = true
	}
	if err := c.Store.MarkReportsSent(ctx, ids); err != nil {
		return false, err
	}
	c.Log.Info("daily summary", "through", today.AddDate(0, 0, -1).Format(time.DateOnly), "sent", sent,
		"log_reports", len(ids))
	return sent, c.Store.SetStatus(ctx, map[string]string{store.StatusSummaryDay: today.AddDate(0, 0, -1).Format(time.DateOnly)})
}

// summaryLines turns one day's counters and log-rule matches into lines; a
// counter the summary does not know is listed by name.
func (c *Chat) summaryLines(counters map[string]int, matches map[string]int) []string {
	keyword := map[string][]string{}
	var lines, other []string
	for name, n := range counters {
		switch {
		case strings.HasPrefix(name, pipeline.CounterKeywordOnly):
			community, list, _ := strings.Cut(strings.TrimPrefix(name, pipeline.CounterKeywordOnly), "|")
			keyword[community] = append(keyword[community], fmt.Sprintf("%s %d", list, n))
		case name == pipeline.CounterMissingParent:
			other = append(other, fmt.Sprintf("Announcement replies that could not be read (their announcement is too old): %d", n))
		case name == pipeline.CounterInboxUnreadable:
			other = append(other, fmt.Sprintf("Inbox items that could not be read: %d", n))
		default:
			other = append(other, fmt.Sprintf("%s: %d", name, n))
		}
	}
	for community, lists := range keyword {
		sort.Strings(lists)
		lines = append(lines, fmt.Sprintf("Keyword-only posts not acted on in %s: %s", c.labelText(community),
			strings.Join(lists, ", ")))
	}
	for text, n := range matches {
		lines = append(lines, fmt.Sprintf("%s (%d)", text, n))
	}
	sort.Strings(lines)
	sort.Strings(other)
	return append(lines, other...)
}

// labelText names the configured communities in a report's text and masks
// any other WhatsApp ID.
func (c *Chat) labelText(text string) string {
	for id, cm := range c.Config.Current().Config.Communities {
		if cm.Name != "" {
			text = strings.ReplaceAll(text, id, cm.Name)
		}
	}
	return mask.IDs(text)
}

// reportMessage is the message ID of a report's own post (0 when none).
func (c *Chat) reportMessage(ctx context.Context, reportID int64) (int, error) {
	msgs, err := c.Store.TGMessagesFor(ctx, reportID)
	if err != nil {
		return 0, err
	}
	for _, m := range msgs {
		if m.Role == store.RoleReport {
			return m.MessageID, nil
		}
	}
	return 0, nil
}

// sendText posts text (link previews off), as a reply when replyTo is set.
func (c *Chat) sendText(ctx context.Context, priority bool, text string, replyTo int, markup models.ReplyMarkup) (*models.Message, error) {
	var msg *models.Message
	err := c.call(ctx, priority, func(ctx context.Context, chatID int64) error {
		var err error
		msg, err = c.api.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text,
			LinkPreviewOptions: noPreview(), ReplyParameters: replyToParams(replyTo), ReplyMarkup: markup})
		return err
	})
	return msg, err
}

func noPreview() *models.LinkPreviewOptions {
	return &models.LinkPreviewOptions{IsDisabled: bot.True()}
}

// replyToParams makes a message a reply to id (nil for none); it is still
// sent if an admin deleted that message.
func replyToParams(id int) *models.ReplyParameters {
	if id == 0 {
		return nil
	}
	return &models.ReplyParameters{MessageID: id, AllowSendingWithoutReply: true}
}

// keyboard is a report's buttons ("<code>:<report ID>" callback data).
func keyboard(r store.Report) models.ReplyMarkup {
	var row []models.InlineKeyboardButton
	for _, name := range r.Buttons {
		code, ok := buttonCodes[name]
		if !ok {
			continue // every ledger.Button* has a code (TestEveryButtonHasACode)
		}
		row = append(row, models.InlineKeyboardButton{Text: name, CallbackData: code + ":" + strconv.FormatInt(r.ID, 10)})
	}
	if len(row) == 0 {
		return nil
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{row}}
}

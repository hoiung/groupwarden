package telegram

import (
	"context"
	"errors"
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

// owedWrite is a store write recording something already posted.
type owedWrite struct {
	what  string
	write func(context.Context) error
}

// record makes write, the store's record of what was just posted (what
// names it for the log). When the store will not take it, the write is kept
// in memory and settle makes it before anything else is posted: a post
// whose record is missing would otherwise be posted again at every retry (a
// full disk re-posting every pending report every idlePoll). Every such
// write is idempotent, so making it twice is harmless. Lost only if the
// process exits first, which costs one repeated post.
func (c *Chat) record(ctx context.Context, what string, write func(context.Context) error) error {
	err := write(ctx)
	if err == nil {
		return nil
	}
	c.owedMu.Lock()
	c.owed = append(c.owed, owedWrite{what: what, write: write})
	n := len(c.owed)
	c.owedMu.Unlock()
	c.Log.Error("could not record a post in the store; delivery waits until it is recorded", "what", what,
		"owed", n, "err", err)
	return err
}

// settle makes the owed record writes, oldest first, stopping at the first
// the store still refuses.
func (c *Chat) settle(ctx context.Context) error {
	c.owedMu.Lock()
	defer c.owedMu.Unlock()
	for len(c.owed) > 0 {
		w := c.owed[0]
		if err := w.write(ctx); err != nil {
			return fmt.Errorf("record %s (%d owed): %w", w.what, len(c.owed), err)
		}
		c.owed = c.owed[1:]
		c.Log.Info("post recorded after all", "what", w.what, "owed", len(c.owed))
	}
	return nil
}

// Step does one piece of work, most urgent first: a priority report, a
// queued edit (`member forget`), a routine report (or a digest of them when
// backlogged), an attachment to post or take down, a message whose text is
// due to be removed, the daily summary. It reports whether it did any.
// Nothing is posted while a record of an earlier post is still owed.
func (c *Chat) Step(ctx context.Context) (bool, error) {
	if err := c.settle(ctx); err != nil {
		return false, err
	}
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
// to exit). It gives up when ctx ends. An owed record that still fails does
// not hold it back: the last word before exit beats a repeated part.
func (c *Chat) Flush(ctx context.Context) error {
	if err := c.settle(ctx); err != nil {
		c.Log.Error("flushing priority reports with a post's record still owed", "err", err)
	}
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
	ctx, err := c.lock(ctx)
	if err != nil {
		return false, err
	}
	defer c.unlock(ctx)
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
		msg, err := c.sendText(ctx, r.Priority, out.parts[i], out.quotes[i], replyTo, markup)
		if err != nil {
			if badRequest(err) && i > 0 {
				// One follow-up refused: the parts after it still go. (A
				// refused part is not recorded, so a retry after a crash
				// before the report is marked sent posts one part again per
				// refused part; it never loses one.)
				c.Log.Error("telegram refused a follow-up part; skipping it", "report", r.ID, "kind", r.Kind,
					"part", i, "err", err.Error())
				continue
			}
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
		m := store.TGMessage{ReportID: r.ID, Role: role, ChatID: msg.Chat.ID, MessageID: msg.ID,
			Stripped: out.stripped[i], SentAt: c.Now()}
		if err := c.record(ctx, fmt.Sprintf("report %d part %d", r.ID, i), func(ctx context.Context) error {
			return c.Store.AddTGMessage(ctx, m)
		}); err != nil {
			return false, err
		}
	}
	if err := c.Store.MarkReportSent(ctx, r.ID); err != nil {
		return false, err
	}
	c.Log.Info("report delivered", "report", r.ID, "kind", r.Kind, "priority", r.Priority, "parts", len(out.parts))
	return true, nil
}

// digest posts several routine reports as one message. When fewer than two
// fit (the first is too long to share a message), the first goes on its own,
// split into parts like any report.
func (c *Chat) digest(ctx context.Context, reps []store.Report) (bool, error) {
	var lines []string
	ids := make([]int64, 0, len(reps))
	used := units(fmt.Sprintf("%d reports while the chat was busy:", len(reps)))
	for _, r := range reps {
		line := fmt.Sprintf("\n\n#%d %s", r.ID, c.labelText(r.Text))
		if used+units(line) > maxUnits {
			break // the rest go in the next digest
		}
		used += units(line)
		lines = append(lines, line)
		ids = append(ids, r.ID)
	}
	if len(ids) < 2 {
		return c.deliverReport(ctx, reps[0].ID)
	}
	ctx, err := c.lock(ctx)
	if err != nil {
		return false, err
	}
	defer c.unlock(ctx)
	text := fmt.Sprintf("%d reports while the chat was busy:", len(ids)) + strings.Join(lines, "")
	msg, err := c.sendText(ctx, false, text, quote{}, 0, nil)
	if err != nil {
		return false, err
	}
	m := store.TGMessage{ReportID: ids[0], Role: store.RoleSummary, ChatID: msg.Chat.ID, MessageID: msg.ID,
		SentAt: c.Now()}
	// One record for the whole post: a write left out of it would let the
	// digest go out again once the first was made.
	if err := c.record(ctx, fmt.Sprintf("digest of reports %d-%d", ids[0], ids[len(ids)-1]),
		func(ctx context.Context) error {
			if err := c.Store.AddTGMessage(ctx, m); err != nil {
				return err
			}
			return c.Store.MarkReportsSent(ctx, ids)
		}); err != nil {
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
	ctx, err = c.lock(ctx)
	if err != nil {
		return false, err
	}
	defer c.unlock(ctx)
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
	if errors.Is(err, errFileRefused) {
		return true, nil // settled and logged: the rest of the work goes on
	}
	return true, err
}

// errFileRefused: Telegram will never take the file; it is marked failed.
var errFileRefused = errors.New("refused by Telegram")

// showAttachment posts ev's saved file as a reply to report r. ok is false
// when the file is no longer there (the evidence copy was purged).
func (c *Chat) showAttachment(ctx context.Context, r store.Report, ev store.Evidence) (bool, error) {
	ctx, err := c.lock(ctx)
	if err != nil {
		return false, err
	}
	defer c.unlock(ctx)
	f, err := os.Open(ev.MediaPath) // #nosec G304 -- a path the attachment fetcher wrote under data_dir
	if err != nil {
		c.Log.Error("the saved attachment is missing", "report", r.ID, "evidence", ev.ID, "err", err)
		return false, c.Store.SetMedia(ctx, ev.ID, store.MediaFailed, "", "the saved file is missing: "+err.Error(), ev.MediaSize)
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
	if err != nil && fileRefused(err) {
		// Telegram will never take this file (empty, or over its limit):
		// settle it, so it does not block the take-downs, text removals
		// and the daily summary behind it. The path stays, so purge and
		// `member forget` still delete the file.
		c.Log.Error("telegram refused an attachment", "report", r.ID, "evidence", ev.ID, "err", err.Error())
		if err := c.Store.SetMedia(ctx, ev.ID, store.MediaFailed, ev.MediaPath, "Telegram refused the file: "+
			err.Error(), ev.MediaSize); err != nil {
			return false, err
		}
		return false, fmt.Errorf("%w: %s", errFileRefused, err.Error())
	}
	if err != nil {
		return false, err
	}
	m := store.TGMessage{ReportID: r.ID, Role: store.RoleAttachment, ChatID: msg.Chat.ID, MessageID: msg.ID,
		SentAt: c.Now()}
	if err := c.record(ctx, fmt.Sprintf("attachment of report %d", r.ID), func(ctx context.Context) error {
		return c.Store.AddTGMessage(ctx, m)
	}); err != nil {
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
	ctx, err = c.lock(ctx)
	if err != nil {
		return false, err
	}
	defer c.unlock(ctx)
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
		return c.markGone(ctx, m)
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
	return c.markGone(ctx, m)
}

// markGone records that attachment post m is taken down.
func (c *Chat) markGone(ctx context.Context, m store.TGMessage) error {
	return c.record(ctx, fmt.Sprintf("take-down of message %d", m.MessageID), func(ctx context.Context) error {
		return c.Store.MarkTGGone(ctx, m.ID)
	})
}

// stripDue removes the member's text from one posted message whose report is
// older than its community's retention.evidence_days.
func (c *Chat) stripDue(ctx context.Context) (bool, error) {
	cfg := c.Config.Current().Config
	now := c.Now()
	cutoffs := map[string]time.Time{}
	for id := range cfg.Communities {
		cutoffs[id] = now.AddDate(0, 0, -cfg.RetentionFor(id).EvidenceDays)
	}
	due, err := c.Store.StripsDue(ctx, now.AddDate(0, 0, -cfg.Retention.EvidenceDays), cutoffs, 1)
	if err != nil || len(due) == 0 {
		return false, err
	}
	ctx, err = c.lock(ctx)
	if err != nil {
		return false, err
	}
	defer c.unlock(ctx)
	return true, c.strip(ctx, due[0])
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
	return c.record(ctx, fmt.Sprintf("text removal of message %d", m.MessageID), func(ctx context.Context) error {
		return c.Store.MarkTGStripped(ctx, m.ID)
	})
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
		if err := c.sendSummary(ctx, text); err != nil {
			return false, err
		}
		sent = true
	}
	through := today.AddDate(0, 0, -1).Format(time.DateOnly)
	if err := c.record(ctx, "daily summary through "+through, func(ctx context.Context) error {
		if err := c.Store.MarkReportsSent(ctx, ids); err != nil {
			return err
		}
		return c.Store.SetStatus(ctx, map[string]string{store.StatusSummaryDay: through})
	}); err != nil {
		return sent, err
	}
	c.Log.Info("daily summary", "through", through, "sent", sent, "log_reports", len(ids))
	return sent, nil
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

// sendSummary posts the daily summary, in parts when it is long.
func (c *Chat) sendSummary(ctx context.Context, text string) error {
	ctx, err := c.lock(ctx)
	if err != nil {
		return err
	}
	defer c.unlock(ctx)
	parts, _ := split(text, "")
	for _, part := range parts {
		msg, err := c.sendText(ctx, false, part, quote{}, 0, nil)
		if err != nil {
			return err
		}
		if err := c.Store.AddTGMessage(ctx, store.TGMessage{Role: store.RoleSummary, ChatID: msg.Chat.ID,
			MessageID: msg.ID, SentAt: c.Now()}); err != nil {
			return err
		}
	}
	return nil
}

// sendText posts text (link previews off), as a reply when replyTo is set.
// The member's text (q) goes as a pre entity, so Telegram links none of it:
// a "/resume" or a link a spammer wrote is not one tap away for an admin.
func (c *Chat) sendText(ctx context.Context, priority bool, text string, q quote, replyTo int,
	markup models.ReplyMarkup) (*models.Message, error) {
	var entities []models.MessageEntity
	if q.length > 0 {
		entities = []models.MessageEntity{{Type: models.MessageEntityTypePre, Offset: q.offset, Length: q.length}}
	}
	var msg *models.Message
	err := c.call(ctx, priority, func(ctx context.Context, chatID int64) error {
		var err error
		msg, err = c.api.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: text, Entities: entities,
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

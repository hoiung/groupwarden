package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Roles of a message the bot posted in the admin chat.
const (
	RoleReport     = "report"     // a report (or alert) itself, carrying its buttons
	RoleFollowup   = "followup"   // message text that did not fit in the report
	RoleAttachment = "attachment" // the deleted post's saved attachment
	RoleSummary    = "summary"    // the daily keyword summary, or a digest
	RoleReply      = "reply"      // an answer to a command or button press
)

// TGMessage is one message the bot posted in the admin chat.
type TGMessage struct {
	ID        int64
	ReportID  int64 // 0 when it belongs to no report
	Role      string
	ChatID    int64
	MessageID int
	// Part is which part of its report (0 the report itself, then its
	// follow-ups) or of the daily summary the message is.
	Part int
	// Upto is how far into its report's content (the member's text, or the
	// report's own text) the part reached, in UTF-16 units.
	Upto int
	// Stripped is what the message becomes once the member's message text is
	// removed ("" when it never carried any).
	Stripped   string
	SentAt     time.Time
	StrippedAt time.Time
	GoneAt     time.Time // deleted (or replaced by a placeholder)
}

// AddTGMessage records a posted message.
func (s *Store) AddTGMessage(ctx context.Context, m TGMessage) error {
	return s.AddTGMessageStatus(ctx, m, nil)
}

// AddTGMessageStatus records a posted message and writes the status keys kv
// in one transaction (a summary part and the count of parts posted).
func (s *Store) AddTGMessageStatus(ctx context.Context, m TGMessage, kv map[string]string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if err := insertTGMessage(ctx, tx, m); err != nil {
			return err
		}
		return setStatusIn(ctx, tx, kv, s.now())
	})
}

// insertTGMessage records m inside tx (a message already recorded is kept).
func insertTGMessage(ctx context.Context, tx *sql.Tx, m TGMessage) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO tg_messages (report_id, role, chat_id, message_id, part, upto, stripped,
	sent_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (chat_id, message_id) DO NOTHING`,
		nullID(m.ReportID), m.Role, m.ChatID, m.MessageID, m.Part, m.Upto, m.Stripped, m.SentAt.UnixMilli())
	return err
}

// AddAttachmentPost records attachment post m of evidence evidenceID and
// returns it with its ID. kept is false when the evidence copy is gone (a
// `member forget` or a purge ran while the file uploaded): the post is still
// recorded, and its deletion queued in the same transaction, so it never stays
// in the chat unknown to the take-downs.
func (s *Store) AddAttachmentPost(ctx context.Context, m TGMessage, evidenceID int64) (TGMessage, bool, error) {
	kept := true
	err := s.Write(ctx, func(tx *sql.Tx) error {
		if err := insertTGMessage(ctx, tx, m); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT id FROM tg_messages WHERE chat_id = ? AND message_id = ?`, m.ChatID,
			m.MessageID).Scan(&m.ID); err != nil {
			return fmt.Errorf("read the attachment post just recorded: %w", err)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM evidence WHERE id = ?`, evidenceID).Scan(&n); err != nil {
			return fmt.Errorf("check the evidence copy: %w", err)
		}
		if kept = n > 0; kept {
			return nil
		}
		return QueueReportEdits(ctx, tx, []int64{m.ReportID}, []string{EditDeleteAttachment}, s.now())
	})
	return m, kept, err
}

const tgCols = `id, COALESCE(report_id, 0), role, chat_id, message_id, part, upto, stripped, sent_at, COALESCE(stripped_at, 0),
	COALESCE(gone_at, 0)`

func (s *Store) tgMessages(ctx context.Context, query string, args ...any) ([]TGMessage, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read admin-chat messages: %w", err)
	}
	defer rows.Close()
	var out []TGMessage
	for rows.Next() {
		var m TGMessage
		var sent, stripped, gone int64
		if err := rows.Scan(&m.ID, &m.ReportID, &m.Role, &m.ChatID, &m.MessageID, &m.Part, &m.Upto, &m.Stripped, &sent, &stripped,
			&gone); err != nil {
			return nil, fmt.Errorf("read admin-chat messages: %w", err)
		}
		m.SentAt, m.StrippedAt, m.GoneAt = time.UnixMilli(sent), msTime(stripped), msTime(gone)
		out = append(out, m)
	}
	return out, rows.Err()
}

// TGMessagesFor lists the messages posted for a report, oldest first.
func (s *Store) TGMessagesFor(ctx context.Context, reportID int64) ([]TGMessage, error) {
	return s.tgMessages(ctx, `SELECT `+tgCols+` FROM tg_messages WHERE report_id = ? ORDER BY id`, reportID)
}

// ReportOfTGMessage returns the report a posted message belongs to; ok is
// false when the message is not a report part (unknown, or a summary). A read
// error is returned as such, never as "not a report".
func (s *Store) ReportOfTGMessage(ctx context.Context, chatID int64, messageID int) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(report_id, 0) FROM tg_messages WHERE chat_id = ? AND message_id = ?`,
		chatID, messageID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the report of an admin-chat message: %w", err)
	}
	return id, id != 0, nil
}

// AttachmentsShownSince lists attachment posts still showing that were posted
// at or before cutoff (due to be taken down).
func (s *Store) AttachmentsShownSince(ctx context.Context, cutoff time.Time) ([]TGMessage, error) {
	return s.tgMessages(ctx, `SELECT `+tgCols+` FROM tg_messages
WHERE role = 'attachment' AND gone_at IS NULL AND sent_at <= ? ORDER BY id`, cutoff.UnixMilli())
}

// StripsDue lists up to limit posted messages that still carry a member's
// message text and belong to a report created before its community's cutoff
// (cutoffs by community; def for any other), oldest first.
func (s *Store) StripsDue(ctx context.Context, def time.Time, cutoffs map[string]time.Time, limit int) ([]TGMessage, error) {
	ids := make([]string, 0, len(cutoffs))
	for id := range cutoffs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	cutoff, args := "?", []any{}
	if len(ids) > 0 {
		cutoff = "CASE community"
		for _, id := range ids {
			cutoff += " WHEN ? THEN ?"
			args = append(args, id, cutoffs[id].UnixMilli())
		}
		cutoff += " ELSE ? END"
	}
	args = append(args, def.UnixMilli(), limit)
	return s.tgMessages(ctx, `SELECT `+tgCols+` FROM tg_messages
WHERE stripped != '' AND stripped_at IS NULL AND gone_at IS NULL
	AND report_id IN (SELECT id FROM reports WHERE created_at < `+cutoff+`) ORDER BY id LIMIT ?`, args...)
}

// MarkTGStripped records that a message's member text was removed, and drops
// the stripped copy (the chat now shows it; the database keeps none).
func (s *Store) MarkTGStripped(ctx context.Context, id int64) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE tg_messages SET stripped_at = ?, stripped = '' WHERE id = ?`,
			s.now().UnixMilli(), id)
		return err
	})
}

// MarkTGGone records that a message was deleted or replaced by a placeholder
// (the first time only: a later record keeps when it went).
func (s *Store) MarkTGGone(ctx context.Context, id int64) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE tg_messages SET gone_at = ? WHERE id = ? AND gone_at IS NULL`, s.now().UnixMilli(), id)
		return err
	})
}

// TGMessage reads one posted message; ok is false when it is no longer
// there (its report was purged).
func (s *Store) TGMessage(ctx context.Context, id int64) (TGMessage, bool, error) {
	ms, err := s.tgMessages(ctx, `SELECT `+tgCols+` FROM tg_messages WHERE id = ?`, id)
	if err != nil || len(ms) == 0 {
		return TGMessage{}, false, err
	}
	return ms[0], true, nil
}

// AttachmentToPost is a delivered report that owns its post's attachment
// (Report.OwnsAttachment), with that post's evidence.
type AttachmentToPost struct {
	Report   Report
	Evidence Evidence
}

// AttachmentsToPost lists delivered reports that own their post's attachment
// (Report.OwnsAttachment), whatever their kind, whose evidence holds a saved
// file and that never had an attachment post, leaving out the evidence IDs in
// skip (uploads waiting out a backoff, so they never fill the window).
func (s *Store) AttachmentsToPost(ctx context.Context, limit int, skip []int64) ([]AttachmentToPost, error) {
	if skip == nil {
		skip = []int64{}
	}
	skipped, err := json.Marshal(skip)
	if err != nil {
		return nil, err
	}
	return s.attachments(ctx, "attachments to post", `SELECT reports.id FROM reports JOIN evidence e ON e.id = reports.evidence_id
WHERE `+ownsAttachment+` AND reports.sent_at IS NOT NULL AND e.media_state = 'saved'
	AND e.id NOT IN (SELECT value FROM json_each(?))
	AND NOT EXISTS (SELECT 1 FROM tg_messages t WHERE t.report_id = reports.id AND t.role = 'attachment')
ORDER BY reports.id LIMIT ?`, string(skipped), limit)
}

// AttachmentsNotKept lists delivered reports that own their post's attachment
// and told the admins it was being saved or posted below, while it was then
// not kept (the download failed or was too large, the saved file was lost, or
// Telegram did not take it) and was never posted: one posted and lost later is
// answered by the [Show attachment] button that finds it gone.
func (s *Store) AttachmentsNotKept(ctx context.Context, limit int) ([]AttachmentToPost, error) {
	return s.attachments(ctx, "attachments not kept", `SELECT reports.id FROM reports JOIN evidence e ON e.id = reports.evidence_id
WHERE `+ownsAttachment+` AND reports.sent_at IS NOT NULL AND reports.media_told IN ('pending', 'saved')
	AND e.media_state IN ('failed', 'too_large')
	AND NOT EXISTS (SELECT 1 FROM tg_messages t WHERE t.report_id = reports.id AND t.role = 'attachment')
ORDER BY reports.id LIMIT ?`, limit)
}

// attachments reads the reports query selects, each with its evidence.
func (s *Store) attachments(ctx context.Context, what, query string, args ...any) ([]AttachmentToPost, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", what, err)
	}
	// The ids are read and the rows closed before the per-report reads: the
	// store has one connection, which open rows would hold.
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]AttachmentToPost, 0, len(ids))
	for _, id := range ids {
		r, ok, err := s.Report(ctx, id)
		if err != nil {
			return out, err
		}
		if !ok {
			continue // purged since the query
		}
		e, ok, err := s.Evidence(ctx, r.EvidenceID)
		if err != nil {
			return out, err
		}
		if !ok {
			continue
		}
		out = append(out, AttachmentToPost{Report: r, Evidence: e})
	}
	return out, nil
}

// Report reads one report.
func (s *Store) Report(ctx context.Context, id int64) (Report, bool, error) {
	r, err := scanReport(s.db.QueryRowContext(ctx, `SELECT `+reportCols+` FROM reports WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// Press is one admin's button press or command acting on a report, keyed
// report + button so a repeat is answered, never acted on twice.
type Press struct {
	ReportID  int64
	Button    string
	UserID    int64
	UserName  string
	CreatedAt time.Time
	Result    string
}

// ClaimPress records p unless that report + button was already pressed; it
// returns the first press and whether this one is it.
func (s *Store) ClaimPress(ctx context.Context, p Press) (Press, bool, error) {
	var first Press
	var claimed bool
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO tg_presses (report_id, button, user_id, user_name, created_at)
VALUES (?, ?, ?, ?, ?) ON CONFLICT (report_id, button) DO NOTHING`, p.ReportID, p.Button, p.UserID, p.UserName,
			s.now().UnixMilli())
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		claimed = n == 1
		var created int64
		if err := tx.QueryRowContext(ctx, `SELECT report_id, button, user_id, user_name, created_at, result FROM tg_presses
WHERE report_id = ? AND button = ?`, p.ReportID, p.Button).Scan(&first.ReportID, &first.Button, &first.UserID,
			&first.UserName, &created, &first.Result); err != nil {
			return err
		}
		first.CreatedAt = time.UnixMilli(created)
		return nil
	})
	return first, claimed, err
}

// SetPressResult records what a press did.
func (s *Store) SetPressResult(ctx context.Context, reportID int64, button, result string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE tg_presses SET result = ? WHERE report_id = ? AND button = ?`,
			result, reportID, button)
		return err
	})
}

// ReleasePress forgets a press whose action failed, so it can be pressed again.
func (s *Store) ReleasePress(ctx context.Context, reportID int64, button string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM tg_presses WHERE report_id = ? AND button = ?`, reportID, button)
		return err
	})
}

// ReleaseUnfinishedPresses forgets every press a run claimed and never
// recorded a result for (it stopped or crashed in between), so it can be
// pressed again; every press's action is safe to run twice. Only for the start
// of a run, before any press is taken. It returns how many it released.
func (s *Store) ReleaseUnfinishedPresses(ctx context.Context) (int64, error) {
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM tg_presses WHERE result = ''`)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// UnsentReportsExcept lists undelivered reports of any kind but skip,
// priority first, then oldest.
func (s *Store) UnsentReportsExcept(ctx context.Context, skip string, limit int) ([]Report, error) {
	return s.reports(ctx, `SELECT `+reportCols+` FROM reports WHERE sent_at IS NULL AND kind != ?
ORDER BY priority DESC, id LIMIT ?`, skip, limit)
}

// UnsentReportsOf lists undelivered reports of kind, oldest first.
func (s *Store) UnsentReportsOf(ctx context.Context, kind string, limit int) ([]Report, error) {
	return s.reports(ctx, `SELECT `+reportCols+` FROM reports WHERE sent_at IS NULL AND kind = ? ORDER BY id LIMIT ?`,
		kind, limit)
}

// CountUnsent counts undelivered reports that are not priority and not of kind skip.
func (s *Store) CountUnsent(ctx context.Context, skip string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reports WHERE sent_at IS NULL AND priority = 0 AND kind != ?`,
		skip).Scan(&n)
	return n, err
}

// MarkReportsSent records several reports as delivered (a digest, or the
// daily summary) in one transaction.
func (s *Store) MarkReportsSent(ctx context.Context, ids []int64) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE reports SET sent_at = ? WHERE id = ? AND sent_at IS NULL`,
				s.now().UnixMilli(), id); err != nil {
				return err
			}
		}
		return nil
	})
}

// CountStaleIntended counts enforce rows still at intended that were written
// before cutoff (queued too long: paused, or failing to send).
func (s *Store) CountStaleIntended(ctx context.Context, cutoff time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger WHERE status = 'intended' AND mode = 'enforce'
	AND created_at < ?`, cutoff.UnixMilli()).Scan(&n)
	return n, err
}

// DeleteReportEdit drops a queued edit once it is done: that queuing only, by
// its ID, so the same edit queued again meanwhile stays queued.
func (s *Store) DeleteReportEdit(ctx context.Context, id int64) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM report_edits WHERE id = ?`, id)
		return err
	})
}

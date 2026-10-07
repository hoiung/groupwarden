package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
)

// Report is one message for the admin chat, mapped to the ledger rows it is
// about. It holds no member's message text: that lives in the evidence copy.
type Report struct {
	ID         int64
	Kind       string
	Priority   bool
	Community  string
	Subject    string // the member the report is about (for `member forget`)
	EvidenceID int64
	Text       string
	Buttons    []string
	// Action is the report's "Action:" line when it is not the one for its
	// kind ("": the kind's).
	Action    string
	CreatedAt time.Time
	SentAt    time.Time
}

// InsertReport writes r and its ledger links inside tx. A report of an alert
// kind in the priority set (AC 4.1) is priority whoever wrote it.
func InsertReport(ctx context.Context, tx *sql.Tx, r Report, ledgerIDs []int64, now time.Time) (int64, error) {
	priority := r.Priority || alert.Kind(r.Kind).Priority()
	res, err := tx.ExecContext(ctx, `INSERT INTO reports (kind, priority, community, subject, evidence_id, text, buttons, action, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.Kind, priority, r.Community, r.Subject, nullID(r.EvidenceID), r.Text,
		strings.Join(r.Buttons, ","), r.Action, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("report insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, l := range ledgerIDs {
		if _, err := tx.ExecContext(ctx, `INSERT INTO report_ledger (report_id, ledger_id) VALUES (?, ?)
ON CONFLICT DO NOTHING`, id, l); err != nil {
			return 0, fmt.Errorf("report link: %w", err)
		}
	}
	return id, nil
}

// Reported reports whether a report of kind about subject in community was
// written at or after since, inside tx.
func Reported(ctx context.Context, tx *sql.Tx, kind, community, subject string, since time.Time) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM reports WHERE subject = ? AND kind = ? AND community = ?
	AND created_at >= ?`, subject, kind, community, since.UnixMilli()).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("read reports: %w", err)
	}
	return n > 0, nil
}

// AddReport writes r in its own transaction.
func (s *Store) AddReport(ctx context.Context, r Report, ledgerIDs []int64) (int64, error) {
	var id int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = InsertReport(ctx, tx, r, ledgerIDs, s.now())
		return err
	})
	return id, err
}

const reportCols = `id, kind, priority, community, subject, COALESCE(evidence_id, 0), text, buttons, action, created_at,
	COALESCE(sent_at, 0)`

func scanReport(sc interface{ Scan(...any) error }) (Report, error) {
	var r Report
	var buttons string
	var created, sent int64
	err := sc.Scan(&r.ID, &r.Kind, &r.Priority, &r.Community, &r.Subject, &r.EvidenceID, &r.Text, &buttons, &r.Action,
		&created, &sent)
	if buttons != "" {
		r.Buttons = strings.Split(buttons, ",")
	}
	r.CreatedAt, r.SentAt = time.UnixMilli(created), msTime(sent)
	return r, err
}

// UnsentReports lists reports not yet delivered, priority first, then oldest.
func (s *Store) UnsentReports(ctx context.Context, limit int) ([]Report, error) {
	return s.reports(ctx, `SELECT `+reportCols+` FROM reports WHERE sent_at IS NULL ORDER BY priority DESC, id LIMIT ?`, limit)
}

// ReportsFor lists the reports about any of the given member keys.
func (s *Store) ReportsFor(ctx context.Context, subjects []string) ([]Report, error) {
	return s.reports(ctx, `SELECT `+reportCols+` FROM reports WHERE subject IN (SELECT value FROM json_each(?)) ORDER BY id`,
		jsonList(nonEmpty(subjects)))
}

func (s *Store) reports(ctx context.Context, query string, args ...any) ([]Report, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read reports: %w", err)
	}
	defer rows.Close()
	var out []Report
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, fmt.Errorf("read reports: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkReportSent records a delivered report.
func (s *Store) MarkReportSent(ctx context.Context, id int64) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE reports SET sent_at = ? WHERE id = ?`, s.now().UnixMilli(), id)
		return err
	})
}

// Report edit operations queued for the admin chat.
const (
	EditStripText        = "strip_text"        // remove the member's message text from a sent report
	EditDeleteAttachment = "delete_attachment" // delete an attachment post still showing
)

// ReportEdit is one queued change to a sent report.
type ReportEdit struct {
	ReportID int64
	Op       string
}

// QueueReportEdits queues ops for each report inside tx (a repeat is ignored).
func QueueReportEdits(ctx context.Context, tx *sql.Tx, reportIDs []int64, ops []string, now time.Time) error {
	for _, id := range reportIDs {
		for _, op := range ops {
			if _, err := tx.ExecContext(ctx, `INSERT INTO report_edits (report_id, op, created_at) VALUES (?, ?, ?)
ON CONFLICT (report_id, op) DO NOTHING`, id, op, now.UnixMilli()); err != nil {
				return fmt.Errorf("queue report edit: %w", err)
			}
		}
	}
	return nil
}

// ReportEdits lists queued report edits, oldest first.
func (s *Store) ReportEdits(ctx context.Context) ([]ReportEdit, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT report_id, op FROM report_edits ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read report edits: %w", err)
	}
	defer rows.Close()
	var out []ReportEdit
	for rows.Next() {
		var e ReportEdit
		if err := rows.Scan(&e.ReportID, &e.Op); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeReports deletes delivered reports created before cutoff. Their ledger
// links and admin-chat messages go with them (ON DELETE CASCADE); the button
// presses on them (who pressed) and the admin-chat messages tied to no report
// (summaries, plain replies) posted before cutoff are deleted here.
func (s *Store) PurgeReports(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM reports WHERE created_at < ? AND sent_at IS NOT NULL`, cutoff.UnixMilli())
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `DELETE FROM tg_presses WHERE report_id NOT IN (SELECT id FROM reports)`); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM tg_messages WHERE report_id IS NULL AND sent_at < ?`, cutoff.UnixMilli())
		return err
	})
	return n, err
}

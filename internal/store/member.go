package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Forgotten is what ForgetMember deleted.
type Forgotten struct {
	Evidence    int64
	Ledger      int64
	Inbox       int64
	ReportEdits int64    // reports queued for text stripping / attachment deletion
	Files       []string // attachment files to delete (the caller removes them)
	KeptBans    []Ban    // active bans are kept
}

// ForgetMember deletes everything groupwarden.db holds about the member known
// by ids (LIDs and phone JIDs): evidence copies, action-log rows and queued
// inbox rows, in one transaction. Active bans are kept (and returned, so the
// caller can say why), and every report about the member is queued for its
// message text to be removed and any attachment post deleted.
func (s *Store) ForgetMember(ctx context.Context, ids []string) (Forgotten, error) {
	var f Forgotten
	list := jsonList(nonEmpty(ids))
	err := s.Write(ctx, func(tx *sql.Tx) error {
		var err error
		in := `IN (SELECT value FROM json_each(?1))`
		f.Files, err = mediaPaths(ctx, tx, `SELECT media_path FROM evidence WHERE media_path != '' AND (subject `+in+
			` OR sender `+in+` OR (sender_alt != '' AND sender_alt `+in+`))`, list)
		if err != nil {
			return err
		}
		if f.Evidence, err = execCount(ctx, tx, `DELETE FROM evidence WHERE subject `+in+` OR sender `+in+
			` OR (sender_alt != '' AND sender_alt `+in+`)`, list); err != nil {
			return err
		}
		// Rows still queued stay (their action has not fired yet).
		if f.Ledger, err = execCount(ctx, tx, `DELETE FROM ledger WHERE target `+in+
			` AND id NOT IN (SELECT ledger_id FROM outbox)`, list); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM report_ledger WHERE ledger_id NOT IN (SELECT id FROM ledger)`); err != nil {
			return err
		}
		if f.Inbox, err = execCount(ctx, tx, `DELETE FROM inbox WHERE kind IN ('message', 'undecryptable') AND (
	json_extract(payload, '$.sender') `+in+` OR json_extract(payload, '$.sender_alt') `+in+`)`, list); err != nil {
			return err
		}
		reportIDs, err := idList(ctx, tx, `SELECT id FROM reports WHERE subject `+in, list)
		if err != nil {
			return err
		}
		if err := QueueReportEdits(ctx, tx, reportIDs, []string{EditStripText, EditDeleteAttachment}, s.now()); err != nil {
			return err
		}
		f.ReportEdits = int64(len(reportIDs))
		// The report rows stay (the queued edits need them) but no longer
		// name the person.
		if _, err := tx.ExecContext(ctx, `UPDATE reports SET subject = '' WHERE subject `+in, list); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+banCols+` FROM bans WHERE member `+in+
			` OR (lid != '' AND lid `+in+`) OR (phone != '' AND phone `+in+`)`, list)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanBan(rows)
			if err != nil {
				return err
			}
			f.KeptBans = append(f.KeptBans, b)
		}
		return rows.Err()
	})
	if err != nil {
		return Forgotten{}, fmt.Errorf("forget member: %w", err)
	}
	return f, nil
}

func execCount(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func idList(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

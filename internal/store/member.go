package store

import (
	"context"
	"database/sql"
	"errors"
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
	HeldBans    int64    // bans a pause holds are kept too: they apply once the pause ends
}

// OtherAddress returns the member's other address as the bot last recorded
// it beside addr (their phone JID for a LID, their LID for a phone JID; ""
// when it has none): the sender and sender_alt of a post it decided (seen)
// or reported (evidence), or the target of a ban row and the phone it keeps
// while that ban is in force (its ban list entry links the row, or a pause
// holds it). A lifted or undone ban's row pairs nothing: the number may have
// passed to someone else since. The group list pairs the two only for the
// members it lists: someone who joined since it was last read is known by
// their LID alone, as a join names no phone number.
func (s *Store) OtherAddress(ctx context.Context, addr string) (string, error) {
	return OtherAddressIn(ctx, s.db, addr)
}

// OtherAddressIn is OtherAddress read through q: a caller inside a
// transaction reads through it, as the store has one connection.
func OtherAddressIn(ctx context.Context, q queryer, addr string) (string, error) {
	return otherAddress(ctx, q, addr, banInForce)
}

// OtherAddressEver is OtherAddress over every ban row of addr, a lifted or
// undone ban's too: `member show|forget` reach everything recorded of the
// person, as they did before a lift.
func (s *Store) OtherAddressEver(ctx context.Context, addr string) (string, error) {
	return otherAddress(ctx, s.db, addr, "true")
}

// banInForce selects the ban rows whose ban is in force: linked from a ban
// list entry, or held by a pause.
const banInForce = `(EXISTS (SELECT 1 FROM bans WHERE bans.ledger_id = ledger.id) OR ` + heldBan + `)`

func otherAddress(ctx context.Context, q queryer, addr, banRows string) (string, error) {
	if addr == "" {
		return "", nil
	}
	var other string
	err := q.QueryRowContext(ctx, otherAddressQuery(banRows), addr).Scan(&other)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read other address: %w", err)
	}
	return other, nil
}

// otherAddressQuery is otherAddress's query, ?1 the address, taking the ban
// rows banRows selects. Each read searches an index (migration 13). Beside
// the column it matches with ?1 a read also asks that column not to be
// empty: the column's index is partial (empty values left out), and SQLite
// uses one only when the query's own terms imply its condition. The address
// is never empty here (otherAddress returns first), so the extra terms change
// no result.
func otherAddressQuery(banRows string) string {
	return `SELECT other FROM (
	SELECT sender_alt AS other, decided_at AS at FROM seen WHERE sender = ?1 AND sender != '' AND sender_alt != ''
	UNION ALL SELECT sender, decided_at FROM seen WHERE sender_alt = ?1 AND sender_alt != '' AND sender != ''
	UNION ALL SELECT sender_alt, created_at FROM evidence WHERE sender = ?1 AND sender != '' AND sender_alt != ''
	UNION ALL SELECT sender, created_at FROM evidence WHERE sender_alt = ?1 AND sender_alt != ''
	UNION ALL SELECT address, created_at FROM ledger WHERE action = 'ban' AND target = ?1 AND address NOT IN ('', ?1)
		AND ` + banRows + `
	UNION ALL SELECT target, created_at FROM ledger WHERE action = 'ban' AND address = ?1 AND address != ''
		AND target != ?1 AND ` + banRows + `)
ORDER BY at DESC LIMIT 1`
}

// ForgetMember deletes everything groupwarden.db holds about the member known
// by ids (LIDs and phone JIDs): evidence copies, action-log rows and queued
// inbox rows, and their address on the posts the bot decided, in one
// transaction. Active bans are kept (and returned, so the
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
		// Rows still queued, and bans a pause holds, stay: their action has
		// not happened yet (a held ban is kept like an active one).
		if f.Ledger, err = execCount(ctx, tx, `DELETE FROM ledger WHERE target `+in+
			` AND id NOT IN (SELECT ledger_id FROM outbox) AND NOT `+heldBan, list); err != nil {
			return err
		}
		// Counted as /unban <number> finds them: by target, or by the phone a
		// held ban keeps (it is not deleted above, which matches the target).
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger WHERE `+heldBan+` AND `+banRowOf,
			list).Scan(&f.HeldBans); err != nil {
			return fmt.Errorf("count held bans: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM report_ledger WHERE ledger_id NOT IN (SELECT id FROM ledger)`); err != nil {
			return err
		}
		if f.Inbox, err = execCount(ctx, tx, `DELETE FROM inbox WHERE kind IN ('message', 'undecryptable') AND (
	json_extract(payload, '$.sender') `+in+` OR json_extract(payload, '$.sender_alt') `+in+`)`, list); err != nil {
			return err
		}
		// The delivery records stay (a redelivery must still be ignored) but
		// no longer name who sent the post.
		if _, err := tx.ExecContext(ctx, `UPDATE seen SET sender = '', sender_alt = '' WHERE sender `+in+
			` OR sender_alt `+in, list); err != nil {
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

func idList(ctx context.Context, tx queryer, query string, args ...any) ([]int64, error) {
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

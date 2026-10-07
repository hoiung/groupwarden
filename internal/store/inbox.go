package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// InboxRow is one persisted, undecided event.
type InboxRow struct {
	ID         int64
	Key        string
	Kind       string
	Payload    []byte
	ReceivedAt time.Time
}

// Seen identifies a decided delivery; only IDs and times are kept, never text.
type Seen struct {
	Chat       string
	MsgID      string
	ServerTime time.Time
}

// InboxPut stores an event unless the same delivery is already queued or was
// already decided. It reports false for such a duplicate.
func (s *Store) InboxPut(ctx context.Context, key, kind string, payload []byte) (bool, error) {
	var inserted bool
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
INSERT INTO inbox (dedupe_key, kind, payload, received_at)
SELECT ?, ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM seen WHERE dedupe_key = ?)
ON CONFLICT (dedupe_key) DO NOTHING`, key, kind, payload, s.now().UnixMilli(), key)
		if err != nil {
			return fmt.Errorf("inbox insert: %w", err)
		}
		n, err := res.RowsAffected()
		inserted = n == 1
		return err
	})
	return inserted, err
}

// InboxOldest returns up to limit undecided rows, oldest first.
func (s *Store) InboxOldest(ctx context.Context, limit int) ([]InboxRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, dedupe_key, kind, payload, received_at FROM inbox ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read inbox: %w", err)
	}
	defer rows.Close()
	var out []InboxRow
	for rows.Next() {
		var r InboxRow
		var at int64
		if err := rows.Scan(&r.ID, &r.Key, &r.Kind, &r.Payload, &at); err != nil {
			return nil, fmt.Errorf("read inbox: %w", err)
		}
		r.ReceivedAt = time.UnixMilli(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// InboxLen counts undecided rows.
func (s *Store) InboxLen(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox`).Scan(&n)
	return n, err
}

// InboxLast is the ID of the newest undecided row (0 when none is).
func (s *Store) InboxLast(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM inbox`).Scan(&id)
	return id, err
}

// InboxUndecidedThrough reports whether any row up to and including id is
// still undecided.
func (s *Store) InboxUndecidedThrough(ctx context.Context, id int64) (bool, error) {
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM inbox WHERE id <= ?)`, id).Scan(&found)
	return found, err
}

// Decide records a decision for row in one transaction: record writes the
// decision, the inbox row (and with it the member's text) is deleted, and the
// delivery is marked seen so a redelivery is ignored.
func (s *Store) Decide(ctx context.Context, row InboxRow, seen Seen, record func(tx *sql.Tx) error) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if err := record(tx); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM inbox WHERE id = ?`, row.ID)
		if err != nil {
			return fmt.Errorf("inbox delete: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("inbox row %d was not deleted (affected %d): %v", row.ID, n, err)
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO seen (dedupe_key, chat, msg_id, server_time, decided_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (dedupe_key) DO NOTHING`, row.Key, seen.Chat, seen.MsgID, seen.ServerTime.UnixMilli(), s.now().UnixMilli())
		if err != nil {
			return fmt.Errorf("seen insert: %w", err)
		}
		return nil
	})
}

// SeenTime returns the server time of an already-decided message, so an edit
// can be aged by its original.
func (s *Store) SeenTime(ctx context.Context, chat, msgID string) (time.Time, bool, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT server_time FROM seen WHERE chat = ? AND msg_id = ?`, chat, msgID).Scan(&at)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read seen: %w", err)
	}
	return time.UnixMilli(at), true, nil
}

// PurgeSeen drops dedupe records decided before cutoff.
func (s *Store) PurgeSeen(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM seen WHERE decided_at < ?`, cutoff.UnixMilli())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

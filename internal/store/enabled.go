package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Enabled kinds.
const (
	// KindCommunity: a WhatsApp Community, every group of it moderated.
	KindCommunity = "community"
	// KindGroup: one group on its own, whatever community it is in.
	KindGroup = "group"
)

// Enabled is a community or a single group an admin switched on with
// /enable.
type Enabled struct {
	ID   string // the community's or the group's "…@g.us" ID
	Kind string // KindCommunity or KindGroup
	Name string // as WhatsApp gave it at /enable
	By   string // the admin, as the ledger names actors
	At   time.Time
}

// Enable records e, replacing an earlier entry for the same ID.
func (s *Store) Enable(ctx context.Context, e Enabled) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO enabled (id, kind, name, enabled_by, created_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET kind = excluded.kind, name = excluded.name, enabled_by = excluded.enabled_by,
	created_at = excluded.created_at`,
			e.ID, e.Kind, e.Name, e.By, e.At.UnixMilli())
		return err
	})
}

// Disable deletes the entry for id; false when there was none.
func (s *Store) Disable(ctx context.Context, id string) (bool, error) {
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM enabled WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n > 0, err
}

// EnabledList lists every entry, sorted by ID.
func (s *Store) EnabledList(ctx context.Context) ([]Enabled, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, name, enabled_by, created_at FROM enabled ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read the enabled list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Enabled
	for rows.Next() {
		var e Enabled
		var at int64
		if err := rows.Scan(&e.ID, &e.Kind, &e.Name, &e.By, &at); err != nil {
			return nil, fmt.Errorf("read the enabled list: %w", err)
		}
		e.At = time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the enabled list: %w", err)
	}
	return out, nil
}

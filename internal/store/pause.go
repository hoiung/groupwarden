package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Scope is which actions a pause stops.
type Scope string

const (
	// ScopeAll stops every action, deletes included.
	ScopeAll Scope = "all"
	// ScopeRemoveBan stops removals and bans; deletes continue.
	ScopeRemoveBan Scope = "remove_ban"
)

// Pause sources: each is one reason a pause can be set; a source is cleared
// on its own.
const (
	SourceStorage        = "storage"
	SourceTempBan        = "temporary_ban"
	SourceExtraCompanion = "extra_companion"
	// SourceBreaker: too many removals in the breaker window.
	SourceBreaker = "breaker"
	// SourceRestore: the database was restored from a backup; every action
	// waits until an admin has checked /status and resumed.
	SourceRestore = "restore"
)

// Pause is one active pause.
type Pause struct {
	Source string
	Scope  Scope
	Reason string
	Since  time.Time
}

// SetPause records p, keeping the original start time if source is already set.
func (s *Store) SetPause(ctx context.Context, p Pause) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
INSERT INTO pause (source, scope, reason, since) VALUES (?, ?, ?, ?)
ON CONFLICT (source) DO UPDATE SET scope = excluded.scope, reason = excluded.reason`,
			p.Source, string(p.Scope), p.Reason, p.Since.UnixMilli())
		return err
	})
}

// ClearPause lifts the pause from source.
func (s *Store) ClearPause(ctx context.Context, source string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM pause WHERE source = ?`, source)
		return err
	})
}

// Pauses lists every active pause, the in-memory storage failure included.
func (s *Store) Pauses(ctx context.Context) ([]Pause, error) {
	var out []Pause
	if f := s.storageFailure(); f != nil {
		out = append(out, *f)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT source, scope, reason, since FROM pause ORDER BY since`)
	if err != nil {
		return out, fmt.Errorf("read pause state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p Pause
		var since int64
		if err := rows.Scan(&p.Source, &p.Scope, &p.Reason, &since); err != nil {
			return out, fmt.Errorf("read pause state: %w", err)
		}
		p.Since = time.UnixMilli(since)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("read pause state: %w", err)
	}
	return out, nil
}

// PausedFor reports whether an action of scope may NOT fire now, and why.
// Deletes are stopped only by ScopeAll pauses; removals and bans by either.
// Pause state that cannot be read counts as paused (fail closed).
func (s *Store) PausedFor(ctx context.Context, scope Scope) (bool, string) {
	pauses, err := s.Pauses(ctx)
	if err != nil {
		return true, "pause state unreadable: " + err.Error()
	}
	for _, p := range pauses {
		if p.Scope == ScopeAll || scope == ScopeRemoveBan {
			return true, p.Source + ": " + p.Reason
		}
	}
	return false, ""
}

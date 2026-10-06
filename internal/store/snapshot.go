package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
)

// Snapshot writes a consistent copy of the database to dst (which must not
// exist) while it stays in use. The copy carries a full pause, so a bot
// restored from it starts with every action paused until an admin has
// checked /status and resumed: a restored outbox may hold actions that were
// already sent or are no longer wanted.
func (s *Store) Snapshot(ctx context.Context, dst string) error {
	if _, err := os.Stat(dst); err == nil { // #nosec G703 -- the caller names a backup file
		return fmt.Errorf("snapshot: %s already exists", dst)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return s.check(fmt.Errorf("snapshot: %w", err))
	}
	db, err := sql.Open("sqlite", DSN(dst))
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `INSERT INTO pause (source, scope, reason, since) VALUES (?, ?, ?, ?)
ON CONFLICT (source) DO UPDATE SET scope = excluded.scope, reason = excluded.reason, since = excluded.since`,
		SourceRestore, string(ScopeAll), "restored from a backup: check /status, then press [Resume]", s.now().UnixMilli())
	if err != nil {
		return fmt.Errorf("snapshot: mark the copy paused: %w", err)
	}
	return nil
}

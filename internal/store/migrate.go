package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are forward-only: entry i brings the schema to version i+1.
// Never edit an entry once released; add a new one.
var migrations = []string{
	// 1: inbox, delivery dedupe, pause state, run status, daily counters.
	`
CREATE TABLE inbox (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	dedupe_key  TEXT    NOT NULL UNIQUE,
	kind        TEXT    NOT NULL,
	payload     BLOB    NOT NULL,
	received_at INTEGER NOT NULL
);
CREATE TABLE seen (
	dedupe_key  TEXT    PRIMARY KEY,
	chat        TEXT    NOT NULL,
	msg_id      TEXT    NOT NULL,
	server_time INTEGER NOT NULL,
	decided_at  INTEGER NOT NULL
);
CREATE INDEX seen_decided_at ON seen (decided_at);
CREATE INDEX seen_msg ON seen (chat, msg_id);
CREATE TABLE pause (
	source TEXT    PRIMARY KEY,
	scope  TEXT    NOT NULL CHECK (scope IN ('all', 'remove_ban')),
	reason TEXT    NOT NULL,
	since  INTEGER NOT NULL
);
CREATE TABLE status (
	key        TEXT    PRIMARY KEY,
	value      TEXT    NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE counters (
	day  TEXT    NOT NULL,
	name TEXT    NOT NULL,
	n    INTEGER NOT NULL,
	PRIMARY KEY (day, name)
);
`,
}

// SchemaVersion is the version this binary migrates to.
func SchemaVersion() int { return len(migrations) }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return s.check(fmt.Errorf("create schema_version: %w", err))
	}
	var current int
	err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&current)
	switch {
	case err == sql.ErrNoRows:
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return s.check(fmt.Errorf("init schema_version: %w", err))
		}
	case err != nil:
		return fmt.Errorf("read schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("groupwarden.db is at schema version %d but this binary knows only %d: run the newer binary or restore a matching backup", current, len(migrations))
	}
	for v := current; v < len(migrations); v++ {
		err := s.Write(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
				return fmt.Errorf("migration %d: %w", v+1, err)
			}
			_, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, v+1)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

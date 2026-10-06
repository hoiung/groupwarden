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
	// 2: action ledger, outbox, ban list, evidence copies, admin reports.
	`
CREATE TABLE ledger (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	key         TEXT    NOT NULL UNIQUE,
	action      TEXT    NOT NULL CHECK (action IN ('revoke', 'remove', 'reject', 'ban', 'unban')),
	chat        TEXT    NOT NULL,
	target      TEXT    NOT NULL,
	trigger_id  TEXT    NOT NULL,
	community   TEXT    NOT NULL,
	mode        TEXT    NOT NULL CHECK (mode IN ('shadow', 'enforce')),
	status      TEXT    NOT NULL CHECK (status IN ('intended', 'requested', 'failed', 'already_gone', 'overturned')),
	reason      TEXT    NOT NULL DEFAULT '',
	rule        TEXT    NOT NULL DEFAULT '',
	actor       TEXT    NOT NULL DEFAULT '',
	config_hash TEXT    NOT NULL,
	msg_id      TEXT    NOT NULL DEFAULT '',
	address     TEXT    NOT NULL DEFAULT '',
	msg_time    INTEGER NOT NULL DEFAULT 0,
	evidence_id INTEGER,
	code        INTEGER NOT NULL DEFAULT 0,
	attempts    INTEGER NOT NULL DEFAULT 0,
	sent_at     INTEGER,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);
CREATE INDEX ledger_target ON ledger (target);
CREATE INDEX ledger_open ON ledger (status, mode);
CREATE INDEX ledger_created ON ledger (created_at);
CREATE INDEX ledger_sent ON ledger (sent_at);
CREATE TABLE outbox (
	ledger_id  INTEGER PRIMARY KEY REFERENCES ledger (id) ON DELETE CASCADE,
	scope      TEXT    NOT NULL CHECK (scope IN ('all', 'remove_ban')),
	not_before INTEGER NOT NULL
);
CREATE TABLE bans (
	member     TEXT    NOT NULL,
	scope      TEXT    NOT NULL,
	lid        TEXT    NOT NULL DEFAULT '',
	phone      TEXT    NOT NULL DEFAULT '',
	reason     TEXT    NOT NULL,
	ledger_id  INTEGER,
	lid_failed INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (member, scope)
);
CREATE INDEX bans_lid ON bans (lid);
CREATE INDEX bans_phone ON bans (phone);
CREATE TABLE evidence (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	chat        TEXT    NOT NULL,
	community   TEXT    NOT NULL,
	sender      TEXT    NOT NULL,
	sender_alt  TEXT    NOT NULL DEFAULT '',
	subject     TEXT    NOT NULL,
	push_name   TEXT    NOT NULL DEFAULT '',
	msg_id      TEXT    NOT NULL,
	target_id   TEXT    NOT NULL,
	msg_time    INTEGER NOT NULL,
	fields      TEXT    NOT NULL,
	normalised  TEXT    NOT NULL,
	rule        TEXT    NOT NULL,
	config_hash TEXT    NOT NULL,
	media_kind  TEXT    NOT NULL DEFAULT '',
	media_mime  TEXT    NOT NULL DEFAULT '',
	media_name  TEXT    NOT NULL DEFAULT '',
	media_size  INTEGER NOT NULL DEFAULT 0,
	media_raw   BLOB,
	media_state TEXT    NOT NULL DEFAULT 'none' CHECK (media_state IN ('none', 'pending', 'saved', 'too_large', 'failed')),
	media_path  TEXT    NOT NULL DEFAULT '',
	media_error TEXT    NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL
);
CREATE INDEX evidence_subject ON evidence (subject);
CREATE INDEX evidence_created ON evidence (created_at);
CREATE INDEX evidence_media ON evidence (media_state);
CREATE TABLE reports (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	kind        TEXT    NOT NULL,
	priority    INTEGER NOT NULL,
	community   TEXT    NOT NULL DEFAULT '',
	subject     TEXT    NOT NULL DEFAULT '',
	evidence_id INTEGER,
	text        TEXT    NOT NULL,
	buttons     TEXT    NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	sent_at     INTEGER
);
CREATE INDEX reports_unsent ON reports (sent_at, priority);
CREATE INDEX reports_subject ON reports (subject);
CREATE INDEX reports_created ON reports (created_at);
CREATE TABLE report_ledger (
	report_id INTEGER NOT NULL REFERENCES reports (id) ON DELETE CASCADE,
	ledger_id INTEGER NOT NULL,
	PRIMARY KEY (report_id, ledger_id)
);
CREATE INDEX report_ledger_ledger ON report_ledger (ledger_id);
CREATE TABLE report_edits (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	report_id  INTEGER NOT NULL,
	op         TEXT    NOT NULL CHECK (op IN ('strip_text', 'delete_attachment')),
	created_at INTEGER NOT NULL,
	UNIQUE (report_id, op)
);
`,
	// 3: the admin chat — every message the bot posted there, and every
	// button press or command that acted on a report.
	`
CREATE TABLE tg_messages (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	report_id   INTEGER REFERENCES reports (id) ON DELETE CASCADE,
	role        TEXT    NOT NULL CHECK (role IN ('report', 'followup', 'attachment', 'summary', 'reply')),
	chat_id     INTEGER NOT NULL,
	message_id  INTEGER NOT NULL,
	stripped    TEXT    NOT NULL DEFAULT '',
	sent_at     INTEGER NOT NULL,
	stripped_at INTEGER,
	gone_at     INTEGER,
	UNIQUE (chat_id, message_id)
);
CREATE INDEX tg_messages_report ON tg_messages (report_id, role);
CREATE INDEX tg_messages_sent ON tg_messages (role, sent_at);
CREATE TABLE tg_presses (
	report_id  INTEGER NOT NULL,
	button     TEXT    NOT NULL,
	user_id    INTEGER NOT NULL,
	user_name  TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	result     TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (report_id, button)
);
`,
}

// migrate brings the schema up to date in ONE write transaction. Every
// connection begins immediate, so the version is read under the write lock: a
// second process opening the same new database (healthcheck beside run) waits,
// then finds the schema current, instead of both applying migration 1.
func (s *Store) migrate(ctx context.Context) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
			return fmt.Errorf("create schema_version: %w", err)
		}
		var current int
		err := tx.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&current)
		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
				return fmt.Errorf("init schema_version: %w", err)
			}
		case err != nil:
			return fmt.Errorf("read schema version: %w", err)
		}
		if current > len(migrations) {
			return fmt.Errorf("groupwarden.db is at schema version %d but this binary knows only %d: run the newer binary or restore a matching backup", current, len(migrations))
		}
		for v := current; v < len(migrations); v++ {
			if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
				return fmt.Errorf("migration %d: %w", v+1, err)
			}
		}
		if current == len(migrations) {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, len(migrations)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
		return nil
	})
}

// Package sessiontest creates a stand-in for the WhatsApp session store
// (whatsmeow.db) holding the tables groupwarden's maintenance queries touch,
// for tests above the WhatsApp layer. TestSessionFixtureMatchesLibrary (in
// internal/client/whatsmeow) checks these columns against the store the
// pinned library creates.
package sessiontest

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite" // pure-Go SQLite driver "sqlite"
)

// Tables is the stand-in schema, column for column as the pinned library
// creates them.
var Tables = map[string]string{
	"whatsmeow_contacts": `CREATE TABLE IF NOT EXISTS whatsmeow_contacts (
	our_jid        TEXT,
	their_jid      TEXT,
	first_name     TEXT,
	full_name      TEXT,
	push_name      TEXT,
	business_name  TEXT,
	redacted_phone TEXT,
	PRIMARY KEY (our_jid, their_jid)
)`,
	"whatsmeow_message_secrets": `CREATE TABLE IF NOT EXISTS whatsmeow_message_secrets (
	our_jid    TEXT,
	chat_jid   TEXT,
	sender_jid TEXT,
	message_id TEXT,
	key        bytea NOT NULL,
	PRIMARY KEY (our_jid, chat_jid, sender_jid, message_id)
)`,
	"whatsmeow_lid_map": `CREATE TABLE IF NOT EXISTS whatsmeow_lid_map (
	lid TEXT PRIMARY KEY,
	pn  TEXT UNIQUE NOT NULL
)`,
}

// Create makes the stand-in at path and returns an open handle to it (closed
// when the test ends).
func Create(t testing.TB, path string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, ddl := range Tables {
		if _, err := db.ExecContext(context.Background(), ddl); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// Exec runs one statement on db, failing the test on error.
func Exec(t testing.TB, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

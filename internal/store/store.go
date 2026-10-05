// Package store is groupwarden's own SQLite database (groupwarden.db): the
// inbox, decisions, pause state and run status. The WhatsApp session lives in
// a separate file owned by the whatsmeow adapter and is never backed up,
// because restoring stale keys breaks the session.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Store is an open groupwarden.db.
type Store struct {
	db        *sql.DB
	now       func() time.Time
	onFailure func(error)

	mu      sync.Mutex
	failure *Pause // set once a write has failed; pauses every action
}

// Options configures Open.
type Options struct {
	// OnWriteFailure is called once, the first time a write fails in a way
	// that means the database cannot be written (full disk, I/O error).
	OnWriteFailure func(error)
	// Now replaces time.Now in tests.
	Now func() time.Time
}

// Open opens (creating if needed) the database at path, refuses a
// Windows-mounted filesystem, enables WAL and applies migrations.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := CheckLinuxFS(dir); err != nil {
		return nil, err
	}
	// Write transactions take the write lock when they begin, so one that
	// reads first can never fail to upgrade while `run` and a store command
	// (ban add, member forget) write at the same time.
	db, err := sql.Open("sqlite", DSN(path)+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection: SQLite has one writer, and a single connection makes
	// every write in this process queue instead of failing with SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, now: time.Now, onFailure: func(error) {}}
	if opts.Now != nil {
		s.now = opts.Now
	}
	if opts.OnWriteFailure != nil {
		s.onFailure = opts.OnWriteFailure
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// DSN is the SQLite connection string used for both databases: WAL so other
// groupwarden commands can read beside `run`, synchronous FULL so an
// acknowledged message survives a power cut, a busy timeout so a short write
// from another command waits instead of failing, and secure_delete so a
// decided message's text is overwritten on disk, not just unlinked.
func DSN(path string) string {
	return "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(10000)" +
		"&_pragma=foreign_keys(1)&_pragma=secure_delete(1)"
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Checkpoint folds the WAL into the main file and truncates it.
func (s *Store) Checkpoint(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return s.check(err)
}

// Now is the store's clock.
func (s *Store) Now() time.Time { return s.now() }

// Write runs fn in one transaction. A failure meaning the database cannot be
// written pauses every action (fail closed) and fires OnWriteFailure once.
func (s *Store) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return s.check(fmt.Errorf("begin: %w", err))
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return s.check(err)
	}
	if err := tx.Commit(); err != nil {
		return s.check(fmt.Errorf("commit: %w", err))
	}
	return nil
}

// check records err as a storage failure when it means writes cannot succeed.
func (s *Store) check(err error) error {
	if !isWriteFailure(err) {
		return err
	}
	s.mu.Lock()
	first := s.failure == nil
	if first {
		s.failure = &Pause{Source: SourceStorage, Scope: ScopeAll, Reason: "database write failed: " + err.Error(), Since: s.now()}
	}
	s.mu.Unlock()
	if first {
		s.onFailure(err)
	}
	return err
}

// ClearStorageFailure lifts the storage pause after the admins resume.
func (s *Store) ClearStorageFailure() {
	s.mu.Lock()
	s.failure = nil
	s.mu.Unlock()
}

func (s *Store) storageFailure() *Pause {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == nil {
		return nil
	}
	p := *s.failure
	return &p
}

// isWriteFailure: SQLite result codes meaning the file cannot be written.
func isWriteFailure(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() & 0xff {
	case sqlite3.SQLITE_FULL, sqlite3.SQLITE_IOERR, sqlite3.SQLITE_READONLY, sqlite3.SQLITE_CANTOPEN,
		sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB, sqlite3.SQLITE_PERM:
		return true
	}
	return false
}

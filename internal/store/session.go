package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Session is maintenance access to the WhatsApp session store (whatsmeow.db):
// purging message secrets by age and reading or deleting what it holds about
// one member. It names whatsmeow's tables and columns at the version pinned
// in go.mod; internal/client/whatsmeow runs these queries against a store the
// library itself created (TestSessionQueriesMatchLibrarySchema), so a pin bump
// that changes them fails there.
type Session struct {
	db  *sql.DB
	now func() time.Time
}

// OpenSession opens the session store at path for maintenance. It works beside
// a running bot through SQLite's own locking.
func OpenSession(ctx context.Context, path string, now func() time.Time) (*Session, error) {
	if now == nil {
		now = time.Now
	}
	db, err := sql.Open("sqlite", DSN(path)+"&mode=rw&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open WhatsApp session store: %w", err)
	}
	// whatsmeow keeps no time with a message secret, so groupwarden records
	// when it first saw each one and ages it from then (at most one purge
	// interval late).
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS groupwarden_secret_seen (
	chat_jid   TEXT    NOT NULL,
	sender_jid TEXT    NOT NULL,
	message_id TEXT    NOT NULL,
	first_seen INTEGER NOT NULL,
	PRIMARY KEY (chat_jid, sender_jid, message_id)
)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prepare WhatsApp session store: %w", err)
	}
	return &Session{db: db, now: now}, nil
}

// Close closes the session store.
func (s *Session) Close() error { return s.db.Close() }

// PurgeSecrets deletes message secrets first seen before cutoff, except those
// of messages in announcementChats, which are kept until announcementCutoff so
// replies to older announcements still decrypt. It returns how many it deleted.
func (s *Session) PurgeSecrets(ctx context.Context, cutoff, announcementCutoff time.Time, announcementChats []string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO groupwarden_secret_seen (chat_jid, sender_jid, message_id, first_seen)
SELECT chat_jid, sender_jid, message_id, ? FROM whatsmeow_message_secrets WHERE true
ON CONFLICT DO NOTHING`, s.now().UnixMilli()); err != nil {
		return 0, fmt.Errorf("stamp message secrets: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM whatsmeow_message_secrets WHERE EXISTS (
	SELECT 1 FROM groupwarden_secret_seen g
	WHERE g.chat_jid = whatsmeow_message_secrets.chat_jid AND g.sender_jid = whatsmeow_message_secrets.sender_jid
		AND g.message_id = whatsmeow_message_secrets.message_id
		AND g.first_seen < CASE WHEN g.chat_jid IN (SELECT value FROM json_each(?)) THEN ? ELSE ? END)`,
		jsonList(announcementChats), announcementCutoff.UnixMilli(), cutoff.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("purge message secrets: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM groupwarden_secret_seen WHERE NOT EXISTS (
	SELECT 1 FROM whatsmeow_message_secrets m WHERE m.chat_jid = groupwarden_secret_seen.chat_jid
		AND m.sender_jid = groupwarden_secret_seen.sender_jid AND m.message_id = groupwarden_secret_seen.message_id)`); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// SessionMember is what the session store holds about one member.
type SessionMember struct {
	Contacts []map[string]string `json:"whatsmeow_contacts"`
	LIDMap   []map[string]string `json:"whatsmeow_lid_map"`
}

// sessionIDs adds the bare user part of each JID: whatsmeow's LID map stores
// users without a server.
func sessionIDs(ids []string) string {
	all := append([]string{}, ids...)
	for _, id := range ids {
		for i := 0; i < len(id); i++ {
			if id[i] == '@' {
				all = append(all, id[:i])
				break
			}
		}
	}
	return jsonList(nonEmpty(all))
}

// Member reads the contact and LID-to-phone rows for anyone known by ids.
func (s *Session) Member(ctx context.Context, ids []string) (SessionMember, error) {
	var m SessionMember
	list := sessionIDs(ids)
	var err error
	m.Contacts, err = rowMaps(ctx, s.db, `SELECT their_jid, COALESCE(first_name, ''), COALESCE(full_name, ''),
	COALESCE(push_name, ''), COALESCE(business_name, ''), COALESCE(redacted_phone, '')
FROM whatsmeow_contacts WHERE their_jid IN (SELECT value FROM json_each(?))`,
		[]string{"their_jid", "first_name", "full_name", "push_name", "business_name", "redacted_phone"}, list)
	if err != nil {
		return m, fmt.Errorf("read WhatsApp contacts: %w", err)
	}
	m.LIDMap, err = rowMaps(ctx, s.db, `SELECT lid, pn FROM whatsmeow_lid_map
WHERE lid IN (SELECT value FROM json_each(?1)) OR pn IN (SELECT value FROM json_each(?1))`, []string{"lid", "pn"}, list)
	if err != nil {
		return m, fmt.Errorf("read WhatsApp LID map: %w", err)
	}
	return m, nil
}

// ForgetMember deletes the contact and LID-to-phone rows for anyone known by
// ids and returns how many rows went.
func (s *Session) ForgetMember(ctx context.Context, ids []string) (int64, error) {
	list := sessionIDs(ids)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var n int64
	for _, q := range []string{
		`DELETE FROM whatsmeow_contacts WHERE their_jid IN (SELECT value FROM json_each(?1))`,
		`DELETE FROM whatsmeow_lid_map WHERE lid IN (SELECT value FROM json_each(?1)) OR pn IN (SELECT value FROM json_each(?1))`,
	} {
		res, err := tx.ExecContext(ctx, q, list)
		if err != nil {
			return 0, fmt.Errorf("forget in WhatsApp session store: %w", err)
		}
		k, _ := res.RowsAffected()
		n += k
	}
	return n, tx.Commit()
}

func rowMaps(ctx context.Context, db *sql.DB, query string, cols []string, args ...any) ([]map[string]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		vals := make([]string, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := map[string]string{}
		for i, c := range cols {
			m[c] = vals[i]
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// BanEverywhere is the scope of a ban that covers every configured community
// (bans.scope all_communities); a per-community ban names its community.
const BanEverywhere = "*"

// Ban is one ban list entry. It is keyed on the member's LID; the phone
// number is kept too when known. Until a phone-only ban is resolved to a LID,
// its key is the phone JID.
type Ban struct {
	Member    string // LID, or the phone JID while no LID is known
	Scope     string // BanEverywhere or a community
	LID       string
	Phone     string
	Reason    string
	LedgerID  int64
	LIDFailed bool // a phone-to-LID lookup failed (and was reported)
	CreatedAt time.Time
}

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v) // a string slice always marshals
	return string(b)
}

// AddBan writes b inside tx (an existing entry for the same member and scope
// is kept, with any newly known LID or phone filled in).
func AddBan(ctx context.Context, tx *sql.Tx, b Ban, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO bans (member, scope, lid, phone, reason, ledger_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (member, scope) DO UPDATE SET
	lid = CASE WHEN bans.lid = '' THEN excluded.lid ELSE bans.lid END,
	phone = CASE WHEN bans.phone = '' THEN excluded.phone ELSE bans.phone END`,
		b.Member, b.Scope, b.LID, b.Phone, b.Reason, nullID(b.LedgerID), now.UnixMilli())
	if err != nil {
		return fmt.Errorf("ban insert: %w", err)
	}
	return nil
}

// FindBan returns the ban covering community for anyone known by one of ids
// (a LID or a phone JID), if any. community "" matches any ban.
func FindBan(ctx context.Context, q queryer, ids []string, community string) (Ban, bool, error) {
	row := q.QueryRowContext(ctx, `SELECT `+banCols+` FROM bans
WHERE (member IN (SELECT value FROM json_each(?1)) OR (lid != '' AND lid IN (SELECT value FROM json_each(?1)))
	OR (phone != '' AND phone IN (SELECT value FROM json_each(?1))))
	AND (?2 = '' OR scope = '*' OR scope = ?2)
ORDER BY scope = '*' DESC, created_at LIMIT 1`, jsonList(nonEmpty(ids)), community)
	b, err := scanBan(row)
	if err == sql.ErrNoRows {
		return b, false, nil
	}
	if err != nil {
		return b, false, fmt.Errorf("read ban list: %w", err)
	}
	return b, true, nil
}

// FindBan is FindBan on the database.
func (s *Store) FindBan(ctx context.Context, ids []string, community string) (Ban, bool, error) {
	return FindBan(ctx, s.db, ids, community)
}

func nonEmpty(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

const banCols = `member, scope, lid, phone, reason, COALESCE(ledger_id, 0), lid_failed, created_at`

func scanBan(sc interface{ Scan(...any) error }) (Ban, error) {
	var b Ban
	var created int64
	err := sc.Scan(&b.Member, &b.Scope, &b.LID, &b.Phone, &b.Reason, &b.LedgerID, &b.LIDFailed, &created)
	b.CreatedAt = time.UnixMilli(created)
	return b, err
}

// RemoveBans deletes every ban (any scope) of anyone known by one of ids
// inside tx, returning how many were removed.
func RemoveBans(ctx context.Context, tx *sql.Tx, ids []string) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM bans WHERE member IN (SELECT value FROM json_each(?1))
	OR (lid != '' AND lid IN (SELECT value FROM json_each(?1))) OR (phone != '' AND phone IN (SELECT value FROM json_each(?1)))`,
		jsonList(nonEmpty(ids)))
	if err != nil {
		return 0, fmt.Errorf("ban delete: %w", err)
	}
	return res.RowsAffected()
}

// Bans lists the whole ban list, oldest first.
func (s *Store) Bans(ctx context.Context) ([]Ban, error) {
	return s.bans(ctx, `SELECT `+banCols+` FROM bans ORDER BY created_at, member, scope`)
}

// BansFor lists the bans of anyone known by one of ids.
func (s *Store) BansFor(ctx context.Context, ids []string) ([]Ban, error) {
	return s.bans(ctx, `SELECT `+banCols+` FROM bans WHERE member IN (SELECT value FROM json_each(?1))
	OR (lid != '' AND lid IN (SELECT value FROM json_each(?1))) OR (phone != '' AND phone IN (SELECT value FROM json_each(?1)))
ORDER BY created_at`, jsonList(nonEmpty(ids)))
}

// UnresolvedBans lists bans that have a phone number but no LID and whose
// lookup has not failed yet.
func (s *Store) UnresolvedBans(ctx context.Context) ([]Ban, error) {
	return s.bans(ctx, `SELECT `+banCols+` FROM bans WHERE lid = '' AND phone != '' AND lid_failed = 0 ORDER BY created_at`)
}

func (s *Store) bans(ctx context.Context, query string, args ...any) ([]Ban, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read ban list: %w", err)
	}
	defer rows.Close()
	var out []Ban
	for rows.Next() {
		b, err := scanBan(rows)
		if err != nil {
			return nil, fmt.Errorf("read ban list: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ResolveBan records the LID found for a phone-only ban: the entry is re-keyed
// on the LID (merging with an existing LID entry of the same scope).
func (s *Store) ResolveBan(ctx context.Context, phone, lid string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE bans SET lid = ? WHERE phone = ? AND lid = ''`, lid, phone); err != nil {
			return err
		}
		// Re-key; where the LID already has an entry for that scope, keep it.
		if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE bans SET member = lid WHERE phone = ? AND member = phone`, phone); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM bans WHERE phone = ? AND member = phone`, phone)
		return err
	})
}

// MarkBanLIDFailed records that the phone number of a ban could not be
// resolved to a LID (the ban stays keyed on the phone number).
func (s *Store) MarkBanLIDFailed(ctx context.Context, phone string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE bans SET lid_failed = 1 WHERE phone = ? AND lid = ''`, phone)
		return err
	})
}

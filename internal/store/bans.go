package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// BanEntry is a person's ban list entry in one scope with the ledger row its
// link names (Linked false: it has no link, or the row was purged since).
type BanEntry struct {
	Ban
	Row    LedgerRow
	Linked bool
}

// Keeps reports whether a new ban (admin: an admin ordered it, AdminOrdered)
// leaves the entry's link where it is. An admin's ban outranks the bot's: a
// link to an admin's row always stays, a link to the bot's stays unless an
// admin bans them, and a link to no row always moves.
func (e BanEntry) Keeps(admin bool) bool {
	return e.Linked && (AdminOrdered(e.Row.TriggerID) || !admin)
}

// BanEntryIn returns the ban list entry in scope of the person known by lid
// and phone (either may be ""), read through q: one whose key or LID is lid,
// or one with no LID that holds phone; with no lid, any entry holding phone.
// An entry with another LID is another person, whatever number it holds. An
// entry found by the LID comes first, then one keyed by the number.
func BanEntryIn(ctx context.Context, q queryer, lid, phone, scope string) (BanEntry, bool, error) {
	var e BanEntry
	b, err := scanBan(q.QueryRowContext(ctx, `SELECT `+banCols+` FROM bans WHERE scope = ?3 AND (
	(?1 != '' AND (member = ?1 OR lid = ?1))
	OR (?2 != '' AND (phone = ?2 OR member = ?2) AND (?1 = '' OR lid = '')))
ORDER BY (?1 != '' AND (member = ?1 OR lid = ?1)) DESC, member = ?2 DESC, created_at LIMIT 1`, lid, phone, scope))
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	if err != nil {
		return e, false, fmt.Errorf("read ban list: %w", err)
	}
	e.Ban = b
	if b.LedgerID != 0 {
		if e.Row, e.Linked, err = oneRow(q.QueryRowContext(ctx, `SELECT `+ledgerCols+` FROM ledger WHERE id = ?`,
			b.LedgerID)); err != nil {
			return e, false, err
		}
	}
	return e, true, nil
}

// PutBan puts b (a scope, the key and addresses of a new entry, its ledger
// row) on the ban list in tx, one entry per person and scope. The entry the
// person known by lid and phone already has there (BanEntryIn) is kept: its
// link moves to b's row unless it keeps it (BanEntry.Keeps; admin: an admin
// ordered b), and the number or LID it lacks is filled in (a LID resolves it
// as the sweep would, ResolveBanIn). With no entry, b is added. added and
// moved report which happened.
func PutBan(ctx context.Context, tx *sql.Tx, b Ban, lid, phone string, admin bool, now time.Time) (added, moved bool, err error) {
	e, found, err := BanEntryIn(ctx, tx, lid, phone, b.Scope)
	if err != nil {
		return false, false, err
	}
	if !found {
		return true, false, AddBan(ctx, tx, b, now)
	}
	if moved = !e.Keeps(admin) && b.LedgerID != 0 && b.LedgerID != e.LedgerID; moved {
		if _, err := tx.ExecContext(ctx, `UPDATE bans SET ledger_id = ? WHERE member = ? AND scope = ?`,
			b.LedgerID, e.Member, e.Scope); err != nil {
			return false, false, fmt.Errorf("move ban link: %w", err)
		}
	}
	return false, moved, fill(ctx, tx, e, lid, phone)
}

// FillBan fills in the number or LID the ban list entry in scope of the person
// known by lid and phone lacks (BanEntryIn), in tx: a ban of someone already
// on the list that reuses their entry's row still records what it knows of
// them. It adds no entry.
func FillBan(ctx context.Context, tx *sql.Tx, lid, phone, scope string) error {
	e, found, err := BanEntryIn(ctx, tx, lid, phone, scope)
	if err != nil || !found {
		return err
	}
	return fill(ctx, tx, e, lid, phone)
}

// fill gives entry e the phone or LID it lacks; a LID resolves it as the
// sweep would (ResolveBanIn).
func fill(ctx context.Context, tx *sql.Tx, e BanEntry, lid, phone string) error {
	if e.Phone == "" && phone != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE bans SET phone = ? WHERE member = ? AND scope = ?`,
			phone, e.Member, e.Scope); err != nil {
			return fmt.Errorf("fill ban phone: %w", err)
		}
	}
	if e.LID == "" && lid != "" && e.Phone != "" {
		if err := ResolveBanIn(ctx, tx, e.Phone, lid); err != nil {
			return fmt.Errorf("resolve ban: %w", err)
		}
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

// RemoveBans deletes the bans of anyone known by one of ids that cover
// community (its own and an everywhere ban, as FindBan matches them; ""
// covers every scope) inside tx, returning how many were removed.
func RemoveBans(ctx context.Context, tx *sql.Tx, ids []string, community string) (int64, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM bans WHERE (member IN (SELECT value FROM json_each(?1))
	OR (lid != '' AND lid IN (SELECT value FROM json_each(?1))) OR (phone != '' AND phone IN (SELECT value FROM json_each(?1))))
	AND (?2 = '' OR scope = '*' OR scope = ?2)`,
		jsonList(nonEmpty(ids)), community)
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
	return BansForIn(ctx, s.db, ids)
}

// BansForIn is BansFor read through q (a transaction).
func BansForIn(ctx context.Context, q queryer, ids []string) ([]Ban, error) {
	return bans(ctx, q, `SELECT `+banCols+` FROM bans WHERE member IN (SELECT value FROM json_each(?1))
	OR (lid != '' AND lid IN (SELECT value FROM json_each(?1))) OR (phone != '' AND phone IN (SELECT value FROM json_each(?1)))
ORDER BY created_at`, jsonList(nonEmpty(ids)))
}

// UnresolvedBans lists bans that have a phone number but no LID and whose
// lookup has not failed yet (LIDFailedBans lists those whose lookup has).
func (s *Store) UnresolvedBans(ctx context.Context) ([]Ban, error) {
	return s.bans(ctx, `SELECT `+banCols+` FROM bans WHERE lid = '' AND phone != '' AND lid_failed = 0 ORDER BY created_at`)
}

// LIDFailedBans lists bans that have a phone number but no LID and whose
// lookup failed (MarkBanLIDFailed): the sweep looks each up again in the
// saved WhatsApp IDs.
func (s *Store) LIDFailedBans(ctx context.Context) ([]Ban, error) {
	return s.bans(ctx, `SELECT `+banCols+` FROM bans WHERE lid = '' AND phone != '' AND lid_failed = 1 ORDER BY created_at`)
}

func (s *Store) bans(ctx context.Context, query string, args ...any) ([]Ban, error) {
	return bans(ctx, s.db, query, args...)
}

func bans(ctx context.Context, q queryer, query string, args ...any) ([]Ban, error) {
	rows, err := q.QueryContext(ctx, query, args...)
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
// on the LID (merging with an existing LID entry of the same scope, which
// keeps an admin's link from either side, the LID entry's when both are an
// admin's), and a failed lookup it carried (lid_failed) is cleared.
func (s *Store) ResolveBan(ctx context.Context, phone, lid string) error {
	return s.Write(ctx, func(tx *sql.Tx) error { return ResolveBanIn(ctx, tx, phone, lid) })
}

// ResolveBanIn is ResolveBan inside tx.
func ResolveBanIn(ctx context.Context, tx *sql.Tx, phone, lid string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE bans SET lid = ?, lid_failed = 0 WHERE phone = ? AND lid = ''`, lid, phone); err != nil {
		return err
	}
	// Re-key; where the LID already has an entry for that scope, keep it,
	// with the phone number filled in so the ban still matches it.
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE bans SET member = lid WHERE phone = ? AND member = phone`, phone); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE bans SET phone = ?1 WHERE member = ?2 AND phone = ''
	AND scope IN (SELECT scope FROM bans WHERE phone = ?1 AND member = phone)`, phone, lid); err != nil {
		return err
	}
	// The kept entry takes the number entry's link when that one is an
	// admin's and its own is not (the bot's, or a row purged since): an
	// admin's ban outranks the bot's, as when it is written (PutBan).
	if _, err := tx.ExecContext(ctx, `UPDATE bans SET ledger_id = (
	SELECT n.ledger_id FROM bans n WHERE n.phone = ?1 AND n.member = n.phone AND n.scope = bans.scope)
WHERE member = ?2 AND NOT `+adminLinked("bans")+`
	AND EXISTS (SELECT 1 FROM bans n WHERE n.phone = ?1 AND n.member = n.phone AND n.scope = bans.scope
		AND `+adminLinked("n")+`)`, phone, lid); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM bans WHERE phone = ? AND member = phone`, phone)
	return err
}

// MarkBanLIDFailed records that the phone number of a ban could not be
// resolved to a LID (the ban stays keyed on the phone number) and stores the
// report r about it, in one transaction: once marked, the ban is looked up
// again only in the saved WhatsApp IDs and never reported again, so a report
// written apart could be lost for good. The report is written only when a ban
// was marked; reported says whether it was.
func (s *Store) MarkBanLIDFailed(ctx context.Context, phone string, r Report) (reported bool, err error) {
	err = s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE bans SET lid_failed = 1 WHERE phone = ? AND lid = ''`, phone)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		reported = n > 0
		if !reported {
			return nil
		}
		_, err = InsertReport(ctx, tx, r, nil, s.now())
		return err
	})
	return reported && err == nil, err
}

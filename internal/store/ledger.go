package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Action is what a ledger row records.
type Action string

const (
	ActRevoke Action = "revoke" // delete a message for everyone
	ActRemove Action = "remove" // remove a member from a group or community
	ActReject Action = "reject" // reject a pending join request
	ActBan    Action = "ban"    // add to the ban list (applied in the same transaction, or held by a pause)
	ActUnban  Action = "unban"  // lift a ban (applied in the same transaction)
)

// WhatsApp reports whether a is a WhatsApp call, fired from the outbox (a ban
// or unban changes only the ban list).
func (a Action) WhatsApp() bool {
	return a == ActRevoke || a == ActRemove || a == ActReject
}

// Status is where a ledger row is. WhatsApp never confirms a delete, so a sent
// action is "requested", never "succeeded".
type Status string

const (
	Intended    Status = "intended"     // written before the outbox fires
	Requested   Status = "requested"    // sent to WhatsApp (or, for ban/unban, applied)
	Failed      Status = "failed"       // not done; reason says why
	AlreadyGone Status = "already_gone" // the member was not there
	Overturned  Status = "overturned"   // an admin pressed [Undo]
)

// Ledger modes: a shadow row records what would have happened and never fires.
const (
	ModeShadow  = "shadow"
	ModeEnforce = "enforce"
)

// LedgerRow is one action, keyed action|chat|target|trigger_id.
type LedgerRow struct {
	ID         int64
	Action     Action
	Chat       string // group or community the action is in ("" for ban/unban)
	Target     string // member key: the LID, or the phone JID when no LID is known
	TriggerID  string // message ID, join event, sweep run, or report + button
	Community  string // the configured community this row counts towards
	Mode       string
	Status     Status
	Reason     string
	Rule       string
	Actor      string
	ConfigHash string
	// Address is the member's address as the chat knows it: a revoke's
	// author, a removal's participant, a rejection's requester.
	Address string
	// Message-triggered rows: the message to delete (revoke) and the server
	// time of the message an action targets (the original's, for an edit).
	MsgID   string
	MsgTime time.Time
	// EvidenceID links a message-triggered row to its evidence copy (0 = none).
	EvidenceID int64
	Code       int // WhatsApp's per-member error code, when it gave one
	Attempts   int
	SentAt     time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Key is the row's unique key.
func (r LedgerRow) Key() string {
	return string(r.Action) + "|" + r.Chat + "|" + r.Target + "|" + r.TriggerID
}

// Terminal reports whether nothing further happens to the row.
func (s Status) Terminal() bool { return s != Intended }

// queryer is a transaction or the database.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const ledgerCols = `id, action, chat, target, trigger_id, community, mode, status, reason, rule, actor, config_hash,
	address, msg_id, msg_time, COALESCE(evidence_id, 0), code, attempts, COALESCE(sent_at, 0), created_at, updated_at`

func scanLedger(sc interface{ Scan(...any) error }) (LedgerRow, error) {
	var r LedgerRow
	var msgTime, sent, created, updated int64
	err := sc.Scan(&r.ID, &r.Action, &r.Chat, &r.Target, &r.TriggerID, &r.Community, &r.Mode, &r.Status, &r.Reason,
		&r.Rule, &r.Actor, &r.ConfigHash, &r.Address, &r.MsgID, &msgTime, &r.EvidenceID, &r.Code, &r.Attempts, &sent,
		&created, &updated)
	if err != nil {
		return r, err
	}
	r.MsgTime, r.CreatedAt, r.UpdatedAt = msTime(msgTime), time.UnixMilli(created), time.UnixMilli(updated)
	r.SentAt = msTime(sent)
	return r, nil
}

// msTime turns a stored unix-ms value into a time; 0 stays the zero time.
func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func msOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// nullID stores 0 as NULL.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// InsertLedger writes r inside tx unless a row with the same key exists
// (inserted false, id of the existing row). An enforce row of a WhatsApp
// action is queued in the outbox in the same transaction, so a row is always
// written before it can fire. An intended ban row is a ban a pause holds: it
// is never queued (the executor settles it once no pause covers bans).
func InsertLedger(ctx context.Context, tx *sql.Tx, r LedgerRow, now time.Time) (id int64, inserted bool, err error) {
	if r.Status == "" {
		r.Status = Intended
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO ledger (key, action, chat, target, trigger_id, community, mode, status, reason, rule, actor, config_hash,
	address, msg_id, msg_time, evidence_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (key) DO NOTHING`, r.Key(), string(r.Action), r.Chat, r.Target, r.TriggerID, r.Community, r.Mode,
		string(r.Status), r.Reason, r.Rule, r.Actor, r.ConfigHash, r.Address, r.MsgID, msOf(r.MsgTime),
		nullID(r.EvidenceID), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return 0, false, fmt.Errorf("ledger insert: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		err := tx.QueryRowContext(ctx, `SELECT id FROM ledger WHERE key = ?`, r.Key()).Scan(&id)
		return id, false, err
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, false, err
	}
	if r.Mode == ModeEnforce && r.Status == Intended && r.Action.WhatsApp() {
		if err := enqueue(ctx, tx, id, r.Action, now); err != nil {
			return 0, false, err
		}
	}
	return id, true, nil
}

// ScopeOf is the pause scope that stops an action: deletes only stop for a
// full pause, removals and rejections for either.
func ScopeOf(a Action) Scope {
	if a == ActRevoke {
		return ScopeAll
	}
	return ScopeRemoveBan
}

func enqueue(ctx context.Context, q queryer, id int64, a Action, now time.Time) error {
	if !a.WhatsApp() {
		return fmt.Errorf("ledger: %s is not a WhatsApp action and cannot be queued", a)
	}
	_, err := q.ExecContext(ctx, `INSERT INTO outbox (ledger_id, scope, not_before) VALUES (?, ?, ?)
ON CONFLICT (ledger_id) DO NOTHING`, id, string(ScopeOf(a)), now.UnixMilli())
	if err != nil {
		return fmt.Errorf("outbox insert: %w", err)
	}
	return nil
}

// Ledger reads one row.
func (s *Store) Ledger(ctx context.Context, id int64) (LedgerRow, error) {
	return scanLedger(s.db.QueryRowContext(ctx, `SELECT `+ledgerCols+` FROM ledger WHERE id = ?`, id))
}

// NextDue returns the oldest queued action that may fire at now: deletes
// first, and only the scopes the caller allows (a pause holds the rest).
func (s *Store) NextDue(ctx context.Context, now time.Time, allowDelete, allowRemove bool) (LedgerRow, bool, error) {
	if !allowDelete && !allowRemove {
		return LedgerRow{}, false, nil
	}
	r, err := scanLedger(s.db.QueryRowContext(ctx, `
SELECT `+ledgerCols+` FROM ledger WHERE id = (
	SELECT o.ledger_id FROM outbox o
	WHERE o.not_before <= ? AND ((o.scope = 'all' AND ?) OR (o.scope = 'remove_ban' AND ?))
	ORDER BY o.scope = 'all' DESC, o.not_before, o.ledger_id LIMIT 1)`, now.UnixMilli(), allowDelete, allowRemove))
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("read outbox: %w", err)
	}
	return r, true, nil
}

// NextWake is when the earliest queued action of the scopes the caller
// allows becomes due (ok false when there is none): an action a pause holds
// is not due, however old.
func (s *Store) NextWake(ctx context.Context, allowDelete, allowRemove bool) (time.Time, bool, error) {
	var at sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(not_before) FROM outbox
WHERE (scope = 'all' AND ?) OR (scope = 'remove_ban' AND ?)`, allowDelete, allowRemove).Scan(&at); err != nil {
		return time.Time{}, false, fmt.Errorf("read outbox: %w", err)
	}
	return time.UnixMilli(at.Int64), at.Valid, nil
}

// OutboxLen counts queued actions (healthcheck prints it).
func (s *Store) OutboxLen(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox`).Scan(&n)
	return n, err
}

// Finish records a row's final status and drops it from the outbox in one
// transaction (an outbox row lives only while its action is pending). Only a
// row still at intended is settled: when [Undo] overturned it while its call
// was in flight, the row keeps that status and settled is false, but a call
// that went out (sent set) is still recorded on it, so the breaker counts it.
func (s *Store) Finish(ctx context.Context, id int64, st Status, reason string, code int, sent time.Time) (settled bool, err error) {
	err = s.Write(ctx, func(tx *sql.Tx) error {
		settled, err = FinishIn(ctx, tx, id, st, reason, code, sent, s.now())
		return err
	})
	return settled, err
}

// FinishIn is Finish inside tx.
func FinishIn(ctx context.Context, tx *sql.Tx, id int64, st Status, reason string, code int, sent, now time.Time) (bool, error) {
	if !st.Terminal() {
		return false, fmt.Errorf("ledger: %s is not a final status", st)
	}
	res, err := tx.ExecContext(ctx, `UPDATE ledger SET status = ?, reason = ?, code = ?, sent_at = COALESCE(?, sent_at),
	updated_at = ? WHERE id = ? AND status = 'intended'`, string(st), reason, code, nullTime(sent), now.UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	settled := n == 1
	if !settled && !sent.IsZero() {
		if _, err := tx.ExecContext(ctx, `UPDATE ledger SET code = ?, sent_at = ?, updated_at = ? WHERE id = ?`,
			code, sent.UnixMilli(), now.UnixMilli(), id); err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM outbox WHERE ledger_id = ?`, id)
	return settled, err
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// Retry keeps a row queued until notBefore, counting the attempt.
func (s *Store) Retry(ctx context.Context, id int64, notBefore time.Time, reason string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE ledger SET attempts = attempts + 1, reason = ?, updated_at = ? WHERE id = ?`,
			reason, s.now().UnixMilli(), id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE outbox SET not_before = ? WHERE ledger_id = ?`, notBefore.UnixMilli(), id)
		return err
	})
}

// ToShadowIn turns a queued row into a shadow record (its target scope is no
// longer in enforce mode) and drops it from the outbox, inside tx. Only a row
// still at intended changes ([Undo] may have overturned it meanwhile); changed
// says whether it did.
func ToShadowIn(ctx context.Context, tx *sql.Tx, id int64, reason string, now time.Time) (changed bool, err error) {
	res, err := tx.ExecContext(ctx, `UPDATE ledger SET mode = 'shadow', reason = ?, updated_at = ?
WHERE id = ? AND status = 'intended'`, reason, now.UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM outbox WHERE ledger_id = ?`, id)
	return n == 1, err
}

// AdminTrigger prefixes the trigger of a row an admin ordered from the admin
// chat ("tg:<report>:<button>"): the report ID + button key of AC 3.1.
const AdminTrigger = "tg:"

// Overturn marks each given row that is still intended or requested as
// overturned (an admin pressed [Undo]) and drops it from the outbox, in tx.
// It returns how many rows changed.
func Overturn(ctx context.Context, tx *sql.Tx, ids []int64, reason string, now time.Time) (int64, error) {
	var n int64
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `UPDATE ledger SET status = 'overturned', reason = ?, updated_at = ?
WHERE id = ? AND status IN ('intended', 'requested')`, reason, now.UnixMilli(), id)
		if err != nil {
			return n, fmt.Errorf("overturn: %w", err)
		}
		c, _ := res.RowsAffected()
		n += c
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE ledger_id = ?`, id); err != nil {
			return n, fmt.Errorf("overturn: %w", err)
		}
	}
	return n, nil
}

// Requeue puts a failed row back in the outbox (a sweep retrying the same
// target and action stays one row per episode).
func Requeue(ctx context.Context, tx *sql.Tx, r LedgerRow, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE ledger SET status = 'intended', updated_at = ? WHERE id = ?`,
		now.UnixMilli(), r.ID); err != nil {
		return err
	}
	return enqueue(ctx, tx, r.ID, r.Action, now)
}

// EnsureQueued puts every given enforce row that is still intended back in
// the outbox if it is missing there, due now.
func (s *Store) EnsureQueued(ctx context.Context, rows []LedgerRow) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			if r.Status != Intended || r.Mode != ModeEnforce {
				continue
			}
			if err := enqueue(ctx, tx, r.ID, r.Action, s.now()); err != nil {
				return err
			}
		}
		return nil
	})
}

// LatestFor returns the newest row in mode for an action on a target in a
// chat whose trigger starts with prefix (ok false when there is none).
func LatestFor(ctx context.Context, tx *sql.Tx, a Action, chat, target, prefix, mode string) (LedgerRow, bool, error) {
	return oneRow(tx.QueryRowContext(ctx, `SELECT `+ledgerCols+` FROM ledger
WHERE action = ? AND chat = ? AND target = ? AND mode = ? AND substr(trigger_id, 1, ?) = ? ORDER BY id DESC LIMIT 1`,
		string(a), chat, target, mode, len(prefix), prefix))
}

// OpenFor returns the open (intended) enforce row of an action on a target in
// a chat, whatever triggered it (ok false when there is none).
func OpenFor(ctx context.Context, tx *sql.Tx, a Action, chat, target string) (LedgerRow, bool, error) {
	return oneRow(tx.QueryRowContext(ctx, `SELECT `+ledgerCols+` FROM ledger
WHERE action = ? AND chat = ? AND target = ? AND mode = 'enforce' AND status = 'intended' ORDER BY id LIMIT 1`,
		string(a), chat, target))
}

// RevokeOf returns the enforce delete of target's message msgID in chat that
// is queued or already sent (ok false when there is none).
func RevokeOf(ctx context.Context, tx *sql.Tx, chat, target, msgID string) (LedgerRow, bool, error) {
	return oneRow(tx.QueryRowContext(ctx, `SELECT `+ledgerCols+` FROM ledger
WHERE target = ? AND action = 'revoke' AND chat = ? AND msg_id = ? AND mode = 'enforce'
	AND status IN ('intended', 'requested') ORDER BY id LIMIT 1`, target, chat, msgID))
}

// heldBan selects the bans a pause holds: never queued, settled by the
// executor once no pause covers bans, and kept by every purge until then.
const heldBan = `(action = 'ban' AND status = 'intended' AND mode = 'enforce')`

// BanRowOf returns the ledger row of target's ban in scope when the ban is in
// force (its ban list entry) or, when orHeld, held by a pause (ok false when
// neither). A ban meant to apply at once (an admin's) passes orHeld false: a
// held ban must not swallow it.
func BanRowOf(ctx context.Context, tx *sql.Tx, target, scope string, orHeld bool) (LedgerRow, bool, error) {
	return oneRow(tx.QueryRowContext(ctx, `SELECT `+ledgerCols+` FROM ledger WHERE id = COALESCE(
	(SELECT ledger_id FROM bans WHERE member = ?1 AND scope = ?2 AND ledger_id IS NOT NULL),
	(SELECT id FROM ledger WHERE ?3 AND `+heldBan+` AND target = ?1 AND chat = ?2 ORDER BY id LIMIT 1))`,
		target, scope, orHeld))
}

func oneRow(row *sql.Row) (LedgerRow, bool, error) {
	r, err := scanLedger(row)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("read ledger: %w", err)
	}
	return r, true, nil
}

// SentSince counts removals and rejections sent after since (the circuit
// breaker's window, or the last [Resume] when that is later).
func (s *Store) SentSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger WHERE action IN ('remove', 'reject') AND sent_at > ?`,
		since.UnixMilli()).Scan(&n)
	return n, err
}

// StaleIntended lists enforce WhatsApp actions still at intended (after a
// crash, these are re-checked and retried; a held ban is not one of them).
func (s *Store) StaleIntended(ctx context.Context) ([]LedgerRow, error) {
	return s.ledgerRows(ctx, `SELECT `+ledgerCols+` FROM ledger WHERE status = 'intended' AND mode = 'enforce'
	AND action IN ('revoke', 'remove', 'reject') ORDER BY id`)
}

// NextHeldBan returns the oldest ban a pause holds (ok false when none).
func (s *Store) NextHeldBan(ctx context.Context) (LedgerRow, bool, error) {
	return oneRow(s.db.QueryRowContext(ctx, `SELECT `+ledgerCols+` FROM ledger WHERE `+heldBan+` ORDER BY id LIMIT 1`))
}

// PostsOf lists the evidence copies ledger row id stands for: its own and
// those of every report linked to it (a later post by the same member that
// reused the row). Copies already purged or forgotten are not listed.
func (s *Store) PostsOf(ctx context.Context, id int64) ([]int64, error) {
	return idList(ctx, s.db, `SELECT e.id FROM evidence e WHERE e.id IN (
	SELECT evidence_id FROM ledger WHERE id = ?1
	UNION SELECT r.evidence_id FROM reports r JOIN report_ledger rl ON rl.report_id = r.id WHERE rl.ledger_id = ?1)
ORDER BY e.id`, id)
}

// ApplyHeldBan applies a ban a pause held: its target goes on the ban list in
// each of scopes (the current config's, for the row's community) and the row
// is marked requested. applied is false when the row was no longer held.
func (s *Store) ApplyHeldBan(ctx context.Context, row LedgerRow, scopes []string) (applied bool, err error) {
	err = s.Write(ctx, func(tx *sql.Tx) error {
		now := s.now()
		res, err := tx.ExecContext(ctx, `UPDATE ledger SET status = 'requested', updated_at = ? WHERE id = ? AND status = 'intended'`,
			now.UnixMilli(), row.ID)
		if err != nil {
			return fmt.Errorf("apply held ban: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return err
		}
		for _, scope := range scopes {
			b := Ban{Member: row.Target, Scope: scope, Phone: row.Address, Reason: row.Reason, LedgerID: row.ID}
			if strings.HasSuffix(row.Target, "@lid") {
				b.LID = row.Target
			} else {
				b.Phone = row.Target
			}
			if err := AddBan(ctx, tx, b, now); err != nil {
				return err
			}
		}
		applied = true
		return nil
	})
	return applied, err
}

// FailHeldBan fails a ban a pause held: no post it stands for counts any more.
func (s *Store) FailHeldBan(ctx context.Context, id int64, reason string) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE ledger SET status = 'failed', reason = ?, updated_at = ? WHERE id = ? AND status = 'intended'`,
			reason, s.now().UnixMilli(), id)
		if err != nil {
			return fmt.Errorf("fail held ban: %w", err)
		}
		return nil
	})
}

// SettleHeldBansIn marks requested every ban a pause holds for target that
// scope covers: an admin's ban (ledger row id) put the member on the ban list
// there at once. It runs in tx.
func SettleHeldBansIn(ctx context.Context, tx *sql.Tx, target, scope string, id int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE ledger SET status = 'requested', reason = reason || ?1, updated_at = ?2
WHERE `+heldBan+` AND target = ?3 AND (?4 = '*' OR chat = ?4)`,
		fmt.Sprintf(" (in force through ledger row %d)", id), now.UnixMilli(), target, scope)
	if err != nil {
		return fmt.Errorf("settle held bans: %w", err)
	}
	return nil
}

// DropHeldBans overturns every ban a pause holds for any of targets that
// covers community (as RemoveBans; "" covers every scope): the member was
// unbanned there before the ban was applied. It runs in tx.
func DropHeldBans(ctx context.Context, tx *sql.Tx, targets []string, community, reason string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE ledger SET status = 'overturned', reason = ?1, updated_at = ?2
WHERE action = 'ban' AND status = 'intended' AND target IN (SELECT value FROM json_each(?3))
	AND (?4 = '' OR chat = '*' OR chat = ?4)`,
		reason, now.UnixMilli(), jsonList(targets), community)
	if err != nil {
		return fmt.Errorf("drop held bans: %w", err)
	}
	return nil
}

// HeldBan reports whether a pause holds a ban for any of targets that covers
// community (as FindBan; "" for any scope).
func (s *Store) HeldBan(ctx context.Context, targets []string, community string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ledger WHERE `+heldBan+`
	AND target IN (SELECT value FROM json_each(?1)) AND (?2 = '' OR chat = '*' OR chat = ?2)`,
		jsonList(targets), community).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("read held bans: %w", err)
	}
	return n > 0, nil
}

// HeldBans lists every ban a pause holds, oldest first (`ban list` shows them
// beside the ban list: they apply after [Resume]).
func (s *Store) HeldBans(ctx context.Context) ([]LedgerRow, error) {
	return s.ledgerRows(ctx, `SELECT `+ledgerCols+` FROM ledger WHERE `+heldBan+` ORDER BY id`)
}

// LedgerForTargets lists every row about any of the given member keys.
func (s *Store) LedgerForTargets(ctx context.Context, targets []string) ([]LedgerRow, error) {
	return LedgerForTargetsIn(ctx, s.db, targets)
}

// LedgerForTargetsIn is LedgerForTargets read through q (a transaction).
func LedgerForTargetsIn(ctx context.Context, q queryer, targets []string) ([]LedgerRow, error) {
	return ledgerRows(ctx, q, `SELECT `+ledgerCols+` FROM ledger WHERE target IN (SELECT value FROM json_each(?)) ORDER BY id`,
		jsonList(targets))
}

// LedgerForReport lists the rows a report is about.
func (s *Store) LedgerForReport(ctx context.Context, reportID int64) ([]LedgerRow, error) {
	return LedgerForReportIn(ctx, s.db, reportID)
}

// LedgerForReportIn is LedgerForReport read through q (a transaction).
func LedgerForReportIn(ctx context.Context, q queryer, reportID int64) ([]LedgerRow, error) {
	return ledgerRows(ctx, q, `SELECT `+ledgerCols+` FROM ledger WHERE id IN (
	SELECT ledger_id FROM report_ledger WHERE report_id = ?) ORDER BY id`, reportID)
}

func (s *Store) ledgerRows(ctx context.Context, query string, args ...any) ([]LedgerRow, error) {
	return ledgerRows(ctx, s.db, query, args...)
}

func ledgerRows(ctx context.Context, q queryer, query string, args ...any) ([]LedgerRow, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	defer rows.Close()
	var out []LedgerRow
	for rows.Next() {
		r, err := scanLedger(rows)
		if err != nil {
			return nil, fmt.Errorf("read ledger: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SummaryRow is one count of `ledger summary`.
type SummaryRow struct {
	Community string
	Mode      string
	Action    Action
	Count     int
}

// LedgerSummary counts rows per community, mode and action.
func (s *Store) LedgerSummary(ctx context.Context) ([]SummaryRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT community, mode, action, COUNT(*) FROM ledger
GROUP BY community, mode, action ORDER BY community, mode, action`)
	if err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	defer rows.Close()
	var out []SummaryRow
	for rows.Next() {
		var r SummaryRow
		if err := rows.Scan(&r.Community, &r.Mode, &r.Action, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PurgeLedger deletes rows created before cutoff that are neither queued nor
// a held ban, with their report links.
func (s *Store) PurgeLedger(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM ledger WHERE created_at < ? AND id NOT IN (SELECT ledger_id FROM outbox)
	AND NOT `+heldBan, cutoff.UnixMilli())
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		_, err = tx.ExecContext(ctx, `DELETE FROM report_ledger WHERE ledger_id NOT IN (SELECT id FROM ledger)`)
		return err
	})
	return n, err
}

// PurgeOutbox drops outbox rows whose ledger row is already final (they are
// normally dropped with the status update; this clears any left behind).
func (s *Store) PurgeOutbox(ctx context.Context) (int64, error) {
	var n int64
	err := s.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE ledger_id IN (
	SELECT id FROM ledger WHERE status != 'intended' OR mode != 'enforce')`)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, err
}

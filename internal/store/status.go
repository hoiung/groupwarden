package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// Status keys written by `run` and read by `healthcheck` and other commands.
const (
	StatusHeartbeat      = "heartbeat"       // run is alive (value unused; updated_at is the beat)
	StatusConnected      = "connected"       // "1" or "0"
	StatusLastEvent      = "last_event"      // unix ms of the last event from any group
	StatusDeaf           = "deaf"            // "1" while no event arrived for deafness_alert_hours
	StatusConfigHash     = "config_hash"     // hash of the loaded config
	StatusCompanionsSeen = "companions_seen" // linked devices already reported (comma list)
	StatusBreakerReset   = "breaker_reset"   // unix ms of the last [Resume]: the breaker counts from there
	// The admin chat.
	StatusTelegramOK   = "tg_ok"          // "1" once Telegram took a message; "0" while it keeps refusing the bot (401/403)
	StatusTelegramChat = "tg_chat_id"     // the chat ID after Telegram migrated the group (overrides the secrets file)
	StatusSummaryDay   = "tg_summary_day" // the last UTC day (2006-01-02) the daily summary covered
	// The pinned command list: "<chat ID>:<message ID>", the hash of the
	// text it shows, and "pinned" / "refused" ("" until a pin was tried).
	StatusTelegramPin      = "tg_pin"
	StatusTelegramPinHash  = "tg_pin_hash"
	StatusTelegramPinState = "tg_pin_state"
	// The "open WhatsApp on the bot phone" reminder: unix ms of the last
	// [Done] (of the first start, before any [Done]), and the [Done] time each
	// reminder and escalation was sent for (one of each per episode).
	StatusPhoneDone      = "phone_done"
	StatusPhoneReminded  = "phone_reminded"
	StatusPhoneEscalated = "phone_escalated"
	// The last local day (2006-01-02) the daily check was posted.
	StatusDailyCheckDay = "daily_check_day"
	// Timers outside the bot (config sync, backup): unix ms of the last run
	// and its result line.
	StatusSyncLastRun   = "sync_last_run"
	StatusSyncResult    = "sync_result"
	StatusBackupLastRun = "backup_last_run"
	StatusBackupResult  = "backup_result"
)

// StatusValue is one status entry.
type StatusValue struct {
	Value     string
	UpdatedAt time.Time
}

// StatusTime is how a status value holds a time (the keys above marked unix
// ms): its unix milliseconds. StatusValue.Time reads it back.
func StatusTime(t time.Time) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}

// Time reads a value written with StatusTime: the zero time when the key is
// absent, unreadable or zero.
func (v StatusValue) Time() time.Time {
	ms, err := strconv.ParseInt(v.Value, 10, 64)
	if err != nil || ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// SetStatus writes the given keys in one transaction.
func (s *Store) SetStatus(ctx context.Context, kv map[string]string) error {
	return s.SetStatusReporting(ctx, kv, nil)
}

// SetStatusReporting writes the given keys and, when r is not nil, the report
// r in one transaction. A failure report and the status that records it was
// raised stand or fall together: written apart, a status write failing after
// the report would raise it again on the next run.
func (s *Store) SetStatusReporting(ctx context.Context, kv map[string]string, r *Report) error {
	now := s.now()
	return s.Write(ctx, func(tx *sql.Tx) error {
		for k, v := range kv {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO status (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, k, v, now.UnixMilli()); err != nil {
				return err
			}
		}
		if r == nil {
			return nil
		}
		_, err := InsertReport(ctx, tx, *r, nil, now)
		return err
	})
}

// Status reads every status entry.
func (s *Store) Status(ctx context.Context) (map[string]StatusValue, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value, updated_at FROM status`)
	if err != nil {
		return nil, fmt.Errorf("read status: %w", err)
	}
	defer rows.Close()
	out := map[string]StatusValue{}
	for rows.Next() {
		var k, v string
		var at int64
		if err := rows.Scan(&k, &v, &at); err != nil {
			return nil, fmt.Errorf("read status: %w", err)
		}
		out[k] = StatusValue{Value: v, UpdatedAt: time.UnixMilli(at)}
	}
	return out, rows.Err()
}

// IncrCounter adds one to today's counter name inside tx (daily summary data).
func IncrCounter(ctx context.Context, tx *sql.Tx, day, name string) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO counters (day, name, n) VALUES (?, ?, 1)
ON CONFLICT (day, name) DO UPDATE SET n = n + 1`, day, name)
	return err
}

// Counter reads one day's counter.
func (s *Store) Counter(ctx context.Context, day, name string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT n FROM counters WHERE day = ? AND name = ?`, day, name).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

// Counters reads every counter of one day, by name.
func (s *Store) Counters(ctx context.Context, day string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, n FROM counters WHERE day = ?`, day)
	if err != nil {
		return nil, fmt.Errorf("read counters: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, fmt.Errorf("read counters: %w", err)
		}
		out[name] = n
	}
	return out, rows.Err()
}

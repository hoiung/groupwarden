package store

import (
	"context"
	"database/sql"
	"fmt"
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
)

// StatusValue is one status entry.
type StatusValue struct {
	Value     string
	UpdatedAt time.Time
}

// SetStatus writes the given keys in one transaction.
func (s *Store) SetStatus(ctx context.Context, kv map[string]string) error {
	now := s.now().UnixMilli()
	return s.Write(ctx, func(tx *sql.Tx) error {
		for k, v := range kv {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO status (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, k, v, now); err != nil {
				return err
			}
		}
		return nil
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

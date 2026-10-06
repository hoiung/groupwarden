package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Where the bot stands in a group of a configured community
// (internal/reconcile coverage.go has the transitions).
const (
	CoverageAbsent   = "absent"    // the bot is not in the group
	CoverageNotAdmin = "not_admin" // in it, but not an admin: it cannot act there
	CoverageCovered  = "covered"   // an admin: moderated
)

// CoverageRow is where the bot stood in one group of a configured community
// when the admins were last told.
type CoverageRow struct {
	Group     string
	Community string
	State     string
	// Listed: a sweep has named the group to the admins (in its community's
	// first list, or as a new group).
	Listed bool
	// FewAdmins: the group has fewer than 2 human admins and the admins were
	// told (one alert per episode).
	FewAdmins bool
	// JoinTried: the bot tried to join the group by itself, or has been in
	// it; it never joins that group by itself again.
	JoinTried bool
	UpdatedAt time.Time
}

// CoverageRows reads every coverage row, by group.
func CoverageRows(ctx context.Context, q queryer) (map[string]CoverageRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT grp, community, state, listed, few_admins, join_tried, updated_at FROM coverage`)
	if err != nil {
		return nil, fmt.Errorf("read coverage: %w", err)
	}
	defer rows.Close()
	out := map[string]CoverageRow{}
	for rows.Next() {
		var r CoverageRow
		var at int64
		if err := rows.Scan(&r.Group, &r.Community, &r.State, &r.Listed, &r.FewAdmins, &r.JoinTried, &at); err != nil {
			return nil, fmt.Errorf("read coverage: %w", err)
		}
		r.UpdatedAt = time.UnixMilli(at)
		out[r.Group] = r
	}
	return out, rows.Err()
}

// CoverageRows reads every coverage row, by group.
func (s *Store) CoverageRows(ctx context.Context) (map[string]CoverageRow, error) {
	return CoverageRows(ctx, s.db)
}

// PutCoverage writes r whole (inserted or replaced) inside tx.
func PutCoverage(ctx context.Context, tx *sql.Tx, r CoverageRow, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO coverage (grp, community, state, listed, few_admins, join_tried, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (grp) DO UPDATE SET community = excluded.community, state = excluded.state, listed = excluded.listed,
	few_admins = excluded.few_admins, join_tried = excluded.join_tried, updated_at = excluded.updated_at`,
		r.Group, r.Community, r.State, r.Listed, r.FewAdmins, r.JoinTried, now.UnixMilli())
	if err != nil {
		return fmt.Errorf("write coverage: %w", err)
	}
	return nil
}

// DeleteCoverage removes the row of group inside tx.
func DeleteCoverage(ctx context.Context, tx *sql.Tx, group string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM coverage WHERE grp = ?`, group); err != nil {
		return fmt.Errorf("delete coverage: %w", err)
	}
	return nil
}

// MarkCoverage records inside tx, from an event or a refusal, that the bot is
// now state in group. The row keeps its other flags and gains join_tried (the
// bot has been in the group, so it never joins it by itself again); a group
// no sweep listed yet stays unlisted, so its community's first list still
// names it.
func MarkCoverage(ctx context.Context, tx *sql.Tx, group, community, state string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO coverage (grp, community, state, join_tried, updated_at) VALUES (?, ?, ?, 1, ?)
ON CONFLICT (grp) DO UPDATE SET community = excluded.community, state = excluded.state, join_tried = 1,
	updated_at = excluded.updated_at`, group, community, state, now.UnixMilli())
	if err != nil {
		return fmt.Errorf("mark coverage: %w", err)
	}
	return nil
}

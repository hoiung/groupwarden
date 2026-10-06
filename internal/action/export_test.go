package action

import (
	"context"
	"database/sql"
	"time"

	"github.com/hoiung/groupwarden/internal/store"
)

// Settle writes a row's change and the report about it together, as a step
// does.
func (x *Executor) Settle(ctx context.Context, rowID int64, change func(tx *sql.Tx, now time.Time) (bool, error),
	rep *store.Report) (bool, error) {
	return x.settle(ctx, rowID, change, rep)
}

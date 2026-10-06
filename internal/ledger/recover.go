package ledger

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/store"
)

// Recover runs at startup. Enforce rows still at intended were written but
// may not have fired before a crash: they stay queued, so the executor
// re-checks and retries them (every WhatsApp action groupwarden takes is safe
// to repeat), and one startup report lists them. Reports not delivered
// before the stop stay in the store; the admin chat sends them.
func Recover(ctx context.Context, st *store.Store, log *slog.Logger) (int, error) {
	stale, err := st.StaleIntended(ctx)
	if err != nil {
		return 0, err
	}
	if len(stale) == 0 {
		return 0, nil
	}
	counts := map[store.Action]int{}
	ids := make([]int64, 0, len(stale))
	for _, r := range stale {
		counts[r.Action]++
		ids = append(ids, r.ID)
	}
	if err := st.EnsureQueued(ctx, stale); err != nil {
		return 0, err
	}
	parts := make([]string, 0, len(counts))
	for a, n := range counts {
		parts = append(parts, fmt.Sprintf("%s %d", a, n))
	}
	sort.Strings(parts)
	text := fmt.Sprintf("Restarted with %d action(s) that may not have been sent before the stop (%s). "+
		"Each is checked again and retried.", len(stale), strings.Join(parts, ", "))
	if _, err := st.AddReport(ctx, store.Report{Kind: KindStartup, Text: text}, ids); err != nil {
		return 0, err
	}
	log.Warn("retrying actions left at intended", "count", len(stale), "by_action", mask.IDs(strings.Join(parts, ", ")))
	return len(stale), nil
}

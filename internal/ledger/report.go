package ledger

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/store"
)

// reportPoll is how often the reporter looks for undelivered reports without
// being woken (a failed delivery is retried then).
const reportPoll = 30 * time.Second

// Reporter delivers stored reports to the admins. A report is stored with the
// decision, so one not delivered before a crash is sent after the restart.
type Reporter struct {
	Store   *store.Store
	Alerter alert.Alerter
	Log     *slog.Logger

	wake chan struct{}
}

// NewReporter returns a reporter over st delivering through a.
func NewReporter(st *store.Store, a alert.Alerter, log *slog.Logger) *Reporter {
	return &Reporter{Store: st, Alerter: a, Log: log, wake: make(chan struct{}, 1)}
}

// Wake asks the reporter to deliver now.
func (r *Reporter) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run delivers every undelivered report (those left by a restart first), then
// each new one as it is stored, until ctx ends.
func (r *Reporter) Run(ctx context.Context) {
	for {
		if err := r.Deliver(ctx); err != nil && ctx.Err() == nil {
			r.Log.Error("report delivery failed; retrying", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-time.After(reportPoll):
		}
	}
}

// Deliver sends every undelivered report, priority first. It stops at the
// first failure (the rest wait for the next attempt).
func (r *Reporter) Deliver(ctx context.Context) error {
	for {
		pending, err := r.Store.UnsentReports(ctx, 50)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		for _, rep := range pending {
			if err := r.Alerter.Alert(ctx, alert.Alert{Kind: alert.Report, Priority: rep.Priority, Text: rep.Text,
				ReportID: rep.ID, Buttons: rep.Buttons}); err != nil {
				return fmt.Errorf("deliver report %d: %w", rep.ID, err)
			}
			if err := r.Store.MarkReportSent(ctx, rep.ID); err != nil {
				return err
			}
		}
	}
}

// Recover runs at startup. Enforce rows still at intended were written but
// may not have fired before a crash: they stay queued, so the executor
// re-checks and retries them (every WhatsApp action groupwarden takes is safe
// to repeat), and one startup report lists them.
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

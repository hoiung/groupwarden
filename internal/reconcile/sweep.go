// Package reconcile runs the periodic sweep over every moderated group: it
// removes banned members the bot finds there, rejects their join requests
// and resolves phone-only bans to LIDs, backing off when WhatsApp says the
// bot is going too fast.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/mask"
	"github.com/hoiung/groupwarden/internal/pipeline"
)

// Back-off on WhatsApp's rate-limit error (fixed, not tunables).
const (
	backoffBase = 30 * time.Second
	backoffMax  = 15 * time.Minute
	maxBackoffs = 6
)

// Sweep is one pass over the moderated groups.
type Sweep struct {
	Enforcer  *pipeline.Enforcer
	Directory *pipeline.Directory
	Config    *config.Holder
	Log       *slog.Logger
	// Sleep waits for d or until ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Result counts what one sweep did.
type Result struct {
	Groups, RateLimited, Errors int
}

// Run sweeps every moderated group once; runID names the pass in the ledger
// (a retry of the same removal in a later pass stays one row per episode).
func (s *Sweep) Run(ctx context.Context, runID string) (Result, error) {
	var res Result
	rs := s.Config.Current().Rules
	for _, g := range s.Directory.Moderated(rs) {
		res.Groups++
		if err := s.call(ctx, &res, func() error { return s.Enforcer.CheckPresent(ctx, g, runID) }); err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			s.Log.Error("sweep: banned-member check failed", "group", mask.IDs(string(g)), "err", mask.IDs(err.Error()))
		}
		if err := s.call(ctx, &res, func() error { return s.Enforcer.CheckJoinRequests(ctx, g, runID) }); err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			s.Log.Error("sweep: join-request check failed", "group", mask.IDs(string(g)), "err", mask.IDs(err.Error()))
		}
	}
	if err := s.call(ctx, &res, func() error { return s.Enforcer.ResolveBans(ctx) }); err != nil && ctx.Err() == nil {
		s.Log.Error("sweep: ban LID lookup failed", "err", mask.IDs(err.Error()))
	}
	return res, ctx.Err()
}

// call runs fn, waiting and trying again (with doubling waits) while
// WhatsApp answers with its rate-limit error.
func (s *Sweep) call(ctx context.Context, res *Result, fn func() error) error {
	wait := backoffBase
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil || !errors.Is(err, client.ErrRateLimited) {
			if err != nil {
				res.Errors++
			}
			return err
		}
		res.RateLimited++
		if attempt >= maxBackoffs {
			res.Errors++
			return fmt.Errorf("still rate limited after %d waits: %w", attempt, err)
		}
		s.Log.Warn("WhatsApp rate limit during the sweep; backing off", "for", wait)
		if err := s.Sleep(ctx, wait); err != nil {
			return err
		}
		wait = min(wait*2, backoffMax)
	}
}

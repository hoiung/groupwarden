// Package reconcile runs the periodic sweep over every configured community:
// it finds where the bot stands in each group (joining linked groups by
// itself where it can) and tells the admins what changed, then removes
// banned members the bot finds in the groups it moderates, rejects their
// join requests and resolves phone-only bans to LIDs, backing off when
// WhatsApp says the bot is going too fast.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
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
	// Now is the clock (nil: time.Now).
	Now func() time.Time

	mu    sync.Mutex
	at    time.Time // when lists was taken
	lists []listing
}

func (s *Sweep) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// Result counts what one sweep did: groups checked for banned members,
// joins tried by itself, coverage reports queued, rate-limit waits, errors.
type Result struct {
	Groups, Joins, Reports, RateLimited, Errors int
}

// Run sweeps every moderated group once; runID names the pass in the ledger
// (a retry of the same removal in a later pass stays one row per episode).
func (s *Sweep) Run(ctx context.Context, runID string) (Result, error) {
	var res Result
	lists := s.discover(ctx, &res)
	s.mu.Lock()
	s.at, s.lists = s.now(), lists
	s.mu.Unlock()
	if err := s.settle(ctx, &res, lists); err != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.Errors++
		s.Log.Error("sweep: could not record coverage", "err", mask.IDs(err.Error()))
	}
	rs := s.Config.Current().Rules
	for _, g := range s.Directory.Moderated(rs) {
		// The bot cannot remove or reject where it is not an admin; the
		// coverage reports already ask for a promotion.
		if !s.Directory.BotIsAdmin(g) {
			s.Log.Debug("sweep: skipping a group where the bot is not an admin", "group", mask.IDs(string(g)))
			continue
		}
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
			if errors.Is(err, client.ErrNotAdmin) {
				s.LostAdmin(ctx, g, "WhatsApp refused to list its join requests")
				continue
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

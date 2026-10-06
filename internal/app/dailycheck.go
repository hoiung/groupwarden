package app

import (
	"context"
	"strings"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/store"
)

// checkDaily posts the daily check in the admin chat once per local day, at or
// after daily_check_time: the date and "alive and working", or what is not
// working (the healthcheck rule's problems). The post coming from the running
// bot is the alive signal, so no post by a quarter past means look at the
// node. Only today is posted: a day the bot was down for is not back-filled.
// The day posted is a once-marker (mark): in the store, so a restart does not
// post twice, and in memory, so a store that cannot be written does not
// re-post it every tick.
func (a *App) checkDaily(ctx context.Context, now time.Time) {
	at := a.Config.Current().Config.DailyCheckTime
	due, err := time.Parse("15:04", at) // the schema allows only HH:MM
	if err != nil {
		a.Log.Error("daily check: daily_check_time unreadable", "value", at, "err", err)
		return
	}
	local := now.In(a.zone())
	if local.Hour()*60+local.Minute() < due.Hour()*60+due.Minute() {
		return
	}
	day := local.Format(time.DateOnly)
	st, err := a.Store.Status(ctx)
	if err != nil {
		a.Log.Error("read status", "err", err)
		return
	}
	if a.marked(st, store.StatusDailyCheckDay) == day {
		return
	}
	h := ReadHealth(st, now)
	head := "Daily check, " + local.Format("Monday 2 January 2006") + ": groupwarden is "
	text := head + "alive and working."
	problems := h.Problems()
	if len(problems) > 0 {
		text = head + "alive but not working fully: " + strings.Join(problems, "; ") + "."
	}
	// Not marked as posted unless the admin chat took it: the next tick tries
	// again.
	if err := a.tryAlert(ctx, alert.Alert{Kind: alert.DailyCheck, Text: text}); err != nil {
		return
	}
	a.mark(ctx, store.StatusDailyCheckDay, day)
	a.Log.Info("daily check posted", "day", day, "healthy", len(problems) == 0, "problems", len(problems))
}

// zone is the time zone daily_check_time is read in.
func (a *App) zone() *time.Location {
	if a.Zone != nil {
		return a.Zone
	}
	return time.Local
}

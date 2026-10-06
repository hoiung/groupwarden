package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Heartbeat ping timings (fixed, not tunables): heartbeat_url is pinged every
// pingEvery while the bot is healthy [S3-10].
const (
	pingEvery   = 5 * time.Minute
	pingTimeout = 10 * time.Second
)

// pinger pings heartbeat_url (when set) every pingEvery while the bot passes
// the same checks as `groupwarden healthcheck`. An outside monitor that
// stops hearing it raises the alarm even when this node is off or asleep,
// which no alert from the bot itself can do.
func (a *App) pinger(ctx context.Context) {
	client := &http.Client{Timeout: pingTimeout}
	withheld, failing := false, false
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.Clock.After(pingEvery):
		}
		target := a.Config.Current().Config.HeartbeatURL
		if target == "" {
			continue
		}
		st, err := a.Store.Status(ctx)
		if err != nil {
			a.Log.Error("heartbeat: read status", "err", err)
			continue
		}
		h := ReadHealth(st, a.Clock.Now())
		if !h.OK() {
			if !withheld {
				a.Log.Warn("heartbeat ping withheld: the bot is not healthy", "running", h.Running,
					"connected", h.Connected, "deaf", h.Deaf, "config_loaded", h.ConfigHash != "", "telegram", h.Telegram)
			}
			withheld = true
			continue
		}
		if withheld {
			a.Log.Info("heartbeat ping resumed: the bot is healthy again")
		}
		withheld = false
		if err := ping(ctx, client, target); err != nil {
			a.Log.Warn("heartbeat ping failed", "err", err.Error())
			failing = true
			continue
		}
		if failing {
			a.Log.Info("heartbeat ping works again")
		}
		failing = false
		a.Log.Debug("heartbeat ping sent")
	}
}

// ping makes one request to target. Its errors never quote target: a
// monitor's ping URL is a secret (whoever has it can fake the heartbeat).
func ping(ctx context.Context, c *http.Client, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return errors.New("heartbeat_url is not a valid URL")
	}
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the monitor answered HTTP %d", resp.StatusCode)
	}
	return nil
}

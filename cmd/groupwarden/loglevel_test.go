package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/config"
)

// TestLogLevel: --log-level, else $GROUPWARDEN_LOG_LEVEL, else info, sets the
// level of the logger the bot runs with; any other value is refused.
func TestLogLevel(t *testing.T) {
	cases := []struct {
		name, env string
		args      []string
		code      int
		lowest    slog.Level // the lowest level the logger writes
	}{
		{"default", "", nil, exitOK, slog.LevelInfo},
		{"flag-debug", "", []string{"--log-level", "debug"}, exitOK, slog.LevelDebug},
		{"env-debug", "debug", nil, exitOK, slog.LevelDebug},
		{"flag-over-env", "debug", []string{"--log-level", "warn"}, exitOK, slog.LevelWarn},
		{"env-error", "error", nil, exitOK, slog.LevelError},
		{"invalid-flag", "", []string{"--log-level", "verbose"}, exitUsage, 0},
		{"invalid-env", "DEBUG", nil, exitUsage, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			te := newTestEnv(t, &clienttest.Fake{})
			te.env.getenv = func(k string) string {
				if k == "GROUPWARDEN_LOG_LEVEL" {
					return c.env
				}
				return ""
			}
			var got *slog.Logger
			open := te.env.openAdapter
			te.env.openAdapter = func(ctx context.Context, cfg *config.Config, log *slog.Logger) (client.Adapter, error) {
				got = log
				return open(ctx, cfg, log)
			}
			code := te.cmd(append([]string{"groups"}, c.args...)...)
			if code != c.code {
				t.Fatalf("exit %d, want %d: %s", code, c.code, te.errb)
			}
			if c.code == exitUsage {
				if got != nil || !strings.Contains(te.errb.String(), "use debug, info, warn or error") {
					t.Fatalf("connected %v, stderr %q", got != nil, te.errb)
				}
				return
			}
			ctx := context.Background()
			if got == nil || !got.Enabled(ctx, c.lowest) || got.Enabled(ctx, c.lowest-1) {
				t.Fatalf("logger writes from %v? got %v", c.lowest, got != nil && got.Enabled(ctx, c.lowest))
			}
		})
	}
}

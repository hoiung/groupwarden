package app

import (
	"context"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/store"
)

// TestReadHealth: healthy needs every check (running, connected, not deaf,
// a config, the admin chat reached); each one failing alone is unhealthy.
func TestReadHealth(t *testing.T) {
	now := time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC)
	at := func(v string, ago time.Duration) store.StatusValue {
		return store.StatusValue{Value: v, UpdatedAt: now.Add(-ago)}
	}
	healthy := func() map[string]store.StatusValue {
		return map[string]store.StatusValue{
			store.StatusHeartbeat:  at("", 30*time.Second),
			store.StatusConnected:  at("1", 0),
			store.StatusDeaf:       at("0", 0),
			store.StatusConfigHash: at("55786e8be51a", 0),
			store.StatusTelegramOK: at("1", time.Hour),
		}
	}
	h := ReadHealth(healthy(), now)
	if !h.OK() || !h.Running || h.BeatAge != 30*time.Second || h.ConfigHash != "55786e8be51a" ||
		!h.TelegramSince.Equal(now.Add(-time.Hour)) {
		t.Fatalf("healthy status read as %+v", h)
	}
	for _, tc := range []struct {
		name   string
		break_ func(map[string]store.StatusValue)
	}{
		{"no status write yet", func(m map[string]store.StatusValue) { delete(m, store.StatusHeartbeat) }},
		{"status write too old", func(m map[string]store.StatusValue) {
			m[store.StatusHeartbeat] = at("", BeatFresh+time.Second)
		}},
		{"disconnected", func(m map[string]store.StatusValue) { m[store.StatusConnected] = at("0", 0) }},
		{"deaf", func(m map[string]store.StatusValue) { m[store.StatusDeaf] = at("1", 0) }},
		{"no config", func(m map[string]store.StatusValue) { delete(m, store.StatusConfigHash) }},
		{"Telegram refusing", func(m map[string]store.StatusValue) { m[store.StatusTelegramOK] = at("0", 0) }},
		{"Telegram not reached yet", func(m map[string]store.StatusValue) { delete(m, store.StatusTelegramOK) }},
	} {
		st := healthy()
		tc.break_(st)
		if h := ReadHealth(st, now); h.OK() {
			t.Errorf("%s: read as healthy (%+v)", tc.name, h)
		}
	}
	// Exactly BeatFresh old still counts as running.
	st := healthy()
	st[store.StatusHeartbeat] = at("", BeatFresh)
	if h := ReadHealth(st, now); !h.Running || !h.OK() {
		t.Fatalf("a status write BeatFresh old: %+v", h)
	}
}

// TestCleanStopRecordsDisconnected: a stopped bot's last status says it is
// not connected, so a health check right after a restart does not pass on
// the stopped process's status.
func TestCleanStopRecordsDisconnected(t *testing.T) {
	h := start(t, &clienttest.Fake{}, Settings{})
	h.clock.step(time.Minute, 30*time.Second)
	h.eventually("connected recorded", func() bool { return h.status(store.StatusConnected) == "1" })
	h.cancel()
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
	if v := h.status(store.StatusConnected); v != "0" {
		t.Fatalf("connected = %q after a clean stop; want 0", v)
	}
}

func (h *harness) status(key string) string {
	h.t.Helper()
	st, err := h.st.Status(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return st[key].Value
}

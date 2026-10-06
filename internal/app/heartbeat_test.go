package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/store"
)

// TestHeartbeatOnlyWhenHealthy: heartbeat_url (off by default) is pinged
// every 5 minutes while the bot passes the healthcheck's checks, and not
// while it fails one (the admin chat not reached yet, WhatsApp down).
func TestHeartbeatOnlyWhenHealthy(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping/check-1" {
			hits.Add(1)
		}
	}))
	defer srv.Close()
	h := start(t, &clienttest.Fake{}, Settings{})
	// pinged waits until the pinger has finished its check at the current
	// time (it then waits for the next one; the clock must not move before
	// that, or the next check drifts off the tick) and returns the hits so far.
	pinged := func() int32 {
		t.Helper()
		next := h.clock.Now().Add(pingEvery)
		h.eventually("the pinger's check", func() bool { return h.clock.hasWaiterAt(next) })
		return hits.Load()
	}

	// Off by default: nothing to ping, the bot healthy or not.
	h.setStatus(store.StatusTelegramOK, "1")
	h.tick(pingEvery)
	if n := pinged(); n != 0 {
		t.Fatalf("pinged %d times with no heartbeat_url", n)
	}

	h.writeConfig("heartbeat_url: " + srv.URL + "/ping/check-1\n")
	h.sighup()
	h.eventually("reload", func() bool { return h.app.Config.Current().Config.HeartbeatURL != "" })
	for want := int32(1); want <= 2; want++ {
		h.tick(pingEvery)
		if n := pinged(); n != want {
			t.Fatalf("%d pings after %d healthy intervals", n, want)
		}
	}

	// The admin chat refusing the bot: unhealthy, so no ping.
	h.setStatus(store.StatusTelegramOK, "0")
	h.tick(pingEvery)
	if n := pinged(); n != 2 {
		t.Fatalf("%d pings; want none while Telegram refuses the bot", n-2)
	}
	h.setStatus(store.StatusTelegramOK, "1")

	// WhatsApp down: no ping until it is back.
	h.fake.SetConnectErr(errors.New("network unreachable"))
	h.disconnect()
	h.tick(pingEvery)
	if n := pinged(); n != 2 {
		t.Fatalf("%d pings while disconnected", n-2)
	}
	// The reconnect waits out the backoff the failures built up, so allow a
	// few checks for it.
	h.fake.SetConnectErr(nil)
	for i := 0; i < 4 && hits.Load() < 3; i++ {
		h.tick(pingEvery)
		pinged()
	}
	h.connected()
	if n := hits.Load(); n != 3 {
		t.Fatalf("%d pings after reconnecting; want 1", n-2)
	}
}

// tick moves the clock d on in monitor ticks, each taken by the supervisor
// before the next, so its status writes keep up with the clock however slowly
// the test runs (the race detector); a plain step can leave them minutes stale.
func (h *harness) tick(d time.Duration) {
	h.t.Helper()
	for moved := time.Duration(0); moved < d; moved += monitorEvery {
		h.clock.Advance(monitorEvery)
		next := h.clock.Now().Add(monitorEvery)
		h.eventually("the supervisor's tick", func() bool { return h.clock.hasWaiterAt(next) })
	}
}

func (h *harness) setStatus(key, value string) {
	h.t.Helper()
	if err := h.st.SetStatus(context.Background(), map[string]string{key: value}); err != nil {
		h.t.Fatal(err)
	}
}

// TestPingErrorsHideTheURL: whoever has a monitor's ping URL can fake the
// heartbeat, so no ping error quotes it.
func TestPingErrorsHideTheURL(t *testing.T) {
	const pingMark = "s3cr3t-check-path"
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer refusing.Close()
	gone := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	gone.Close()
	c := &http.Client{Timeout: pingTimeout}
	for _, tc := range []struct {
		name, target, want string
	}{
		{"refused", refusing.URL + "/ping/" + pingMark, "the monitor answered HTTP 500"},
		{"unreachable", gone.URL + "/ping/" + pingMark, "connection refused"},
		{"malformed", "http://[::1/ping/" + pingMark, "heartbeat_url is not a valid URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ping(context.Background(), c, tc.target)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), pingMark) {
				t.Fatalf("the error quotes the ping URL: %v", err)
			}
		})
	}
	ok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ok.Close()
	if err := ping(context.Background(), c, ok.URL+"/ping/"+pingMark); err != nil {
		t.Fatalf("a 200 is a ping: %v", err)
	}
}

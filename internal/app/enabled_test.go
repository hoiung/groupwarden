package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/config/configtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
)

// TestEnableSweepsAfterTheJoin: /enable asks for the sweep only once its own
// join through the link is done. A sweep that listed a community's groups
// before the bot was in the community would try to join them by itself too
// early, and never try again. /disable asks for one after the removal.
func TestEnableSweepsAfterTheJoin(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "g.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	holder, _ := configtest.Holder(t, testSet)
	community := client.JID("99999000000777@g.us") // listed in .secret-pii-allowlist
	fake := &clienttest.Fake{Invites: map[string]client.Group{"CoMm1": {JID: community, Name: "book club", IsCommunity: true}}}
	a := &App{Adapter: fake, Store: st, Config: holder, Directory: &pipeline.Directory{}, Clock: SystemClock{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), sweepNow: make(chan struct{}, 1)}
	pendingAtJoin := -1
	fake.OnCall = func(call string) {
		if strings.HasPrefix(call, "JoinWithLink") {
			pendingAtJoin = len(a.sweepNow)
		}
	}
	by := telegram.Actor{ID: 501, Name: "Ann"}
	reply, err := controls{a}.Enable(ctx, "https://chat.whatsapp.com/CoMm1", by)
	if err != nil || !strings.Contains(reply, "the bot joined it") {
		t.Fatalf("enable: %q %v", reply, err)
	}
	if pendingAtJoin != 0 || len(a.sweepNow) != 1 {
		t.Fatalf("sweeps pending at the join %d, after /enable %d; want 0 then 1", pendingAtJoin, len(a.sweepNow))
	}
	<-a.sweepNow
	reply, err = controls{a}.Disable(ctx, "book club", by)
	if err != nil || !strings.Contains(reply, "no longer moderates") || len(a.sweepNow) != 1 {
		t.Fatalf("disable: %q %v, %d sweeps pending", reply, err, len(a.sweepNow))
	}
}

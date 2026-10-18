package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// TestRunLoadsTheEnabledList: `run` merges the store's /enable list into the
// config before it starts moderating, and the version stays the file's (the
// one `check` prints).
func TestRunLoadsTheEnabledList(t *testing.T) {
	te := newTestEnv(t, &clienttest.Fake{})
	te.provision(t)
	tg := telegramtest.New(t, nil)
	te.writeFile(t, "secrets.env", config.KeyTelegramBot+"="+tg.Token+"\n"+
		config.KeyTelegramChatID+"="+strconv.FormatInt(telegramtest.ChatID, 10)+"\n", 0o600)
	l, err := config.Load(te.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, l.Config.StoreDB(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e := store.Enabled{ID: "99999000000777@g.us", Kind: store.KindCommunity, Name: "book club", By: "test", At: time.Now()}
	if err := st.Enable(ctx, e); err != nil {
		t.Fatal(err)
	}
	list, err := st.EnabledList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	merged, err := l.WithEnabled(app.EnabledEntries(list))
	if err != nil {
		t.Fatal(err)
	}
	if merged.Hash != l.Hash {
		t.Fatalf("the /enable entry changed the config version: v%s, the file's v%s", merged.Hash, l.Hash)
	}
	if got, file := len(merged.Rules.Communities()), len(l.Rules.Communities()); got != file+1 {
		t.Fatalf("%d communities merged, the file has %d", got, file)
	}
	runEnv := *te.env
	stderr := &syncBuffer{}
	runEnv.stdout, runEnv.stderr, runEnv.telegramURL = &syncBuffer{}, stderr, tg.URL
	done := make(chan int, 1)
	go func() { done <- runEnv.run([]string{"run", "--config", te.cfgPath}) }()
	want := `"msg":"enabled list loaded","config":"v` + l.Hash + `","communities":` +
		strconv.Itoa(len(merged.Rules.Communities()))
	// Stopped only once it has started (a stop during the ledger recovery
	// exits 1).
	started := `"msg":"watching outside timers"`
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(stderr.String(), started); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("run did not start:\n%s", stderr)
		}
	}
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("run did not load the /enable list (want %s):\n%s", want, stderr)
	}
	te.cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("run exited %d on shutdown\n%s", code, stderr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not stop")
	}
}

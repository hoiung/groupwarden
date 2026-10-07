package pipeline

import (
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config/configtest"
)

func swapConfig(list, rule string) string {
	return "communities:\n  test-set:\n    groups: [\"99999000000111@g.us\"]\n" +
		"word_lists:\n  " + list + ": [\"" + list + "\"]\n" +
		"rules:\n  list:\n    - name: " + rule + "\n      action: log\n      when:\n        words: " + list + "\n"
}

// TestAtomicSwap: while reloads swap between two configs, every decision is
// made by ONE config: the rule that decided and the config hash it reports
// always belong together.
func TestAtomicSwap(t *testing.T) {
	cfgA, cfgB := swapConfig("alpha", "rule-a"), swapConfig("beta", "rule-b")
	holder, path := configtest.Holder(t, cfgA)
	hashA := holder.Current().Hash
	if err := os.WriteFile(path, []byte(configtest.Base+cfgB), 0o600); err != nil {
		t.Fatal(err)
	}
	lb, err := holder.Reload()
	if err != nil {
		t.Fatal(err)
	}
	hashB := lb.Hash
	if hashA == hashB {
		t.Fatal("the two configs share a hash")
	}
	m := &Moderator{Config: holder, Directory: &Directory{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	msg := &client.Message{Chat: "99999000000111@g.us", Sender: "99999000000444@lid", ID: "M1", TargetID: "M1",
		Fields: []client.Field{{Name: "body", Text: "alpha beta", Match: "alpha beta"}}}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the reloader
		defer wg.Done()
		bodies := []string{cfgA, cfgB}
		for i := 0; i < 40; i++ {
			if err := os.WriteFile(path, []byte(configtest.Base+bodies[i%2]), 0o600); err != nil {
				t.Error(err)
				return
			}
			if _, err := holder.Reload(); err != nil {
				t.Error(err)
				return
			}
		}
		close(stop)
	}()
	seen := map[string]int{}
	var mu sync.Mutex
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				d, hash := m.EvaluateIn(msg, "test-set")
				ok := (hash == hashA && d.Rule == "rule-a") || (hash == hashB && d.Rule == "rule-b")
				if !ok {
					t.Errorf("mixed decision: rule %q under config v%s", d.Rule, hash)
					return
				}
				mu.Lock()
				seen[hash]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if seen[hashA] == 0 && seen[hashB] == 0 {
		t.Fatal("no decision was made during the swaps")
	}
}

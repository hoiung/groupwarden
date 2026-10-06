package telegram_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// failWrites makes writes to table fail (those matching when, a trigger WHEN
// condition on NEW, when it is not empty), as a full disk would, until the
// returned lift is called. The store has one connection, so temporary
// triggers on it see every write.
func failWrites(t *testing.T, st *store.Store, table, when string) (lift func()) {
	t.Helper()
	cond := ""
	if when != "" {
		cond = " WHEN " + when
	}
	events := []string{"INSERT", "UPDATE"}
	if when == "" {
		events = append(events, "DELETE")
	}
	exec := func(stmts []string) {
		t.Helper()
		if err := st.Write(context.Background(), func(tx *sql.Tx) error {
			for _, s := range stmts {
				if _, err := tx.Exec(s); err != nil {
					return fmt.Errorf("%s: %w", s, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var create, drop []string
	for _, ev := range events {
		name := "fail_" + table + "_" + strings.ToLower(ev)
		create = append(create, "CREATE TEMP TRIGGER "+name+" BEFORE "+ev+" ON "+table+cond+
			" BEGIN SELECT RAISE(FAIL, 'database or disk is full'); END")
		drop = append(drop, "DROP TRIGGER temp."+name)
	}
	exec(create)
	return func() { exec(drop) }
}

// requests counts the Bot API calls of method whose text contains match.
func requests(h *harness, method, match string) int {
	n := 0
	for _, r := range h.srv.Requests(method) {
		if strings.Contains(r.Params["text"], match) {
			n++
		}
	}
	return n
}

func step(h *harness) { _, _ = h.chat.Step(h.k.Ctx) }

func setup(h *harness) { _ = h.chat.Setup(h.k.Ctx) }

// TestPostNotRepeatedWhileItsRecordFails: every post whose record is a store
// write (each member of the class), with that write failing: the post goes
// out once, nothing posts it again on the retries while the store refuses,
// and once the store takes writes again the record is made and the post is
// still not repeated.
func TestPostNotRepeatedWhileItsRecordFails(t *testing.T) {
	busy := func(h *harness) {
		for i := 0; i < 12; i++ {
			h.report(store.Report{Kind: string(alert.Coverage), Text: fmt.Sprintf("busy %d", i)})
		}
	}
	cases := []struct {
		name        string
		cfg         string                         // modtest config (default when empty)
		setup       func(t *testing.T, h *harness) // up to the point where the work posts next
		table, when string                         // the record write that fails
		run         func(h *harness)               // one try of the work
		posts       func(h *harness) int           // what must not repeat
		recorded    func(t *testing.T, h *harness) // the record, once writes work again
	}{
		{name: "report part", table: "tg_messages", run: step,
			setup: func(t *testing.T, h *harness) {
				h.report(store.Report{Kind: string(alert.Coverage), Text: "one report"})
			},
			posts: func(h *harness) int { return requests(h, "sendMessage", "one report") },
			recorded: func(t *testing.T, h *harness) {
				r := h.k.Reports(string(alert.Coverage))[0]
				if len(h.messages(r.ID)) != 1 {
					t.Fatalf("report %d messages %+v, want its post recorded", r.ID, h.messages(r.ID))
				}
			}},
		{name: "digest message", table: "tg_messages", run: step, setup: func(t *testing.T, h *harness) { busy(h) },
			posts:    func(h *harness) int { return requests(h, "sendMessage", "reports while the chat was busy") },
			recorded: func(t *testing.T, h *harness) { unsentNone(t, h) }},
		{name: "digest reports sent", table: "reports", run: step, setup: func(t *testing.T, h *harness) { busy(h) },
			posts:    func(h *harness) int { return requests(h, "sendMessage", "reports while the chat was busy") },
			recorded: func(t *testing.T, h *harness) { unsentNone(t, h) }},
		{name: "attachment", table: "tg_messages", run: step,
			setup: func(t *testing.T, h *harness) {
				h.k.Fake.Download = func(context.Context, *client.Message) ([]byte, string, string, error) {
					return []byte("JPEGDATA"), "image/jpeg", "offer.jpg", nil
				}
				h.k.Deliver(image(h.k.Spam("I1", modtest.G1)))
				h.k.Fire()
				h.drain()
				if n, err := h.k.Media.Fetch(h.k.Ctx); err != nil || n != 1 {
					t.Fatalf("fetched %d (%v)", n, err)
				}
			},
			posts: func(h *harness) int { return len(h.srv.Requests("sendDocument")) },
			recorded: func(t *testing.T, h *harness) {
				if attachmentPost(t, h).MessageID == 0 {
					t.Fatal("the attachment post was never recorded")
				}
			}},
		{name: "attachment take-down", table: "tg_messages", run: step,
			setup: func(t *testing.T, h *harness) {
				attachmentReport(t, h)
				h.k.Clock.Advance(25 * time.Hour)
			},
			posts: func(h *harness) int { return len(h.srv.Requests("deleteMessage")) },
			recorded: func(t *testing.T, h *harness) {
				if attachmentPost(t, h).GoneAt.IsZero() {
					t.Fatal("the take-down was never recorded")
				}
			}},
		{name: "text removal", table: "tg_messages", run: step,
			setup: func(t *testing.T, h *harness) {
				h.k.Deliver(h.k.Spam("E1", modtest.G1))
				h.drain()
				h.k.Clock.Advance(31 * 24 * time.Hour)
			},
			posts: func(h *harness) int { return len(h.srv.Requests("editMessageText")) },
			recorded: func(t *testing.T, h *harness) {
				for _, m := range h.messages(h.only(ledger.KindAction).ID) {
					if m.StrippedAt.IsZero() {
						t.Fatalf("message %+v: the text removal was never recorded", m)
					}
				}
			}},
		{name: "daily summary", table: "status", when: "NEW.key = '" + store.StatusSummaryDay + "'", run: step,
			setup: func(t *testing.T, h *harness) {
				h.drain() // the first run starts the summaries with today
				h.k.Deliver(h.k.Msg("K1", modtest.G1, modtest.Member, "anyone into crypto?"))
				h.drain()
				h.k.Clock.Advance(24 * time.Hour)
			},
			posts: func(h *harness) int { return requests(h, "sendMessage", "Daily summary for ") },
			recorded: func(t *testing.T, h *harness) {
				if got := statusOf(t, h, store.StatusSummaryDay); got != "2026-10-06" {
					t.Fatalf("summary day %q, want 2026-10-06", got)
				}
			}},
		{name: "daily summary log reports", cfg: logRuleConfig, table: "reports", run: step,
			setup: func(t *testing.T, h *harness) {
				h.drain()
				h.k.Deliver(h.k.Msg("S1", modtest.G1, modtest.Member, "nasdaq today"))
				// A day counter too: a summary re-run after only its log
				// reports were recorded would still have this to post.
				h.k.Deliver(h.k.Msg("K1", modtest.G1, modtest.Member, "anyone into crypto?"))
				h.drain()
				h.k.Clock.Advance(24 * time.Hour)
			},
			posts: func(h *harness) int { return requests(h, "sendMessage", "Daily summary for ") },
			recorded: func(t *testing.T, h *harness) {
				if rest, err := h.k.Store.UnsentReportsOf(h.k.Ctx, ledger.KindLog, 10); err != nil || len(rest) != 0 {
					t.Fatalf("%d log reports unsent (%v)", len(rest), err)
				}
				if got := statusOf(t, h, store.StatusSummaryDay); got != "2026-10-06" {
					t.Fatalf("summary day %q, want 2026-10-06", got)
				}
			}},
		{name: "command list post", table: "status", when: "NEW.key = '" + store.StatusTelegramPin + "'", run: setup,
			setup: func(*testing.T, *harness) {},
			posts: func(h *harness) int { return requests(h, "sendMessage", telegram.CommandList()) },
			recorded: func(t *testing.T, h *harness) {
				if statusOf(t, h, store.StatusTelegramPinState) != "pinned" {
					t.Fatal("the posted list was never recorded and pinned")
				}
			}},
		{name: "command list pin", table: "status",
			when: "NEW.key = '" + store.StatusTelegramPinState + "' AND NEW.value = 'pinned'", run: setup,
			setup: func(*testing.T, *harness) {},
			posts: func(h *harness) int { return len(h.srv.Requests("pinChatMessage")) },
			recorded: func(t *testing.T, h *harness) {
				if statusOf(t, h, store.StatusTelegramPinState) != "pinned" {
					t.Fatal("the pin was never recorded")
				}
			}},
		{name: "command list update", table: "status", when: "NEW.key = '" + store.StatusTelegramPinHash + "'", run: setup,
			setup: func(t *testing.T, h *harness) {
				if err := h.chat.Setup(h.k.Ctx); err != nil {
					t.Fatal(err)
				}
				if err := h.k.Store.SetStatus(h.k.Ctx, map[string]string{store.StatusTelegramPinHash: "old"}); err != nil {
					t.Fatal(err)
				}
			},
			posts: func(h *harness) int { return len(h.srv.Requests("editMessageText")) },
			recorded: func(t *testing.T, h *harness) {
				if statusOf(t, h, store.StatusTelegramPinHash) == "old" {
					t.Fatal("the updated list was never recorded")
				}
			}},
		{name: "pin refusal report", table: "status",
			when: "NEW.key = '" + store.StatusTelegramPinState + "' AND NEW.value = 'refused'", run: setup,
			setup: func(t *testing.T, h *harness) {
				h.srv.FailAlways("pinChatMessage", telegramtest.Reply{Code: 400,
					Description: "Bad Request: not enough rights to manage pinned messages in the chat"})
			},
			posts: func(h *harness) int { return len(h.k.Reports(string(alert.PinRefused))) },
			recorded: func(t *testing.T, h *harness) {
				if statusOf(t, h, store.StatusTelegramPinState) != "refused" {
					t.Fatal("the refusal was never recorded")
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, "")
			if c.cfg != "" {
				h = newHarnessKit(t, modtest.NewConfig(t, c.cfg))
			}
			c.setup(t, h)
			before := c.posts(h)
			lift := failWrites(t, h.k.Store, c.table, c.when)
			for i := 0; i < 3; i++ {
				c.run(h)
			}
			if n := c.posts(h) - before; n != 1 {
				t.Fatalf("posted %d times while its record could not be written, want once\nlog:\n%s", n, h.logs)
			}
			if !strings.Contains(h.logs.String(), "could not record a post in the store") {
				t.Fatalf("no log line for the missing record:\n%s", h.logs)
			}
			lift()
			c.run(h)
			c.run(h)
			if n := c.posts(h) - before; n != 1 {
				t.Fatalf("posted %d times once writes worked again, want still once", n)
			}
			c.recorded(t, h)
		})
	}
}

// unsentNone: every report is marked sent.
func unsentNone(t *testing.T, h *harness) {
	t.Helper()
	h.drain()
	if n, err := h.k.Store.CountUnsent(h.k.Ctx, ledger.KindLog); err != nil || n != 0 {
		t.Fatalf("%d reports unsent (%v), want none", n, err)
	}
}

// attachmentPost is the recorded attachment post of the one action report.
func attachmentPost(t *testing.T, h *harness) store.TGMessage {
	t.Helper()
	for _, m := range h.messages(h.only(ledger.KindAction).ID) {
		if m.Role == store.RoleAttachment {
			return m
		}
	}
	return store.TGMessage{}
}

func statusOf(t *testing.T, h *harness, key string) string {
	t.Helper()
	st, err := h.k.Store.Status(h.k.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	return st[key].Value
}

// logRuleConfig adds a log-only rule (report in the daily summary only).
var logRuleConfig = strings.Replace(strings.Replace(modtest.Config, "  lures: [\"inbox me\"]\n",
	"  lures: [\"inbox me\"]\n  stocks: [\"nasdaq\"]\n", 1), "rules:\n  list:\n",
	"rules:\n  list:\n    - name: stocktalk\n      action: log\n      when:\n        words: stocks\n", 1)

package telegram_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/telegram"
	"github.com/hoiung/groupwarden/internal/telegram/telegramtest"
)

// wantCommands is every AC 4.3 command, in the list's order, with its argument.
var wantCommands = []string{"/status", "/pause", "/resume", "/reload", "/join <invite link>", "/ban",
	"/unban <report number>"}

// setup runs the command list setup once and fails the test on an error.
func (h *harness) setup() {
	h.t.Helper()
	if err := h.chat.Setup(h.k.Ctx); err != nil {
		h.t.Fatalf("setup: %v", err)
	}
}

// pinStatus is the stored pinned list: its message, text hash and state.
func (h *harness) pinStatus() (msg, hash, state string) {
	h.t.Helper()
	st, err := h.k.Store.Status(h.k.Ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return st[store.StatusTelegramPin].Value, st[store.StatusTelegramPinHash].Value, st[store.StatusTelegramPinState].Value
}

// setStatus writes one status key.
func (h *harness) setStatus(key, value string) {
	h.t.Helper()
	if err := h.k.Store.SetStatus(h.k.Ctx, map[string]string{key: value}); err != nil {
		h.t.Fatal(err)
	}
}

// TestCommandListPinnedAtStart: at start the bot posts one message listing
// every command and pins it without a notification; the reply to a command
// it does not know is the same list; a later start with the same list posts,
// edits and pins nothing.
func TestCommandListPinnedAtStart(t *testing.T) {
	h := newHarness(t, "")
	h.setup()
	list := telegram.CommandList()
	for _, cmd := range wantCommands {
		if !strings.Contains(list, "\n"+cmd+" — ") {
			t.Fatalf("the list has no line for %s:\n%s", cmd, list)
		}
	}
	if got := strings.Count(list, "\n/"); got != len(wantCommands) {
		t.Fatalf("the list has %d command lines, want %d:\n%s", got, len(wantCommands), list)
	}
	posts := h.srv.Requests("sendMessage")
	if len(posts) != 1 || posts[0].Params["text"] != list ||
		posts[0].Params["chat_id"] != strconv.FormatInt(telegramtest.ChatID, 10) {
		t.Fatalf("posts %+v, want the command list once in the admin chat", posts)
	}
	pins := h.srv.Requests("pinChatMessage")
	if len(pins) != 1 || pins[0].Params["message_id"] != strconv.Itoa(posts[0].MessageID) ||
		pins[0].Params["chat_id"] != strconv.FormatInt(telegramtest.ChatID, 10) ||
		pins[0].Params["disable_notification"] != "true" {
		t.Fatalf("pins %+v, want message %d pinned quietly", pins, posts[0].MessageID)
	}
	msg, _, state := h.pinStatus()
	if msg != strconv.FormatInt(telegramtest.ChatID, 10)+":"+strconv.Itoa(posts[0].MessageID) || state != "pinned" {
		t.Fatalf("stored pin %q state %q", msg, state)
	}
	if !strings.Contains(h.logs.String(), "command list pinned") {
		t.Fatalf("no log line for the pin:\n%s", h.logs)
	}

	before := len(h.srv.Requests("sendMessage", "editMessageText", "pinChatMessage"))
	h.restart()
	h.setup()
	if calls := h.srv.Requests("sendMessage", "editMessageText", "pinChatMessage"); len(calls) != before {
		t.Fatalf("a restart with the same list posted, edited or pinned again: %+v", calls[before:])
	}

	h.command(telegramtest.ChatID, adminUser, "/help", 0)
	if got := h.lastReply(); got != list {
		t.Fatalf("reply to an unknown command:\n%s\nwant the command list", got)
	}
}

// TestCommandListEditedInPlace: when the list changed since it was posted,
// a start edits the pinned message itself (no new post, no new pin).
func TestCommandListEditedInPlace(t *testing.T) {
	h := newHarness(t, "")
	h.setup()
	first := h.srv.Requests("sendMessage")[0]
	msg, hash, _ := h.pinStatus()
	h.setStatus(store.StatusTelegramPinHash, "an-older-list")

	h.restart()
	h.setup()
	edits := h.srv.Requests("editMessageText")
	if len(edits) != 1 || edits[0].Params["message_id"] != strconv.Itoa(first.MessageID) ||
		edits[0].Params["text"] != telegram.CommandList() {
		t.Fatalf("edits %+v, want message %d edited to the current list", edits, first.MessageID)
	}
	if n, p := len(h.srv.Requests("sendMessage")), len(h.srv.Requests("pinChatMessage")); n != 1 || p != 1 {
		t.Fatalf("%d posts and %d pins, want the first of each only", n, p)
	}
	if msg2, hash2, state := h.pinStatus(); msg2 != msg || hash2 != hash || state != "pinned" {
		t.Fatalf("stored pin %q hash %q state %q, want %q %q pinned", msg2, hash2, state, msg, hash)
	}
	h.setup()
	if n := len(h.srv.Requests("editMessageText")); n != 1 {
		t.Fatalf("%d edits after the list was brought up to date, want 1", n)
	}
}

// TestCommandListReplacedWhenGone: when the pinned message is gone (an admin
// deleted it) the list is posted and pinned again; when the chat moved to a
// new ID it is posted and pinned in the new chat.
func TestCommandListReplacedWhenGone(t *testing.T) {
	t.Run("message deleted", func(t *testing.T) {
		h := newHarness(t, "")
		h.setup()
		old := h.srv.Requests("sendMessage")[0].MessageID
		h.setStatus(store.StatusTelegramPinHash, "an-older-list")
		h.srv.Fail("editMessageText", telegramtest.Reply{Code: 400, Description: "Bad Request: message to edit not found"})
		h.restart()
		h.setup()
		posts, pins := h.srv.Requests("sendMessage"), h.srv.Requests("pinChatMessage")
		if len(posts) != 2 || len(pins) != 2 || posts[1].MessageID == old ||
			pins[1].Params["message_id"] != strconv.Itoa(posts[1].MessageID) {
			t.Fatalf("posts %+v pins %+v, want the list posted and pinned again", posts, pins)
		}
		if msg, _, state := h.pinStatus(); msg != strconv.FormatInt(telegramtest.ChatID, 10)+":"+
			strconv.Itoa(posts[1].MessageID) || state != "pinned" {
			t.Fatalf("stored pin %q state %q", msg, state)
		}
	})
	t.Run("chat moved", func(t *testing.T) {
		// The running loop sets up at start, and again when the chat moves.
		h := newHarness(t, "")
		ctx, cancel := context.WithCancel(h.k.Ctx)
		done := make(chan struct{})
		go func() {
			h.chat.SetupLoop(ctx)
			close(done)
		}()
		defer func() {
			cancel()
			<-done
		}()
		waitFor(t, func() bool { return len(h.srv.Requests("pinChatMessage")) == 1 })
		const moved int64 = -1009999000111
		h.report(store.Report{Kind: "would_remove", Text: "x"})
		h.srv.Fail("sendMessage", telegramtest.Reply{Code: 400,
			Description: "Bad Request: group chat was upgraded to a supergroup chat", MigrateTo: moved})
		h.drain()
		if h.chat.ChatID() != moved {
			t.Fatalf("chat %d, want %d", h.chat.ChatID(), moved)
		}
		waitFor(t, func() bool { return len(h.srv.Requests("pinChatMessage")) == 2 })
		pins := h.srv.Requests("pinChatMessage")
		menus := h.srv.Requests("setMyCommands")
		if len(pins) != 2 || pins[1].Params["chat_id"] != strconv.FormatInt(moved, 10) {
			t.Fatalf("pins %+v, want the list pinned again in the new chat", pins)
		}
		if len(menus) != 2 || !strings.Contains(menus[1].Params["scope"], strconv.FormatInt(moved, 10)) {
			t.Fatalf("menus %+v, want the menu set again for the new chat", menus)
		}
		var listPosts []telegramtest.Request
		for _, p := range h.srv.Requests("sendMessage") {
			if p.Params["text"] == telegram.CommandList() {
				listPosts = append(listPosts, p)
			}
		}
		if len(listPosts) != 2 || listPosts[1].Params["chat_id"] != strconv.FormatInt(moved, 10) ||
			pins[1].Params["message_id"] != strconv.Itoa(listPosts[1].MessageID) {
			t.Fatalf("list posts %+v, want a second one in the new chat, the one pinned", listPosts)
		}
	})
}

// TestCommandMenuScopedToAdminChat: the command menu is the same list, set
// for the admin chat only; every command in it is one the bot runs.
func TestCommandMenuScopedToAdminChat(t *testing.T) {
	h := newHarness(t, "")
	h.setup()
	menus := h.srv.Requests("setMyCommands")
	if len(menus) != 1 {
		t.Fatalf("%d setMyCommands calls, want 1", len(menus))
	}
	var scope struct {
		Type   string `json:"type"`
		ChatID int64  `json:"chat_id"`
	}
	if err := json.Unmarshal([]byte(menus[0].Params["scope"]), &scope); err != nil || scope.Type != "chat" ||
		scope.ChatID != telegramtest.ChatID {
		t.Fatalf("scope %q (%v), want the admin chat only", menus[0].Params["scope"], err)
	}
	var cmds []struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(menus[0].Params["commands"]), &cmds); err != nil {
		t.Fatal(err)
	}
	if len(cmds) != len(wantCommands) {
		t.Fatalf("menu %+v, want %d commands", cmds, len(wantCommands))
	}
	for i, c := range cmds {
		name, args, _ := strings.Cut(wantCommands[i], " ")
		if "/"+c.Command != name || c.Description == "" || len(c.Description) > 256 ||
			(args != "" && !strings.HasPrefix(c.Description, args+": ")) ||
			!strings.Contains(telegram.CommandList(), "/"+c.Command) {
			t.Fatalf("menu entry %d = %+v, want %s", i, c, wantCommands[i])
		}
		// A listed command is one the bot runs, not answered with the list.
		h.command(telegramtest.ChatID, adminUser, "/"+c.Command, 0)
		if h.lastReply() == telegram.CommandList() {
			t.Fatalf("/%s is listed but the bot does not know it", c.Command)
		}
	}
	h.setup()
	if n := len(h.srv.Requests("setMyCommands")); n != 1 {
		t.Fatalf("%d setMyCommands calls after a second setup in the same run, want 1", n)
	}
}

// TestPinRefusedReportedOnce: without the "Pin messages" right the list stays
// posted, the bot logs it and files one routine report saying to grant the
// right (not again on a restart), and tries the pin again every hour until it
// works.
func TestPinRefusedReportedOnce(t *testing.T) {
	h := newHarness(t, "")
	refused := telegramtest.Reply{Code: 400, Description: "Bad Request: not enough rights to manage pinned messages in the chat"}
	h.srv.FailAlways("pinChatMessage", refused)
	if err := h.chat.Setup(h.k.Ctx); err == nil || !strings.Contains(err.Error(), "refused to pin") {
		t.Fatalf("setup with the pin refused: %v", err)
	}
	reps := h.k.Reports("pin_refused")
	if len(reps) != 1 || reps[0].Priority || !strings.Contains(reps[0].Text, `"Pin messages" admin right`) {
		t.Fatalf("reports %+v, want one routine report naming the right", reps)
	}
	if posts := h.srv.Requests("sendMessage"); len(posts) != 1 || posts[0].Params["text"] != telegram.CommandList() {
		t.Fatalf("posts %+v, want the list posted", posts)
	}
	if !strings.Contains(h.logs.String(), "refused to pin the command list") {
		t.Fatalf("no log line for the refusal:\n%s", h.logs)
	}
	h.restart()
	if err := h.chat.Setup(h.k.Ctx); err == nil {
		t.Fatal("setup after a restart, pin still refused: no error")
	}
	if n := len(h.k.Reports("pin_refused")); n != 1 {
		t.Fatalf("%d pin_refused reports after a restart, want still 1", n)
	}
	if n := len(h.srv.Requests("sendMessage")); n != 1 {
		t.Fatalf("%d posts, want the list posted once", n)
	}

	// The loop tries again on its own (every hour; shortened here) and pins
	// once an admin grants the right.
	h.chat.SetPinRetry(10 * time.Millisecond)
	ctx, cancel := context.WithCancel(h.k.Ctx)
	done := make(chan struct{})
	go func() {
		h.chat.SetupLoop(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	waitFor(t, func() bool { return len(h.srv.Requests("pinChatMessage")) >= 4 })
	if n := len(h.k.Reports("pin_refused")); n != 1 {
		t.Fatalf("%d pin_refused reports while retrying, want still 1", n)
	}
	h.srv.Clear()
	waitFor(t, func() bool { _, _, state := h.pinStatus(); return state == "pinned" })
	if n := len(h.srv.Requests("sendMessage")); n != 1 {
		t.Fatalf("%d posts, want the list posted once", n)
	}
}

// waitFor polls cond for up to 5 seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

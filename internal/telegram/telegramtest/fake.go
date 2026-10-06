// Package telegramtest is a fake Telegram Bot API for tests: it answers the
// methods groupwarden calls the way api.telegram.org does, records every
// request, and can be told to fail one way or another.
package telegramtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

// ChatID is the fake admin chat (a supergroup ID, negative like every group).
const ChatID int64 = -1009999000000

// Request is one call the fake received.
type Request struct {
	Method string
	Params map[string]string // form fields (JSON-encoded for nested values)
	Files  map[string][]byte // uploaded files, by form field
	At     time.Time         // the test clock when it arrived
	// MessageID is the message the fake answered with (posts and edits).
	MessageID int
}

// Reply is a failure the fake answers with instead of success.
type Reply struct {
	Code        int    // HTTP status / error_code (400, 401, 403, 429, ...)
	Description string // "Bad Request: message can't be deleted", ...
	RetryAfter  int    // parameters.retry_after
	MigrateTo   int64  // parameters.migrate_to_chat_id
	// EchoPath answers with a body that is not JSON and quotes the request
	// path (which holds the bot token), as a broken proxy might.
	EchoPath bool
}

// Server is the fake Bot API.
type Server struct {
	*httptest.Server
	// Token is a token-shaped string built at run time (a literal would trip
	// the repo's secret scanner).
	Token string

	mu      sync.Mutex
	now     func() time.Time
	reqs    []Request
	nextID  int
	fail    map[string][]Reply // per method, consumed in order
	always  map[string]Reply   // per method, until Clear
	admins  map[int64]bool
	updates []json.RawMessage
	updID   int64
}

// New starts a fake Bot API; now stamps each request (nil: time.Now).
func New(t testing.TB, now func() time.Time) *Server {
	t.Helper()
	if now == nil {
		now = time.Now
	}
	s := &Server{now: now, nextID: 100, fail: map[string][]Reply{}, always: map[string]Reply{}, admins: map[int64]bool{}}
	s.Token = strconv.Itoa(700000001) + ":" + strings.Repeat("Ab0_", 9) // secret-allow (built at run time; fake)
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Fail makes the next call of method answer r (several calls queue; a zero
// Reply lets its call through, so a later one can fail).
func (s *Server) Fail(method string, r Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[method] = append(s.fail[method], r)
}

// FailAlways makes every call of method answer r until Clear.
func (s *Server) FailAlways(method string, r Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.always[method] = r
}

// Clear ends every FailAlways.
func (s *Server) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.always = map[string]Reply{}
}

// SetAdmin makes userID an administrator of the admin chat (or not).
func (s *Server) SetAdmin(userID int64, admin bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admins[userID] = admin
}

// QueueUpdate adds an update (without its update_id) for getUpdates.
func (s *Server) QueueUpdate(u map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updID++
	u["update_id"] = s.updID
	raw, _ := json.Marshal(u)
	s.updates = append(s.updates, raw)
}

// Requests lists the calls of the given methods (every call when none are
// named), in order.
func (s *Server) Requests(methods ...string) []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Request
	for _, r := range s.reqs {
		if len(methods) == 0 || contains(methods, r.Method) {
			out = append(out, r)
		}
	}
	return out
}

// Posted lists the messages posted (sendMessage and sendDocument), in order.
func (s *Server) Posted() []Request { return s.Requests("sendMessage", "sendDocument") }

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	prefix := "/bot" + s.Token + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, prefix)
	req := Request{Method: method, Params: map[string]string{}, Files: map[string][]byte{}}
	const maxBody = 64 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseMultipartForm(maxBody); err == nil { // #nosec G120 -- the body is capped by MaxBytesReader above
		for k, v := range r.MultipartForm.Value {
			req.Params[k] = v[0]
		}
		for k, fh := range r.MultipartForm.File {
			f, err := fh[0].Open()
			if err == nil {
				req.Files[k], _ = io.ReadAll(f)
				_ = f.Close()
			}
		}
	}
	s.mu.Lock()
	req.At = s.now()
	reply, failing := s.always[method]
	if !failing && len(s.fail[method]) > 0 {
		reply, failing = s.fail[method][0], true
		s.fail[method] = s.fail[method][1:]
	}
	idx := -1
	if method != "getUpdates" {
		idx = len(s.reqs)
		s.reqs = append(s.reqs, req)
	}
	s.mu.Unlock()
	if failing && (reply.Code != 0 || reply.EchoPath) { // a zero Reply lets that call through
		s.failWith(w, r, reply)
		return
	}
	if refused, ok := refuse(req); ok {
		s.failWith(w, r, refused)
		return
	}
	var result any
	switch method {
	case "getMe":
		result = map[string]any{"id": 700000001, "is_bot": true, "first_name": "groupwarden", "username": "groupwarden_test_bot"}
	case "sendMessage", "sendDocument", "editMessageText", "editMessageMedia":
		msg, id := s.message(req)
		s.mu.Lock()
		s.reqs[idx].MessageID = id
		s.mu.Unlock()
		result = msg
	case "deleteMessage", "answerCallbackQuery", "pinChatMessage", "setMyCommands":
		result = true
	case "getChatMember":
		uid, _ := strconv.ParseInt(req.Params["user_id"], 10, 64)
		s.mu.Lock()
		status := "member"
		if s.admins[uid] {
			status = "administrator"
		}
		s.mu.Unlock()
		result = map[string]any{"status": status, "user": map[string]any{"id": uid, "is_bot": false, "first_name": "u"}}
	case "getUpdates":
		s.mu.Lock()
		ups := s.updates
		s.updates = nil
		s.mu.Unlock()
		if len(ups) == 0 {
			time.Sleep(20 * time.Millisecond) // a long poll with nothing to say
		}
		result = append([]json.RawMessage{}, ups...)
	default:
		s.failWith(w, r, Reply{Code: 404, Description: "Not Found: method " + method})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// refuse answers what the Bot API refuses whatever the test set up: a text
// that is empty once trimmed, an empty file, and an entity outside the text.
func refuse(req Request) (Reply, bool) {
	bad := func(d string) (Reply, bool) { return Reply{Code: 400, Description: "Bad Request: " + d}, true }
	switch req.Method {
	case "sendMessage", "editMessageText":
		text := req.Params["text"]
		if strings.TrimSpace(text) == "" {
			return bad("message text is empty")
		}
		var entities []struct{ Offset, Length int }
		if e := req.Params["entities"]; e != "" {
			if err := json.Unmarshal([]byte(e), &entities); err != nil {
				return bad("can't parse entities: " + err.Error())
			}
		}
		n := len(utf16.Encode([]rune(text)))
		for _, e := range entities {
			if e.Offset < 0 || e.Length <= 0 || e.Offset+e.Length > n {
				return bad("can't parse entities: an entity is outside the text")
			}
		}
	case "sendDocument":
		for _, f := range req.Files {
			if len(f) == 0 {
				return bad("file must be non-empty")
			}
		}
	}
	return Reply{}, false
}

func (s *Server) message(req Request) (map[string]any, int) {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.mu.Unlock()
	if v, err := strconv.Atoi(req.Params["message_id"]); err == nil {
		id = v // an edit answers with the edited message
	}
	chat, _ := strconv.ParseInt(req.Params["chat_id"], 10, 64)
	return map[string]any{"message_id": id, "date": 1, "chat": map[string]any{"id": chat, "type": "supergroup"},
		"text": req.Params["text"]}, id
}

func (s *Server) failWith(w http.ResponseWriter, r *http.Request, f Reply) {
	if f.EchoPath {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, "bad gateway for %s", r.URL.Path) // #nosec G705 -- test fake: quoting the path (which holds the token) is the failure it plays
		return
	}
	params := map[string]any{}
	if f.RetryAfter > 0 {
		params["retry_after"] = f.RetryAfter
	}
	if f.MigrateTo != 0 {
		params["migrate_to_chat_id"] = f.MigrateTo
	}
	w.WriteHeader(f.Code)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": f.Code, "description": f.Description,
		"parameters": params})
}

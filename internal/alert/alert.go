// Package alert carries messages for the human admins. Phase 1 alerts go to
// the structured log; the Telegram admin chat implements the same interface.
package alert

import (
	"context"
	"log/slog"
	"sync"

	"github.com/hoiung/groupwarden/internal/mask"
)

// Kind names what an alert is about.
type Kind string

const (
	FatalDisconnect     Kind = "fatal_disconnect"
	TemporaryBan        Kind = "temporary_ban"
	Paused              Kind = "paused"
	Deafness            Kind = "deafness"
	ProlongedDisconnect Kind = "prolonged_disconnect"
	ExtraCompanion      Kind = "extra_companion"
	DecryptError        Kind = "decrypt_error"
	StorageFailure      Kind = "storage_failure"
	// ConfigLoaded: "config v<hash> loaded" (routine).
	ConfigLoaded Kind = "config_loaded"
	// ConfigRejected: "REJECTED: <reason>, still running v<hash>" (priority).
	ConfigRejected Kind = "config_rejected"
)

// Alert is one message for the admins. Priority alerts jump any queue.
type Alert struct {
	Kind     Kind
	Priority bool
	Text     string
}

// Alerter delivers alerts.
type Alerter interface {
	Alert(ctx context.Context, a Alert) error
}

// Log writes alerts to the structured log (identifiers masked).
type Log struct{ Logger *slog.Logger }

// Alert logs a at warn level, or error level when it is a priority alert.
func (l Log) Alert(ctx context.Context, a Alert) error {
	level := slog.LevelWarn
	if a.Priority {
		level = slog.LevelError
	}
	l.Logger.Log(ctx, level, "alert", "kind", string(a.Kind), "priority", a.Priority, "text", mask.IDs(a.Text))
	return nil
}

// Recorder keeps every alert in memory; tests use it as the fake Alerter.
type Recorder struct {
	mu     sync.Mutex
	alerts []Alert
}

// Alert records a.
func (r *Recorder) Alert(_ context.Context, a Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, a)
	return nil
}

// All returns a copy of every recorded alert.
func (r *Recorder) All() []Alert {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Alert(nil), r.alerts...)
}

// OfKind returns the recorded alerts of kind k.
func (r *Recorder) OfKind(k Kind) []Alert {
	var out []Alert
	for _, a := range r.All() {
		if a.Kind == k {
			out = append(out, a)
		}
	}
	return out
}

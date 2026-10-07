// Package alerttest holds a fake Alerter for tests in other packages.
package alerttest

import (
	"context"
	"sync"

	"github.com/hoiung/groupwarden/internal/alert"
)

// Recorder keeps every alert in memory: the fake Alerter.
type Recorder struct {
	mu     sync.Mutex
	alerts []alert.Alert
}

// Alert records a.
func (r *Recorder) Alert(_ context.Context, a alert.Alert) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, a)
	return nil
}

// All returns a copy of every recorded alert.
func (r *Recorder) All() []alert.Alert {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]alert.Alert(nil), r.alerts...)
}

// OfKind returns the recorded alerts of kind k.
func (r *Recorder) OfKind(k alert.Kind) []alert.Alert {
	var out []alert.Alert
	for _, a := range r.All() {
		if a.Kind == k {
			out = append(out, a)
		}
	}
	return out
}

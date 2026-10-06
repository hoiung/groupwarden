package app

import (
	"fmt"
	"sync"
	"time"

	"github.com/hoiung/groupwarden/internal/alert"
)

// monitor watches for a deaf connection (connected, but no event from any
// moderated group for too long) and a prolonged disconnect. Each raises one
// alert per episode.
type monitor struct {
	deafAfter, disconnectAfter time.Duration

	mu          sync.Mutex
	connected   bool
	since       time.Time // when the current connected/disconnected state began
	lastEvent   time.Time
	deaf        bool // the current connected stretch is deaf (alert sent)
	discAlerted bool
}

// setThresholds applies reloaded alert thresholds.
func (m *monitor) setThresholds(deafAfter, disconnectAfter time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deafAfter, m.disconnectAfter = deafAfter, disconnectAfter
}

func (m *monitor) onConnected(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected, m.since, m.discAlerted = true, now, false
}

func (m *monitor) onDisconnected(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connected || m.since.IsZero() {
		m.connected, m.since = false, now
	}
}

// onEvent: an event arrived, which ends a deaf episode.
func (m *monitor) onEvent(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastEvent, m.deaf = now, false
}

// check returns the alerts due at now.
func (m *monitor) check(now time.Time) []alert.Alert {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []alert.Alert
	if m.connected {
		ref := m.since
		if m.lastEvent.After(ref) {
			ref = m.lastEvent
		}
		if !m.deaf && now.Sub(ref) >= m.deafAfter {
			m.deaf = true
			out = append(out, alert.Alert{Kind: alert.Deafness, Priority: true, Text: fmt.Sprintf(
				"connected but no event from any moderated group for %s: the event stream may have stalled (check the bot phone and the linked device)",
				now.Sub(ref).Round(time.Minute))})
		}
	} else if !m.since.IsZero() && !m.discAlerted && now.Sub(m.since) >= m.disconnectAfter {
		m.discAlerted = true
		out = append(out, alert.Alert{Kind: alert.ProlongedDisconnect, Priority: true, Text: fmt.Sprintf(
			"disconnected from WhatsApp for %s and still reconnecting", now.Sub(m.since).Round(time.Minute))})
	}
	return out
}

func (m *monitor) snapshot() (connected, deaf bool, lastEvent time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected, m.deaf, m.lastEvent
}

package app

import (
	"time"

	"github.com/hoiung/groupwarden/internal/store"
)

// BeatFresh is how recent run's status write must be for run to count as
// alive (it writes every 30 seconds).
const BeatFresh = 2 * time.Minute

// Health is run's state as `groupwarden healthcheck` and the heartbeat ping
// judge it, read from the status run writes to the store.
type Health struct {
	Running    bool          // run wrote its status within BeatFresh
	BeatAge    time.Duration // how long ago it did (when Running)
	Connected  bool
	Deaf       bool
	ConfigHash string // "" until a config is loaded
	// Telegram is "1" once the admin chat took a message, "0" while Telegram
	// keeps refusing the bot, "" before the first try; TelegramSince is when
	// it was last set.
	Telegram      string
	TelegramSince time.Time
}

// ReadHealth reads Health from run's status at now.
func ReadHealth(st map[string]store.StatusValue, now time.Time) Health {
	h := Health{
		Connected:     st[store.StatusConnected].Value == "1",
		Deaf:          st[store.StatusDeaf].Value == "1",
		ConfigHash:    st[store.StatusConfigHash].Value,
		Telegram:      st[store.StatusTelegramOK].Value,
		TelegramSince: st[store.StatusTelegramOK].UpdatedAt,
	}
	if beat, ok := st[store.StatusHeartbeat]; ok && now.Sub(beat.UpdatedAt) <= BeatFresh {
		h.Running, h.BeatAge = true, now.Sub(beat.UpdatedAt)
	}
	return h
}

// OK is the healthy state: running, connected, not deaf, a config loaded and
// the admin chat reached.
func (h Health) OK() bool {
	return h.Running && h.Connected && !h.Deaf && h.ConfigHash != "" && h.Telegram == "1"
}

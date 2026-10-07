// Package alert carries messages for the human admins. `run` delivers them
// to the Telegram admin chat; one-off commands log them.
package alert

import (
	"context"
	"log/slog"

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
	// Undecided: an inbox item whose decision kept failing was set aside so
	// the items behind it could be decided (priority).
	Undecided Kind = "undecided"
	// ConfigLoaded: "config v<hash> loaded" (routine).
	ConfigLoaded Kind = "config_loaded"
	// ConfigRejected: "REJECTED: <reason>, still running v<hash>" (priority).
	ConfigRejected Kind = "config_rejected"
	// Breaker: the circuit breaker paused removals and bans (priority).
	Breaker Kind = "breaker"
	// Started and Stopping bracket a run (version, config hash).
	Started  Kind = "started"
	Stopping Kind = "stopping"
	// BotDemoted / BotRemoved: the bot lost admin in, or was removed from, a
	// moderated group.
	BotDemoted Kind = "bot_demoted"
	BotRemoved Kind = "bot_removed"
	// CoverageLost: a moderated group the bot no longer covers.
	CoverageLost Kind = "coverage_lost"
	// Coverage: the groups of a community the bot found, a new group, or a
	// group it joined or asked to join, each with what an admin must do
	// (routine).
	Coverage Kind = "coverage"
	// FewHumanAdmins: a group the bot is in has fewer than 2 human admins
	// (routine; once per episode).
	FewHumanAdmins Kind = "few_human_admins"
	// SyncFailed: the config sync could not fetch or apply the config.
	SyncFailed Kind = "sync_failed"
	// BackupFailed: the nightly backup could not be made.
	BackupFailed Kind = "backup_failed"
	// Overdue: the config sync or backup timer has not run for twice its
	// interval.
	Overdue Kind = "overdue"
	// PhoneReminder asks an admin to open WhatsApp on the bot phone (weekly,
	// with [Done]); PhoneEscalation repeats it on day 10 without [Done].
	PhoneReminder   Kind = "phone_reminder"
	PhoneEscalation Kind = "phone_escalation"
	// PinRefused: the admin chat's command list is posted but Telegram would
	// not pin it (the bot lacks the "Pin messages" right); routine.
	PinRefused Kind = "pin_refused"
	// DailyCheck: once a day at daily_check_time, the bot says it is alive
	// and working (or what is not working), with the date; routine.
	DailyCheck Kind = "daily_check"
)

// Priority reports whether an alert of kind k always jumps the admin chat's
// queue, whatever its sender set: the priority set of AC 4.1.
func (k Kind) Priority() bool {
	switch k {
	case FatalDisconnect, TemporaryBan, Paused, Breaker, Deafness, ProlongedDisconnect, BotDemoted, BotRemoved,
		CoverageLost, ExtraCompanion, ConfigRejected, SyncFailed, BackupFailed, Overdue, DecryptError, StorageFailure,
		Undecided, PhoneEscalation:
		return true
	}
	return false
}

// Alert is one message for the admins. Priority alerts jump any queue.
type Alert struct {
	Kind     Kind
	Priority bool
	Text     string
	// Buttons the admins can press, by name ("Resume", "Done", ...).
	Buttons []string
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
	if a.Priority || a.Kind.Priority() {
		level = slog.LevelError
	}
	l.Logger.Log(ctx, level, "alert", "kind", string(a.Kind), "priority", a.Priority || a.Kind.Priority(),
		"text", mask.IDs(a.Text), "buttons", a.Buttons)
	return nil
}

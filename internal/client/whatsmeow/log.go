package whatsmeow

import (
	"context"
	"fmt"
	"log/slog"

	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/hoiung/groupwarden/internal/mask"
)

// slogLogger routes the WhatsApp library's log lines into slog, masking every
// identifier. Library debug output is dropped unless slog is at debug level.
type slogLogger struct {
	l      *slog.Logger
	module string
}

var _ waLog.Logger = slogLogger{}

func newLogger(l *slog.Logger) waLog.Logger { return slogLogger{l: l, module: "whatsmeow"} }

func (s slogLogger) log(level slog.Level, msg string, args []any) {
	if !s.l.Enabled(context.Background(), level) {
		return
	}
	s.l.Log(context.Background(), level, mask.IDs(fmt.Sprintf(msg, args...)), "module", s.module)
}

func (s slogLogger) Warnf(msg string, args ...any)  { s.log(slog.LevelWarn, msg, args) }
func (s slogLogger) Errorf(msg string, args ...any) { s.log(slog.LevelError, msg, args) }
func (s slogLogger) Infof(msg string, args ...any)  { s.log(slog.LevelDebug, msg, args) }
func (s slogLogger) Debugf(msg string, args ...any) { s.log(slog.LevelDebug, msg, args) }
func (s slogLogger) Sub(module string) waLog.Logger {
	return slogLogger{l: s.l, module: s.module + "/" + module}
}

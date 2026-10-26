package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestLogAlert: a one-off command logs each alert once, at warn level, or at
// error level when it is a priority alert (by its kind or as sent), with its
// identifiers masked.
func TestLogAlert(t *testing.T) {
	for _, c := range []struct {
		name     string
		a        Alert
		level    string
		priority bool
	}{
		{"routine kind", Alert{Kind: ConfigLoaded, Text: "config vabc123 loaded"}, "WARN", false},
		{"priority kind", Alert{Kind: Breaker, Text: "removals paused"}, "ERROR", true},
		{"sent as priority", Alert{Kind: Coverage, Priority: true, Text: "new group"}, "ERROR", true},
	} {
		var buf bytes.Buffer
		l := Log{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}
		if err := l.Alert(context.Background(), c.a); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		if len(lines) != 1 {
			t.Fatalf("%s: %d log lines", c.name, len(lines))
		}
		var rec struct {
			Level, Msg, Kind, Text string
			Priority               bool
		}
		if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Level != c.level || rec.Msg != "alert" || rec.Kind != string(c.a.Kind) || rec.Priority != c.priority ||
			rec.Text != c.a.Text {
			t.Errorf("%s: logged %+v", c.name, rec)
		}
	}

	var buf bytes.Buffer
	l := Log{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	_ = l.Alert(context.Background(), Alert{Kind: CoverageLost, Text: "lost 99999000000111@g.us; removed 99999000000444@lid",
		Buttons: []string{"Resume"}})
	if out := buf.String(); strings.Contains(out, "99999000000") || !strings.Contains(out, "0111") ||
		!strings.Contains(out, "0444") || !strings.Contains(out, "Resume") {
		t.Fatalf("logged %q", out)
	}
}

// TestPriorityKinds: the kinds that always jump the admin chat's queue.
func TestPriorityKinds(t *testing.T) {
	priority := []Kind{FatalDisconnect, TemporaryBan, Breaker, Deafness, ProlongedDisconnect, BotDemoted, BotRemoved,
		CoverageLost, ExtraCompanion, ConfigRejected, SyncFailed, BackupFailed, Overdue, DecryptError, StorageFailure,
		Undecided, PhoneEscalation}
	routine := []Kind{ConfigLoaded, Started, Stopping, Coverage, FewHumanAdmins, PhoneReminder, PinRefused, DailyCheck,
		Kind("")}
	for _, k := range priority {
		if !k.Priority() {
			t.Errorf("%q is not priority", k)
		}
	}
	for _, k := range routine {
		if k.Priority() {
			t.Errorf("%q is priority", k)
		}
	}
}

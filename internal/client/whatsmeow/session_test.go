package whatsmeow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	wm "go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"

	"github.com/hoiung/groupwarden/internal/client"
	gwstore "github.com/hoiung/groupwarden/internal/store"
	"github.com/hoiung/groupwarden/internal/store/sessiontest"
)

// librarySession creates a session store the way the adapter does (the
// pinned library's own schema) and returns its path and a handle to it.
func librarySession(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "whatsmeow.db")
	c, err := sqlstore.New(context.Background(), "sqlite", gwstore.DSN(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	db, err := sql.Open("sqlite", gwstore.DSN(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return path, db
}

func columns(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), fmt.Sprintf("SELECT name FROM pragma_table_info('%s')", table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// TestSessionFixtureMatchesLibrary: the stand-in session store the tests
// above the WhatsApp layer use has the same columns as the store the pinned
// library creates.
func TestSessionFixtureMatchesLibrary(t *testing.T) {
	_, real := librarySession(t)
	fake := sessiontest.Create(t, filepath.Join(t.TempDir(), "stand-in.db"))
	for table := range sessiontest.Tables {
		want, got := columns(t, real, table), columns(t, fake, table)
		if len(want) == 0 || !slices.Equal(want, got) {
			t.Errorf("%s: library columns %v, stand-in %v", table, want, got)
		}
	}
}

// TestSessionQueriesMatchLibrarySchema: the secret purge and the member
// lookup and forget run against the library's own schema.
func TestSessionQueriesMatchLibrarySchema(t *testing.T) {
	ctx := context.Background()
	path, db := librarySession(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO whatsmeow_message_secrets (our_jid, chat_jid, sender_jid, message_id, key) VALUES (NULL, ?, ?, 'M1', x'00')`,
		"99999000000111@g.us", "99999000000444@lid")
	exec(`INSERT INTO whatsmeow_contacts (our_jid, their_jid, push_name) VALUES (NULL, '447700900123@s.whatsapp.net', 'x')`)
	exec(`INSERT INTO whatsmeow_lid_map (lid, pn) VALUES ('99999000000444', '447700900123')`)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	sess, err := gwstore.OpenSession(ctx, path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if n, err := sess.PurgeSecrets(ctx, now.Add(-time.Hour), now.Add(-time.Hour), nil); err != nil || n != 0 {
		t.Fatalf("first purge (stamps only): %d %v", n, err)
	}
	if n, err := sess.PurgeSecrets(ctx, now.Add(time.Hour), now.Add(time.Hour), nil); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	ids := []string{"99999000000444@lid", "447700900123@s.whatsapp.net"}
	m, err := sess.Member(ctx, ids)
	if err != nil || len(m.Contacts) != 1 || len(m.LIDMap) != 1 {
		t.Fatalf("member rows %+v (%v)", m, err)
	}
	if n, err := sess.ForgetMember(ctx, ids); err != nil || n != 2 {
		t.Fatalf("forget: %d %v", n, err)
	}
}

// TestCallErrorsClassified: WhatsApp's rate-limit and permission refusals
// reach callers as client.ErrRateLimited and client.ErrNotAdmin, whatever
// text came with the code; anything else is passed through.
func TestCallErrorsClassified(t *testing.T) {
	cases := []struct {
		err  error
		want error
	}{
		{&wm.IQError{Code: 429, Text: "rate-overlimit"}, client.ErrRateLimited},
		{fmt.Errorf("remove: %w", &wm.IQError{Code: 429}), client.ErrRateLimited},
		{&wm.IQError{Code: 403, Text: "forbidden"}, client.ErrNotAdmin},
		{&wm.IQError{Code: 401}, client.ErrNotAdmin},
	}
	for _, c := range cases {
		if got := callError(c.err); !errors.Is(got, c.want) || !errors.Is(got, c.err) {
			t.Errorf("%v classified as %v, want %v (wrapping the original)", c.err, got, c.want)
		}
	}
	other := &wm.IQError{Code: 500, Text: "internal-server-error"}
	if got := callError(other); errors.Is(got, client.ErrRateLimited) || errors.Is(got, client.ErrNotAdmin) || got != other {
		t.Errorf("500 classified as %v", got)
	}
	if callError(nil) != nil {
		t.Error("nil became an error")
	}
}

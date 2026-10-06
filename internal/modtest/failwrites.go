package modtest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/hoiung/groupwarden/internal/store"
)

// FailWrites makes writes to table fail (those matching when, a trigger WHEN
// condition on NEW, when it is not empty), as a full disk would, until the
// returned lift is called. The store has one connection, so temporary
// triggers on it see every write.
func FailWrites(t testing.TB, st *store.Store, table, when string) (lift func()) {
	t.Helper()
	return failWrites(t, st, table, when, "TEMP ", "temp.")
}

// FailWritesInFile is FailWrites with triggers stored in the database file,
// so they also fail writes made through other connections (a command that
// opens the store itself).
func FailWritesInFile(t testing.TB, st *store.Store, table, when string) (lift func()) {
	t.Helper()
	return failWrites(t, st, table, when, "", "")
}

func failWrites(t testing.TB, st *store.Store, table, when, temp, schema string) (lift func()) {
	t.Helper()
	cond := ""
	if when != "" {
		cond = " WHEN " + when
	}
	events := []string{"INSERT", "UPDATE"}
	if when == "" {
		events = append(events, "DELETE")
	}
	exec := func(stmts []string) {
		t.Helper()
		if err := st.Write(context.Background(), func(tx *sql.Tx) error {
			for _, s := range stmts {
				if _, err := tx.Exec(s); err != nil {
					return fmt.Errorf("%s: %w", s, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var create, drop []string
	for _, ev := range events {
		name := "fail_" + table + "_" + strings.ToLower(ev)
		create = append(create, "CREATE "+temp+"TRIGGER "+name+" BEFORE "+ev+" ON "+table+cond+
			" BEGIN SELECT RAISE(FAIL, 'database or disk is full'); END")
		drop = append(drop, "DROP TRIGGER "+schema+name)
	}
	exec(create)
	return func() { exec(drop) }
}

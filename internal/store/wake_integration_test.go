//go:build integration

package store_test

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/prismaport/prismaport/internal/testdb"
)

// Migration 00118 leaves the wake triggers in place, each calling prismaport_wake with its topic.
func TestWakeTriggersExistAfterTheMigration(t *testing.T) {
	t.Parallel()
	conn, err := pgx.Connect(t.Context(), testdb.Migrated(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	rows, err := conn.Query(t.Context(), `SELECT c.relname || '.' || t.tgname || ':' || p.proname
		FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE NOT t.tgisinternal AND p.proname = 'prismaport_wake' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"events.events_wake:prismaport_wake", "session_reveals.session_reveals_wake:prismaport_wake",
		"sessions.sessions_recheck_wake:prismaport_wake", "sessions.sessions_wake:prismaport_wake"}
	if !slices.Equal(got, want) {
		t.Fatalf("wake triggers %v, want %v", got, want)
	}
}

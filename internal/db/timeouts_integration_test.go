//go:build integration

package db_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prismaport/prismaport/internal/db"
	"github.com/prismaport/prismaport/internal/testdb"
)

// The app's pools run every statement under a server-side timeout; a hung query fails instead of
// stalling the loop that ran it. The migrator's pool (db.Open) has none.
func TestAppPoolTimesOutAHungStatement(t *testing.T) {
	t.Parallel()
	url := testdb.Empty(t)
	ctx := t.Context()
	app, err := db.OpenWith(ctx, url, db.Timeouts{Statement: 200 * time.Millisecond, IdleInTransaction: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	var v1, v2 string
	if err := app.QueryRow(ctx, "SELECT current_setting('statement_timeout'), current_setting('idle_in_transaction_session_timeout')").
		Scan(&v1, &v2); err != nil {
		t.Fatal(err)
	}
	if v1 != "200ms" || v2 != "1min" {
		t.Fatalf("statement_timeout %q idle_in_transaction_session_timeout %q", v1, v2)
	}
	_, err = app.Exec(ctx, "SELECT pg_sleep(5)")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("a hung statement was not cancelled: %v", err)
	}

	plain, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if err := plain.QueryRow(ctx, "SELECT current_setting('statement_timeout')").Scan(&v1); err != nil || v1 != "0" {
		t.Fatalf("migrator pool statement_timeout %q %v", v1, err)
	}
}

func TestAppTimeoutsAreSet(t *testing.T) {
	if db.AppTimeouts.Statement <= 0 || db.AppTimeouts.IdleInTransaction <= db.AppTimeouts.Statement {
		t.Fatalf("%+v", db.AppTimeouts)
	}
}

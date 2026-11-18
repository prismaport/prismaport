package db

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsConnLimit(t *testing.T) {
	pooler := &pgconn.PgError{Code: "XX000", Message: "(EMAXCONNSESSION) max clients reached in session mode - max clients are limited to pool_size: 15"}
	tooMany := &pgconn.PgError{Code: "53300", Message: "sorry, too many clients already"}
	auth := &pgconn.PgError{Code: "28P01", Message: "password authentication failed"}
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"pooler slots", pooler, true},
		{"too many connections", tooMany, true},
		{"wrapped", fmt.Errorf("db: read goose_db_version: %w", pooler), true},
		{"one of several hosts", errors.Join(errors.New("dial: timeout"), pooler), true},
		{"bad password", auth, false},
		{"other XX000", &pgconn.PgError{Code: "XX000", Message: "internal error"}, false},
		{"not postgres", errors.New("connection refused"), false},
		{"nil", nil, false},
	} {
		if got := IsConnLimit(c.err); got != c.want {
			t.Errorf("%s: IsConnLimit = %v, want %v", c.name, got, c.want)
		}
	}
}

package db

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsConnLimit reports whether err is the server or its pooler turning a connection away because
// every slot is taken: Postgres's too_many_connections, or the session pooler's EMAXCONNSESSION
// (which comes as XX000). It passes while a deploy overlaps the old and new processes, so a
// one-off command that meets it can wait and try again.
func IsConnLimit(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "53300" || strings.Contains(pgErr.Message, "EMAXCONNSESSION")
}

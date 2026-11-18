// Package migrations embeds the goose SQL migrations: the baseline (00100) and every migration
// after it.
package migrations

import "embed"

// FS holds the SQL migration files embedded in the binary.
//
//go:embed *.sql
var FS embed.FS

// BaselineVersion is the baseline migration's version. New migrations take the numbers after it.
const BaselineVersion = 100

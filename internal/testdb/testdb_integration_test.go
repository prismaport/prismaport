//go:build integration

package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

// A template that went away (another run on the server dropped it) is built again, and the copy
// still has every migration.
func TestMigratedRebuildsAMissingTemplate(t *testing.T) {
	// Arrange
	admin := adminURL()
	templateMu.Lock()
	templates[admin] = templatePrefix + "gone"
	templateMu.Unlock()

	// Act
	pool := Pool(t)

	// Assert
	var applied int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM goose_db_version WHERE version_id > 0").Scan(&applied); err != nil || applied == 0 {
		t.Fatalf("copy of the rebuilt template: %d migrations, %v", applied, err)
	}
	templateMu.Lock()
	got := templates[admin]
	templateMu.Unlock()
	if got != templatePrefix+migrationsHash(t) {
		t.Fatalf("cached template = %q", got)
	}
}

// Only templates of other migrations built over staleTemplate ago, or with no build time, go.
func TestDropStaleTemplatesKeepsRecentOnes(t *testing.T) {
	// Arrange
	ctx := t.Context()
	conn := connect(ctx, t, adminURL())
	// Registered first, so it closes after the drops below.
	t.Cleanup(func() { conn.Close(context.Background()) })
	// As template does, so a template another run is building is never seen undated.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(templateLock)); err != nil {
		t.Fatal(err)
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	run := hex.EncodeToString(b[:])
	built := map[string]string{
		"recent":  fmt.Sprint(time.Now().Add(-time.Hour).Unix()),
		"old":     fmt.Sprint(time.Now().Add(-staleTemplate - time.Hour).Unix()),
		"undated": "",
	}
	for kind, at := range built {
		name := templatePrefix + "t" + run + "_" + kind
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name) })
		if at != "" {
			if _, err := conn.Exec(ctx, fmt.Sprintf("COMMENT ON DATABASE %s IS '%s'", name, at)); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Act
	dropStaleTemplates(ctx, t, conn, templatePrefix+migrationsHash(t))

	// Assert
	for kind, want := range map[string]bool{"recent": true, "old": false, "undated": false} {
		t.Run(kind, func(t *testing.T) {
			var exists bool
			if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1)", templatePrefix+"t"+run+"_"+kind).Scan(&exists); err != nil {
				t.Fatal(err)
			}
			if exists != want {
				t.Errorf("%s template kept = %v, want %v", kind, exists, want)
			}
		})
	}
}

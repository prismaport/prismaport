//go:build integration

package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/db"
	"github.com/prismaport/prismaport/internal/testdb"
)

func TestCLI(t *testing.T) {
	t.Setenv("PRISMAPORT_DATABASE_URL", testdb.Empty(t))
	t.Setenv("PRISMAPORT_TREASURY_EVM_TEST", "0xfb6916095ca1df60bb79ce92ce3ea74c37c5d359")
	ctx := t.Context()
	exec := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := run(ctx, args, &out)
		return out.String(), err
	}
	if out, err := exec("migrate", "status"); err != nil || !strings.Contains(out, "schema version 0") || !strings.Contains(out, "state: empty") {
		t.Fatalf("migrate status: %q %v", out, err)
	}
	// The last migration in migrations/: bump it with each new one.
	const last = 119
	latest := fmt.Sprintf("schema version %d\n", last)
	if out, err := exec("migrate", "up"); err != nil || !strings.Contains(out, latest) {
		t.Fatalf("migrate up: %q %v", out, err)
	}
	if out, err := exec("migrate", "status"); err != nil || !strings.Contains(out, "state: baseline chain (from version 100); up to date") {
		t.Fatalf("migrate status: %q %v", out, err)
	}
	// down reverts one version at a time, down to the baseline and no further.
	if out, err := exec("migrate", "down"); err != nil || !strings.Contains(out, fmt.Sprintf("schema version %d\n", last-1)) {
		t.Fatalf("migrate down: %q %v", out, err)
	}
	for range 20 {
		out, err := exec("migrate", "down")
		if err != nil {
			t.Fatalf("migrate down to the baseline: %q %v", out, err)
		}
		if strings.Contains(out, "schema version 100\n") {
			break
		}
	}
	if out, err := exec("migrate", "down"); !errors.Is(err, db.ErrBelowBaseline) {
		t.Fatalf("migrate down below the baseline: %q %v", out, err)
	}
	if out, err := exec("migrate", "up"); err != nil || !strings.Contains(out, latest) {
		t.Fatalf("migrate up again: %q %v", out, err)
	}
	if _, err := exec("bogus"); err == nil {
		t.Fatal("unknown command accepted")
	}
}

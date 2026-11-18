//go:build integration

package api_test

import (
	"net/http"
	"testing"

	"github.com/prismaport/prismaport/internal/apptest"
)

func TestReadyzNeedsTheDatabaseWhileHealthzDoesNot(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})

	up := app.Do("GET", "/readyz", "", nil)
	app.Store.Pool().Close()
	down := app.Do("GET", "/readyz", "", nil)
	health := app.Do("GET", "/healthz", "", nil)

	if up.Status != http.StatusNoContent {
		t.Fatalf("readyz with the database up: %d %s", up.Status, up.Body)
	}
	if down.Status != http.StatusServiceUnavailable || down.ErrorCode(t) != "service_unavailable" {
		t.Fatalf("readyz with the database gone: %d %s", down.Status, down.Body)
	}
	if health.Status != http.StatusNoContent {
		t.Fatalf("healthz with the database gone: %d", health.Status)
	}
}

//go:build integration

package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/prismaport/prismaport/internal/apptest"
	"github.com/prismaport/prismaport/internal/version"
)

// GET /version over HTTP: the build's identity, never cached. (hosts_test.go covers it on every
// host through ServeHTTP, which the route-coverage contract does not see.)
func TestVersionOverHTTP(t *testing.T) {
	t.Parallel()
	app := apptest.New(t, apptest.Options{})
	resp, err := http.Get(app.Server.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var got version.Info
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &got) != nil || got.Version != version.Version {
		t.Fatalf("GET /version: %d %s", resp.StatusCode, raw)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q, want no-store", cc)
	}
}

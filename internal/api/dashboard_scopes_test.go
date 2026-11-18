package api

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/prismaport/prismaport/internal/apikey"
)

// The dashboard's Full access sends its own scope list, and its mock refuses scopes outside
// another one; both must match the scopes the API grants, or a "full access" key is quietly
// restricted.
func TestDashboardScopeListsMatchTheAPI(t *testing.T) {
	var want []string
	for _, s := range apikey.AllScopes {
		want = append(want, string(s))
	}
	slices.Sort(want)

	for _, c := range []struct{ file, list string }{
		{filepath.Join("lib", "apiKeys.ts"), "ALL_SCOPES"},
		{filepath.Join("mocks", "handlers.ts"), "KEY_SCOPES"},
	} {
		t.Run(c.file, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join("..", "..", "web", "dashboard", "src", c.file))
			if err != nil {
				t.Fatal(err)
			}
			m := regexp.MustCompile(`const ` + c.list + ` = \[([^\]]*)\]`).FindSubmatch(src)
			if m == nil {
				t.Fatalf("%s: no %s list", c.file, c.list)
			}
			var got []string
			for _, s := range regexp.MustCompile(`'([a-z_]+:[a-z]+)'`).FindAllSubmatch(m[1], -1) {
				got = append(got, string(s[1]))
			}
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("%s %s = %v, want the API's %v", c.file, c.list, got, want)
			}
		})
	}
}

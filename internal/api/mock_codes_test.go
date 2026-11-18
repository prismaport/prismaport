package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The dashboard's MSW mock is what its unit and Playwright tests run against, so every error
// code it answers with must be one the API can really return.
func TestDashboardMockErrorCodesExistInTheAPI(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "dashboard", "src", "mocks", "handlers.ts"))
	if err != nil {
		t.Fatal(err)
	}
	mock := map[string]bool{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`\berr\(\s*\d{3}\s*,\s*'([a-z_]+)'`),
		regexp.MustCompile(`\bcode:\s*'([a-z_]+)'`),
	} {
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			mock[m[1]] = true
		}
	}
	if len(mock) < 10 {
		t.Fatalf("found only %d error codes in the mock; the patterns no longer match handlers.ts", len(mock))
	}

	api := map[string]bool{}
	for _, dir := range []string{".", filepath.Join("..", "service"), filepath.Join("..", "checkout")} {
		for code := range errorCodeConsts(t, dir) {
			api[code] = true
		}
	}
	if len(api) < 10 {
		t.Fatalf("found only %d Code* constants in the API packages; the parser no longer finds them", len(api))
	}

	var missing []string
	for code := range mock {
		if !api[code] {
			missing = append(missing, code)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the dashboard mock answers with error codes the API never returns: %s", strings.Join(missing, ", "))
	}
}

// errorCodeConsts collects the string literal constants named Code* declared in the non-test Go
// files of dir; a constant that aliases another package's is found in that package.
func errorCodeConsts(t *testing.T, dir string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if !strings.HasPrefix(id.Name, "Code") || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if v, err := strconv.Unquote(lit.Value); err == nil {
							out[v] = true
						}
					}
				}
			}
		}
	}
	return out
}

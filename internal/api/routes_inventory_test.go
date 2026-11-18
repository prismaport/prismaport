package api

import (
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/service"
	checkoutweb "github.com/prismaport/prismaport/web/checkout"
	dashboardweb "github.com/prismaport/prismaport/web/dashboard"
	docsweb "github.com/prismaport/prismaport/web/docs"
	landingweb "github.com/prismaport/prismaport/web/landing"
)

// The route inventory of the route-coverage contract (ADR 0008 E-7,
// route_coverage_integration_test.go): every pattern registered on the router of a server
// mounted the way cmd/prismaport mounts it (API, checkout, dashboard, sandbox and every web
// app).

// productionServer mounts every route cmd/prismaport's runAPI and mountPages mount.
func productionServer() (*Server, error) {
	s := New(service.New(nil, service.Config{Registry: chain.Pilot()}), nil, nil)
	s.MountRoutes(nil, nil)
	if err := s.MountCheckoutPages(checkoutweb.Dist()); err != nil {
		return nil, err
	}
	if err := s.MountDashboardPages(dashboardweb.Dist()); err != nil {
		return nil, err
	}
	s.MountDocsPages(docsweb.Dist())
	s.MountLandingPages(landingweb.Site(), "")
	return s, nil
}

var handlerType = reflect.TypeFor[http.Handler]()

// muxPatterns lists the patterns registered on m. http.ServeMux has no listing API, so this
// walks its routing tree by reflection: every net/http `pattern` value it holds, through
// net/http types only and never into a registered handler. Reading unexported fields is allowed
// (only Interface() and setting are not), so nothing unsafe is involved. If a Go release
// reshapes the mux so the walk finds nothing, TestRouteInventoryReadsTheRouter fails rather than
// the coverage contract passing on an empty inventory.
func muxPatterns(m *http.ServeMux) []string {
	seen := map[uintptr]bool{}
	found := map[string]bool{}
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer:
			if v.IsNil() || seen[v.Pointer()] {
				return
			}
			seen[v.Pointer()] = true
			walk(v.Elem())
		case reflect.Interface:
			if !v.IsNil() && v.Type() != handlerType {
				walk(v.Elem())
			}
		case reflect.Struct:
			t := v.Type()
			if t.PkgPath() != "net/http" {
				return
			}
			if t.Name() == "pattern" {
				if f := v.FieldByName("str"); f.IsValid() && f.Kind() == reflect.String {
					found[f.String()] = true
				}
				return
			}
			for i := range v.NumField() {
				walk(v.Field(i))
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i))
			}
		case reflect.Map:
			it := v.MapRange()
			for it.Next() {
				walk(it.Key())
				walk(it.Value())
			}
		}
	}
	walk(reflect.ValueOf(m))
	out := make([]string, 0, len(found))
	for p := range found {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

func TestRouteInventoryReadsTheRouter(t *testing.T) {
	s, err := productionServer()
	if err != nil {
		t.Fatal(err)
	}
	got := muxPatterns(s.mux)
	if len(got) == 0 {
		t.Fatal("no pattern found on the router: net/http's ServeMux changed shape; update muxPatterns")
	}
	// Everything the server lists itself, plus routes registered straight on the mux.
	want := []string{"GET /healthz", "GET /readyz", "GET /version", "/", "GET /{$}", "GET /docs/{path...}", "GET /dashboard/{path...}", "GET /pay/{path...}"}
	for _, r := range s.MerchantRoutes() {
		want = append(want, r.Pattern)
	}
	for _, r := range s.PublicRoutes() {
		want = append(want, r.Pattern)
	}
	want = append(want, s.DashboardRoutes()...)
	for _, p := range want {
		if !slices.Contains(got, p) {
			t.Errorf("registered route %q missing from the inventory read off the router", p)
		}
	}
	// A pattern the walk invents would make the mux refuse it as a duplicate.
	shadow := http.NewServeMux()
	for _, p := range got {
		shadow.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	t.Logf("%d routes", len(got))
}

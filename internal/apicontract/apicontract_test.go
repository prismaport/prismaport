package apicontract

import (
	"encoding/json"
	"strings"
	"testing"
)

func load(t *testing.T) *Doc {
	t.Helper()
	d, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func example(t *testing.T, d *Doc, method, path, status string) []byte {
	t.Helper()
	return d.Paths[path][method].Responses[status].Content["application/json"].Example
}

func TestMatchUsesTheTemplate(t *testing.T) {
	d := load(t)
	for _, c := range []struct{ method, path, want string }{
		{"GET", "/v1/sessions/ses_123", "/v1/sessions/{id}"},
		{"POST", "/v1/sessions?x=1", "/v1/sessions"},
		{"POST", "/v1/webhook_endpoints/we_1/rotate_secret", "/v1/webhook_endpoints/{id}/rotate_secret"},
		{"GET", "/v1/checkout/links/plink_1", "/v1/checkout/links/{id}"},
	} {
		t.Run(c.path, func(t *testing.T) {
			e, ok := d.Match(c.method, c.path)
			if !ok || e.Path != c.want {
				t.Errorf("%s %s: matched %q (%v), want %s", c.method, c.path, e.Path, ok, c.want)
			}
		})
	}
	for _, c := range [][2]string{{"GET", "/v1/sessions/"}, {"PUT", "/v1/sessions/ses_1"}, {"GET", "/v1/sessions/ses_1/extra"}, {"GET", "/v1/nope"}} {
		if e, ok := d.Match(c[0], c[1]); ok {
			t.Errorf("%s %s matched %s", c[0], c[1], e.Path)
		}
	}
}

func TestCheckResponseAcceptsTheDocumentedAnswer(t *testing.T) {
	d := load(t)
	if p := d.CheckResponse("GET", "/v1/sessions/ses_1", 200, example(t, d, "get", "/v1/sessions/{id}", "200")); len(p) != 0 {
		t.Fatal(p)
	}
	if p := d.CheckResponse("GET", "/v1/sessions/ses_1", 404, []byte(`{"error":{"code":"session_not_found","message":"no such session"}}`)); len(p) != 0 {
		t.Fatal(p)
	}
}

func TestCheckResponseRefusesWhatTheContractDoesNotSay(t *testing.T) {
	d := load(t)
	session := example(t, d, "get", "/v1/sessions/{id}", "200")
	var extra map[string]any
	_ = json.Unmarshal(session, &extra)
	extra["surprise"] = true
	withExtra, _ := json.Marshal(extra)
	for name, c := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"undocumented status":    {422, `{"error":{"code":"invalid_request","message":"x"}}`, "status not documented"},
		"code of another status": {404, `{"error":{"code":"insufficient_scope","message":"x"}}`, "not in x-errors"},
		"field the schema lacks": {200, string(withExtra), "surprise is not in the schema"},
		"missing field":          {200, `{"id":"ses_1"}`, "is required"},
		"not JSON":               {200, `<html>`, "not JSON"},
	} {
		t.Run(name, func(t *testing.T) {
			p := d.CheckResponse("GET", "/v1/sessions/ses_1", c.status, []byte(c.body))
			if len(p) == 0 || !strings.Contains(strings.Join(p, "\n"), c.want) {
				t.Errorf("%s: %v, want a problem with %q", name, p, c.want)
			}
		})
	}
}

func TestCheckResponseIgnoresWhatIsOutsideTheContract(t *testing.T) {
	d := load(t)
	for _, c := range [][2]string{{"GET", "/v1/dashboard/me"}, {"GET", "/v1/no_such_route"}, {"OPTIONS", "/v1/checkout/sessions/ses_1"}, {"GET", "/docs/"}} {
		if p := d.CheckResponse(c[0], c[1], 418, []byte(`nope`)); len(p) != 0 {
			t.Errorf("%s %s: %v", c[0], c[1], p)
		}
	}
}

func TestCheckRequest(t *testing.T) {
	d := load(t)
	if p := d.CheckRequest("POST", "/v1/sessions", []byte(`{"product_id":"prod_1","client_reference_id":"order-42"}`)); len(p) != 0 {
		t.Fatal(p)
	}
	if p := d.CheckRequest("POST", "/v1/sessions", []byte(`{"product_id":"prod_1","currency":"USD"}`)); len(p) == 0 {
		t.Fatal("an undocumented field passed")
	}
	if p := d.CheckRequest("POST", "/v1/sessions", nil); len(p) != 0 {
		t.Fatal(p)
	}
}

func TestValidateKeywords(t *testing.T) {
	d := &Doc{}
	n := 3
	s := &Schema{Type: "string", MaxLength: &n, Pattern: "^[a-z]+$"}
	if p := d.Validate(s, "abcd", "x"); len(p) == 0 {
		t.Error("maxLength not enforced")
	}
	if p := d.Validate(s, "ab1", "x"); len(p) == 0 {
		t.Error("pattern not enforced")
	}
	two := 2
	list := &Schema{Type: "array", MaxItems: &two}
	if p := d.Validate(list, []any{"a", "b", "c"}, "x"); len(p) == 0 {
		t.Error("maxItems not enforced")
	}
	if p := d.Validate(list, []any{"a", "b"}, "x"); len(p) != 0 {
		t.Error(p)
	}
	if p := d.Validate(&Schema{Type: "string", Format: "date-time"}, "yesterday", "x"); len(p) == 0 {
		t.Error("date-time not enforced")
	}
	both := &Schema{OneOf: []*Schema{{Type: "string"}, {Type: []any{"string", "null"}}}}
	if p := d.Validate(both, "a", "x"); len(p) == 0 {
		t.Error("oneOf matching two alternatives passed")
	}
	nullable := &Schema{Type: []any{"string", "null"}, Enum: []any{"a", nil}}
	if p := d.Validate(nullable, nil, "x"); len(p) != 0 {
		t.Error(p)
	}
	if p := d.Validate(nullable, "b", "x"); len(p) == 0 {
		t.Error("enum not enforced")
	}
	if p := d.Validate(&Schema{Type: "integer"}, json.Number("1.5"), "x"); len(p) == 0 {
		t.Error("a decimal passed as an integer")
	}
}

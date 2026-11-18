// Package apicontract reads docs/openapi.json, the HTTP API contract the developer portal
// renders, and checks values and real answers against it. Tests use it: the drift tests in
// internal/api hold the document to the code, and apptest checks every answer the integration
// tests receive.
package apicontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Schema is the subset of JSON Schema the contract uses. Keywords lists every keyword allowed;
// a schema with any other keyword is refused by the drift tests, so nothing is silently ignored.
type Schema struct {
	Ref                  string             `json:"$ref"`
	Type                 any                `json:"type"`
	Description          string             `json:"description"`
	Properties           map[string]*Schema `json:"properties"`
	Required             []string           `json:"required"`
	AdditionalProperties json.RawMessage    `json:"additionalProperties"`
	Items                *Schema            `json:"items"`
	Enum                 []any              `json:"enum"`
	OneOf                []*Schema          `json:"oneOf"`
	Const                any                `json:"const"`
	Pattern              string             `json:"pattern"`
	Format               string             `json:"format"`
	Minimum              *float64           `json:"minimum"`
	Maximum              *float64           `json:"maximum"`
	MaxLength            *int               `json:"maxLength"`
	MaxItems             *int               `json:"maxItems"`
}

// Keywords are the schema keywords Validate understands.
var Keywords = []string{"$ref", "type", "description", "properties", "required", "additionalProperties", "items", "enum",
	"oneOf", "const", "pattern", "format", "minimum", "maximum", "maxLength", "maxItems"}

// Media is the schema and example of one content type.
type Media struct {
	Schema  *Schema         `json:"schema"`
	Example json.RawMessage `json:"example"`
}

// Param is one operation parameter of the API contract.
type Param struct {
	Name     string          `json:"name"`
	In       string          `json:"in"`
	Required bool            `json:"required"`
	Schema   *Schema         `json:"schema"`
	Example  json.RawMessage `json:"example"`
}

// Response is one operation response of the API contract.
type Response struct {
	Content map[string]Media          `json:"content"`
	Headers map[string]map[string]any `json:"headers"`
}

// ErrorRow pairs an HTTP status with an error code.
type ErrorRow struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
}

// RateLimit is the rate limit of one request bucket.
type RateLimit struct {
	Bucket    string `json:"bucket"`
	PerMinute int    `json:"per_minute"`
	Burst     int    `json:"burst"`
	Per       string `json:"per"`
}

// Operation is one API operation of the contract.
type Operation struct {
	OperationID string                `json:"operationId"`
	Security    []map[string][]string `json:"security"`
	Parameters  []Param               `json:"parameters"`
	RequestBody *struct {
		Content map[string]Media `json:"content"`
	} `json:"requestBody"`
	Responses map[string]Response `json:"responses"`
	Scope     string              `json:"x-scope"`
	RateLimit *RateLimit          `json:"x-rate-limit"`
	Errors    []ErrorRow          `json:"x-errors"`
	NotFound  *struct {
		Code string `json:"code"`
	} `json:"x-not-found"`
	TestOnly      bool   `json:"x-test-only"`
	SessionStatus string `json:"x-session-status"`
}

// ErrorCodes lists the codes x-errors documents for status.
func (o Operation) ErrorCodes(status int) []string {
	var out []string
	for _, e := range o.Errors {
		if e.Status == status {
			out = append(out, e.Code)
		}
	}
	return out
}

// HasHeader reports whether the operation takes the header parameter name.
func (o Operation) HasHeader(name string) bool {
	return slices.ContainsFunc(o.Parameters, func(p Param) bool { return p.In == "header" && p.Name == name && p.Required })
}

// Doc is the parsed API contract document.
type Doc struct {
	Security   []map[string][]string           `json:"security"`
	Paths      map[string]map[string]Operation `json:"paths"`
	Webhooks   map[string]map[string]Operation `json:"webhooks"`
	Components struct {
		Schemas map[string]*Schema `json:"schemas"`
	} `json:"components"`
	// Raw is the document as generic JSON, for walks over every schema.
	Raw map[string]any `json:"-"`
}

// Entry is one operation with its method and path template.
type Entry struct {
	Method, Path string
	Op           Operation
}

// File finds docs/openapi.json from the working directory up to the module root.
func File() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "docs", "openapi.json"), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("apicontract: no go.mod above the working directory")
		}
		dir = parent
	}
}

// Load reads and decodes the contract.
func Load() (*Doc, error) {
	path, err := File()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d Doc
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("apicontract: %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, &d.Raw); err != nil {
		return nil, err
	}
	return &d, nil
}

// Operations lists every operation, sorted by method and path.
func (d *Doc) Operations() []Entry {
	var out []Entry
	for p, methods := range d.Paths {
		for m, op := range methods {
			out = append(out, Entry{strings.ToUpper(m), p, op})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Method+out[i].Path < out[j].Method+out[j].Path })
	return out
}

// Match finds the documented operation for a concrete request path (query ignored).
func (d *Doc) Match(method, path string) (Entry, bool) {
	path, _, _ = strings.Cut(path, "?")
	for _, e := range d.Operations() {
		if e.Method == method && matchesTemplate(e.Path, path) {
			return e, true
		}
	}
	return Entry{}, false
}

func matchesTemplate(template, path string) bool {
	t := strings.Split(template, "/")
	p := strings.Split(path, "/")
	if len(t) != len(p) {
		return false
	}
	for i := range t {
		if strings.HasPrefix(t[i], "{") && strings.HasSuffix(t[i], "}") {
			if p[i] == "" {
				return false
			}
			continue
		}
		if t[i] != p[i] {
			return false
		}
	}
	return true
}

// Resolve follows the $ref of s through the component schemas and returns the schema it points to.
func (d *Doc) Resolve(s *Schema) *Schema {
	for i := 0; s != nil && s.Ref != "" && i < 16; i++ {
		s = d.Components.Schemas[strings.TrimPrefix(s.Ref, "#/components/schemas/")]
	}
	return s
}

// TypeNames returns the JSON types a schema allows.
func TypeNames(s *Schema) []string {
	switch v := s.Type.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, x := range v {
			out = append(out, fmt.Sprint(x))
		}
		return out
	}
	return nil
}

// DecodeJSON decodes keeping numbers exact, so integers can be told from decimals.
func DecodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func kind(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		if !strings.ContainsAny(x.String(), ".eE") {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// Validate checks v against s and returns one line per problem.
func (d *Doc) Validate(s *Schema, v any, at string) []string {
	s = d.Resolve(s)
	if s == nil {
		return []string{at + ": unresolved schema"}
	}
	if len(s.OneOf) > 0 {
		matches := 0
		for _, alt := range s.OneOf {
			if len(d.Validate(alt, v, at)) == 0 {
				matches++
			}
		}
		if matches != 1 {
			return []string{fmt.Sprintf("%s: %v matches %d alternatives of oneOf, want exactly 1", at, v, matches)}
		}
		return nil
	}
	if s.Const != nil && fmt.Sprint(s.Const) != fmt.Sprint(v) {
		return []string{fmt.Sprintf("%s: must be %v", at, s.Const)}
	}
	k := kind(v)
	if types := TypeNames(s); len(types) > 0 && !slices.Contains(types, k) && !(k == "integer" && slices.Contains(types, "number")) {
		return []string{fmt.Sprintf("%s: %s, schema wants %v", at, k, types)}
	}
	if len(s.Enum) > 0 && !slices.ContainsFunc(s.Enum, func(e any) bool {
		return (e == nil && v == nil) || (e != nil && v != nil && fmt.Sprint(e) == fmt.Sprint(v))
	}) {
		return []string{fmt.Sprintf("%s: %v is not one of %v", at, v, s.Enum)}
	}
	var problems []string
	switch x := v.(type) {
	case string:
		if s.Pattern != "" && !regexp.MustCompile(s.Pattern).MatchString(x) {
			problems = append(problems, fmt.Sprintf("%s: %q does not match %s", at, x, s.Pattern))
		}
		if s.MaxLength != nil && utf8.RuneCountInString(x) > *s.MaxLength {
			problems = append(problems, fmt.Sprintf("%s: longer than %d characters", at, *s.MaxLength))
		}
		if s.Format == "date-time" {
			if _, err := time.Parse(time.RFC3339, x); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %q is not an RFC 3339 time", at, x))
			}
		}
	case json.Number:
		f, _ := x.Float64()
		if (s.Minimum != nil && f < *s.Minimum) || (s.Maximum != nil && f > *s.Maximum) {
			problems = append(problems, fmt.Sprintf("%s: %v is out of range", at, x))
		}
	case []any:
		if s.MaxItems != nil && len(x) > *s.MaxItems {
			problems = append(problems, fmt.Sprintf("%s: more than %d items", at, *s.MaxItems))
		}
		if s.Items != nil {
			for i, e := range x {
				problems = append(problems, d.Validate(s.Items, e, fmt.Sprintf("%s[%d]", at, i))...)
			}
		}
	case map[string]any:
		for _, r := range s.Required {
			if _, ok := x[r]; !ok {
				problems = append(problems, fmt.Sprintf("%s.%s is required", at, r))
			}
		}
		closed := string(s.AdditionalProperties) == "false"
		var extra *Schema
		if len(s.AdditionalProperties) > 0 && !closed && string(s.AdditionalProperties) != "true" {
			extra = &Schema{}
			if err := json.Unmarshal(s.AdditionalProperties, extra); err != nil {
				extra = nil
			}
		}
		for k, e := range x {
			switch p, ok := s.Properties[k]; {
			case ok:
				problems = append(problems, d.Validate(p, e, at+"."+k)...)
			case closed:
				problems = append(problems, fmt.Sprintf("%s.%s is not in the schema", at, k))
			case extra != nil:
				problems = append(problems, d.Validate(extra, e, at+"."+k)...)
			}
		}
	}
	return problems
}

// CheckRequest checks a request body the API accepted against the documented request schema,
// so every field a successful request used is documented and every documented required field
// was needed.
func (d *Doc) CheckRequest(method, path string, body []byte) []string {
	if len(bytes.TrimSpace(body)) == 0 || !strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v1/dashboard/") {
		return nil
	}
	e, ok := d.Match(method, path)
	if !ok || e.Op.RequestBody == nil {
		return nil
	}
	v, err := DecodeJSON(body)
	if err != nil {
		return nil
	}
	m := e.Op.RequestBody.Content["application/json"]
	return d.Validate(m.Schema, v, fmt.Sprintf("%s %s (%s %s) accepted request", method, path, e.Method, e.Path))
}

// CheckResponse checks a real answer of the API against the contract: a documented status, a
// body that validates against that status's schema, and for errors a code x-errors lists for
// that status. Requests outside the contract (the dashboard API, unknown routes) are not checked.
func (d *Doc) CheckResponse(method, path string, status int, body []byte) []string {
	if !strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v1/dashboard/") || method == "OPTIONS" {
		return nil
	}
	e, ok := d.Match(method, path)
	if !ok {
		return nil
	}
	at := fmt.Sprintf("%s %s (%s %s) %d", method, path, e.Method, e.Path, status)
	r, ok := e.Op.Responses[strconv.Itoa(status)]
	if !ok {
		return []string{at + ": status not documented"}
	}
	m, ok := r.Content["application/json"]
	if !ok {
		return nil
	}
	v, err := DecodeJSON(body)
	if err != nil {
		return []string{fmt.Sprintf("%s: body is not JSON: %v", at, err)}
	}
	problems := d.Validate(m.Schema, v, at+" body")
	if status >= 400 {
		if obj, ok := v.(map[string]any); ok {
			if errObj, ok := obj["error"].(map[string]any); ok {
				code := fmt.Sprint(errObj["code"])
				if !slices.Contains(e.Op.ErrorCodes(status), code) {
					problems = append(problems, fmt.Sprintf("%s: error code %s is not in x-errors for %d", at, code, status))
				}
			}
		}
	}
	return problems
}

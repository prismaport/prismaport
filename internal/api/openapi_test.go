package api

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/prismaport/prismaport/internal/apicontract"
	"github.com/prismaport/prismaport/internal/chain"
	"github.com/prismaport/prismaport/internal/chainpause"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/ids"
	"github.com/prismaport/prismaport/internal/payout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/session"
	"github.com/prismaport/prismaport/internal/view"
	"github.com/prismaport/prismaport/internal/webhook"
)

// docs/openapi.json is the contract the developer portal renders and clients generate from.
// These tests hold it to the code: routes, scopes, rate limits, success statuses and response
// types, response shapes, enums, error codes and every example. The integration tests add the
// last link: apptest checks every answer they receive against the contract.

func loadOpenAPI(t *testing.T) *apicontract.Doc {
	t.Helper()
	d, err := apicontract.Load()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func documentedServer() *Server {
	s := New(service.New(nil, service.Config{Registry: chain.Pilot()}), nil, nil)
	s.MountRoutes(nil, nil)
	return s
}

// notInContract are public routes served outside the JSON API, so docs/openapi.json leaves them
// out: the logo file is the PNG a checkout page shows, linked from merchant.logo_url
// of a public session or link.
var notInContract = map[string]bool{"GET /v1/checkout/logos/{file}": true}

func TestOpenAPIMatchesRoutes(t *testing.T) {
	d := loadOpenAPI(t)
	s := documentedServer()

	code := map[string]string{}
	for _, r := range s.MerchantRoutes() {
		if strings.Contains(r.Pattern, " /v1/") && !strings.Contains(r.Pattern, " /v1/dashboard") {
			code[r.Pattern] = string(r.Scope)
		}
	}
	for _, p := range s.PublicRoutes() {
		if !notInContract[p.Pattern] {
			code[p.Pattern] = ""
		}
	}
	if len(d.Security) != 0 {
		t.Errorf("the document sets a root security %v; every operation must state its own", d.Security)
	}

	spec := map[string]apicontract.Operation{}
	for _, e := range d.Operations() {
		spec[e.Method+" "+e.Path] = e.Op
	}
	for pattern := range code {
		if _, ok := spec[pattern]; !ok {
			t.Errorf("%s is served but missing from docs/openapi.json", pattern)
		}
	}
	for pattern, op := range spec {
		scope, ok := code[pattern]
		if !ok {
			t.Errorf("%s (%s) is documented but not served", pattern, op.OperationID)
			continue
		}
		if op.Scope != scope {
			t.Errorf("%s: x-scope %q, code requires %q", pattern, op.Scope, scope)
		}
		public := scope == ""
		if public && (op.Security == nil || len(op.Security) != 0) {
			t.Errorf("%s is public: security must be [] (got %v)", pattern, op.Security)
		}
		if !public && (len(op.Security) != 1 || op.Security[0]["secretKey"] == nil) {
			t.Errorf("%s needs a secret key but documents security %v", pattern, op.Security)
		}
	}
}

func TestOpenAPIPathParameters(t *testing.T) {
	d := loadOpenAPI(t)
	braces := regexp.MustCompile(`\{(\w+)\}`)
	for _, e := range d.Operations() {
		var want, got []string
		for _, m := range braces.FindAllStringSubmatch(e.Path, -1) {
			want = append(want, m[1])
		}
		for _, p := range e.Op.Parameters {
			if p.In == "path" {
				got = append(got, p.Name)
				if !p.Required {
					t.Errorf("%s %s: path parameter %s must be required", e.Method, e.Path, p.Name)
				}
			}
		}
		if !slices.Equal(want, got) {
			t.Errorf("%s %s: path parameters %v, path has %v", e.Method, e.Path, got, want)
		}
	}
}

// The webhook test route also takes the key's api_write token in route(); its description says so.
const webhookTestRoute = "POST /v1/webhook_endpoints/{id}/test"

func TestOpenAPIRateLimits(t *testing.T) {
	d := loadOpenAPI(t)
	buckets := map[string]string{}
	for _, p := range documentedServer().PublicRoutes() {
		buckets[p.Pattern] = p.Bucket
	}
	for _, e := range d.Operations() {
		at := e.Method + " " + e.Path
		rl := e.Op.RateLimit
		if e.Op.Scope != "" {
			want, per := RouteAPIWrite, "key"
			if e.Method == "GET" {
				want = RouteAPIRead
			}
			if at == webhookTestRoute {
				want, per = RouteWebhookTest, "merchant"
			}
			if rl == nil || rl.Bucket != want || rl.Per != per {
				t.Errorf("%s: x-rate-limit %+v, the route takes %s per %s", at, rl, want, per)
				continue
			}
		} else {
			bucket := buckets[at]
			switch {
			case bucket == "" && rl != nil:
				t.Errorf("%s: documents a rate limit, the route has none", at)
			case bucket != "" && (rl == nil || rl.Bucket != bucket || rl.Per != "ip"):
				t.Errorf("%s: x-rate-limit %+v, the route takes %s per ip", at, rl, bucket)
			}
		}
		if rl == nil {
			continue
		}
		l, ok := DefaultLimits()[rl.Bucket]
		if !ok {
			t.Errorf("%s: unknown rate limit bucket %q", at, rl.Bucket)
			continue
		}
		if l.PerMinute != rl.PerMinute || l.Burst != rl.Burst {
			t.Errorf("%s: documents %d/min burst %d, %s is %d/min burst %d", at, rl.PerMinute, rl.Burst, rl.Bucket, l.PerMinute, l.Burst)
		}
	}
}

// operationResults is what each handler writes on success: the status (statuses, comma
// separated, when it answers more than one) and the Go type, and whether it honours
// Idempotency-Key. A new route fails the test until it is listed here.
var operationResults = map[string]struct {
	status string
	typ    reflect.Type
	idem   bool
}{
	"POST /v1/customer_addresses":                           {"200,201", reflect.TypeOf(view.CustomerAddress{}), true},
	"GET /v1/customer_addresses":                            {"200", reflect.TypeOf(service.CustomerAddressList{}), false},
	"GET /v1/customer_addresses/{id}":                       {"200", reflect.TypeOf(view.CustomerAddress{}), false},
	"POST /v1/customer_addresses/{id}/rotate":               {"200", reflect.TypeOf(view.CustomerAddress{}), false},
	"GET /v1/customer_addresses/{id}/sessions":              {"200", reflect.TypeOf(service.AddressSessionList{}), false},
	"POST /v1/products":                                     {"200", reflect.TypeOf(view.Product{}), false},
	"GET /v1/products":                                      {"200", reflect.TypeOf(service.ProductList{}), false},
	"GET /v1/products/{id}":                                 {"200", reflect.TypeOf(view.Product{}), false},
	"PATCH /v1/products/{id}":                               {"200", reflect.TypeOf(view.Product{}), false},
	"POST /v1/payment_links":                                {"200", reflect.TypeOf(service.PaymentLink{}), true},
	"GET /v1/payment_links":                                 {"200", reflect.TypeOf(service.PaymentLinkList{}), false},
	"GET /v1/payment_links/{id}":                            {"200", reflect.TypeOf(service.PaymentLink{}), false},
	"PATCH /v1/payment_links/{id}":                          {"200", reflect.TypeOf(service.PaymentLink{}), false},
	"POST /v1/sessions":                                     {"200", reflect.TypeOf(view.Session{}), true},
	"GET /v1/sessions/{id}":                                 {"200", reflect.TypeOf(view.Session{}), false},
	"POST /v1/sessions/{id}/reveal":                         {"200", reflect.TypeOf(view.Session{}), false},
	"POST /v1/sessions/{id}/recheck":                        {"202", reflect.TypeOf(view.Session{}), false},
	"GET /v1/events":                                        {"200", reflect.TypeOf(service.EventList{}), false},
	"GET /v1/wallets":                                       {"200", reflect.TypeOf(service.WalletList{}), false},
	"GET /v1/wallets/{id}":                                  {"200", reflect.TypeOf(service.WalletView{}), false},
	"POST /v1/webhook_endpoints":                            {"200", reflect.TypeOf(service.WebhookEndpointView{}), false},
	"GET /v1/webhook_endpoints":                             {"200", reflect.TypeOf(WebhookEndpointList{}), false},
	"PATCH /v1/webhook_endpoints/{id}":                      {"200", reflect.TypeOf(service.WebhookEndpointView{}), false},
	"DELETE /v1/webhook_endpoints/{id}":                     {"200", reflect.TypeOf(Deleted{}), false},
	"POST /v1/webhook_endpoints/{id}/rotate_secret":         {"200", reflect.TypeOf(service.WebhookEndpointView{}), false},
	"POST /v1/webhook_endpoints/{id}/test":                  {"200", reflect.TypeOf(webhook.TestResult{}), false},
	"POST /v1/test_helpers/sessions/{id}/simulate":          {"200", reflect.TypeOf(service.SimulateResult{}), false},
	"GET /v1/checkout/sessions/{id}":                        {"200", reflect.TypeOf(checkout.PublicSession{}), false},
	"POST /v1/checkout/sessions/{id}/reveal":                {"200", reflect.TypeOf(checkout.PublicSession{}), false},
	"POST /v1/checkout/sessions/{id}/eip3009/typed_data":    {"200", reflect.TypeOf(checkout.TypedDataResponse{}), false},
	"POST /v1/checkout/sessions/{id}/eip3009/submit":        {"202", reflect.TypeOf(checkout.SubmitResponse{}), false},
	"POST /v1/checkout/sessions/{id}/test_helpers/simulate": {"200", reflect.TypeOf(checkout.TestPanelResult{}), false},
	"POST /v1/checkout/sessions/{id}/solana/transaction":    {"200", reflect.TypeOf(checkout.WalletTxResponse{}), false},
	"POST /v1/checkout/sessions/{id}/solana/submit":         {"202", reflect.TypeOf(checkout.SubmitResponse{}), false},
	"GET /v1/checkout/sessions/{id}/solana/pay":             {"200", reflect.TypeOf(checkout.SolanaPayInfo{}), false},
	"POST /v1/checkout/sessions/{id}/solana/pay":            {"200", reflect.TypeOf(checkout.SolanaPayTransaction{}), false},
	"GET /v1/checkout/networks":                             {"200", reflect.TypeOf(view.NetworkList{}), false},
	"GET /v1/checkout/links/{id}":                           {"200", reflect.TypeOf(service.PublicLink{}), false},
	"POST /v1/checkout/links/{id}/sessions":                 {"200", reflect.TypeOf(checkout.PublicSession{}), true},
	"GET /v1/checkout/addresses/{id}":                       {"200", reflect.TypeOf(service.PublicAddress{}), false},
	"POST /v1/checkout/addresses/{id}/sessions":             {"200", reflect.TypeOf(checkout.PublicSession{}), false},
}

func TestOpenAPIOperationResults(t *testing.T) {
	d := loadOpenAPI(t)
	for _, e := range d.Operations() {
		at := e.Method + " " + e.Path
		want, ok := operationResults[at]
		if !ok {
			t.Errorf("%s: add it to operationResults with its success status and Go type", at)
			continue
		}
		var success []string
		for status := range e.Op.Responses {
			if strings.HasPrefix(status, "2") {
				success = append(success, status)
			}
		}
		slices.Sort(success)
		statuses := strings.Split(want.status, ",")
		if !slices.Equal(success, statuses) {
			t.Errorf("%s: documents success %v, the handler writes %s", at, success, want.status)
			continue
		}
		for _, status := range statuses {
			m := e.Op.Responses[status].Content["application/json"]
			name := strings.TrimPrefix(m.Schema.Ref, "#/components/schemas/")
			if schemaTypes[name] != want.typ {
				t.Errorf("%s %s: documents %s (%v), the handler writes %v", at, status, name, schemaTypes[name], want.typ)
			}
			_, replay := e.Op.Responses[status].Headers["Idempotent-Replayed"]
			if replay != want.idem {
				t.Errorf("%s %s: Idempotent-Replayed header documented = %v, the handler sends it: %v", at, status, replay, want.idem)
			}
		}
		// POST /v1/customer_addresses takes the key but does not require it.
		got := slices.ContainsFunc(e.Op.Parameters, func(p apicontract.Param) bool { return p.In == "header" && p.Name == "Idempotency-Key" })
		if got != want.idem {
			t.Errorf("%s: Idempotency-Key documented = %v, the handler: %v", at, got, want.idem)
		}
	}
}

// schemaTypes maps every response schema to the Go type the handler encodes. Schemas that only
// describe a request are listed in requestSchemas.
var schemaTypes = map[string]reflect.Type{
	"Error":                reflect.TypeOf(view.ErrorBody{}),
	"PayoutItem":           reflect.TypeOf(view.PayoutItem{}),
	"LinkTotals":           reflect.TypeOf(view.LinkTotals{}),
	"PaymentMethod":        reflect.TypeOf(view.PaymentMethod{}),
	"Product":              reflect.TypeOf(view.Product{}),
	"ProductNetwork":       reflect.TypeOf(view.ProductNetwork{}),
	"ProductList":          reflect.TypeOf(service.ProductList{}),
	"AssetTotal":           reflect.TypeOf(service.AssetTotal{}),
	"LinkStats":            reflect.TypeOf(service.LinkStats{}),
	"PaymentLink":          reflect.TypeOf(service.PaymentLink{}),
	"PaymentLinkList":      reflect.TypeOf(service.PaymentLinkList{}),
	"PayTo":                reflect.TypeOf(view.PayTo{}),
	"Payment":              reflect.TypeOf(view.Payment{}),
	"SettlementPayout":     reflect.TypeOf(view.SettlementPayout{}),
	"Settlement":           reflect.TypeOf(view.Settlement{}),
	"PayoutRecipient":      reflect.TypeOf(view.PayoutRecipient{}),
	"Session":              reflect.TypeOf(view.Session{}),
	"AddressPayTo":         reflect.TypeOf(view.AddressPayTo{}),
	"CustomerAddress":      reflect.TypeOf(view.CustomerAddress{}),
	"CustomerAddressList":  reflect.TypeOf(service.CustomerAddressList{}),
	"AddressSessionList":   reflect.TypeOf(service.AddressSessionList{}),
	"Event":                reflect.TypeOf(view.Envelope{}),
	"EventList":            reflect.TypeOf(service.EventList{}),
	"Wallet":               reflect.TypeOf(service.WalletView{}),
	"WalletList":           reflect.TypeOf(service.WalletList{}),
	"WebhookEndpoint":      reflect.TypeOf(service.WebhookEndpointView{}),
	"WebhookEndpointList":  reflect.TypeOf(WebhookEndpointList{}),
	"Deleted":              reflect.TypeOf(Deleted{}),
	"Delivery":             reflect.TypeOf(service.DeliveryView{}),
	"TestHeader":           reflect.TypeOf(webhook.Header{}),
	"TestRequest":          reflect.TypeOf(webhook.TestRequest{}),
	"TestResponse":         reflect.TypeOf(webhook.TestResponse{}),
	"TestEventRef":         reflect.TypeOf(webhook.TestEventRef{}),
	"TestEventResult":      reflect.TypeOf(webhook.TestResult{}),
	"SimEvent":             reflect.TypeOf(service.SimEvent{}),
	"SimulateResult":       reflect.TypeOf(service.SimulateResult{}),
	"PublicProduct":        reflect.TypeOf(checkout.PublicProduct{}),
	"PublicMerchant":       reflect.TypeOf(service.PublicMerchant{}),
	"PublicPayTo":          reflect.TypeOf(checkout.PublicPayTo{}),
	"PublicPayment":        reflect.TypeOf(checkout.PublicPayment{}),
	"PublicSigned":         reflect.TypeOf(checkout.PublicSigned{}),
	"PublicSession":        reflect.TypeOf(checkout.PublicSession{}),
	"PublicLink":           reflect.TypeOf(service.PublicLink{}),
	"PublicAddress":        reflect.TypeOf(service.PublicAddress{}),
	"PublicAddressPayTo":   reflect.TypeOf(service.PublicAddressPayTo{}),
	"PublicAddressSession": reflect.TypeOf(service.PublicAddressSession{}),
	"NetworkAsset":         reflect.TypeOf(view.NetworkAsset{}),
	"Network":              reflect.TypeOf(view.Network{}),
	"NetworkList":          reflect.TypeOf(view.NetworkList{}),
	"TypedField":           reflect.TypeOf(checkout.TypedField{}),
	"TypedDomain":          reflect.TypeOf(checkout.TypedDomain{}),
	"TypedMessage":         reflect.TypeOf(checkout.TypedMessage{}),
	"TypedData":            reflect.TypeOf(checkout.TypedData{}),
	"TypedDataResult":      reflect.TypeOf(checkout.TypedDataResponse{}),
	"SignedPaymentJob":     reflect.TypeOf(checkout.SubmitResponse{}),
	"SolanaTransaction":    reflect.TypeOf(checkout.WalletTxResponse{}),
	"SolanaPayInfo":        reflect.TypeOf(checkout.SolanaPayInfo{}),
	"SolanaPayTransaction": reflect.TypeOf(checkout.SolanaPayTransaction{}),
	"SolanaPayError":       reflect.TypeOf(SolanaPayError{}),
	"TestPanelEvent":       reflect.TypeOf(checkout.TestPanelEvent{}),
	"TestPanelResult":      reflect.TypeOf(checkout.TestPanelResult{}),
}

var requestSchemas = []string{"PayoutItemInput", "NetworkInput"}

var (
	rawMessage = reflect.TypeOf(json.RawMessage{})
	marshaler  = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
)

func TestOpenAPISchemasMatchGoTypes(t *testing.T) {
	d := loadOpenAPI(t)
	for name := range d.Components.Schemas {
		if _, ok := schemaTypes[name]; !ok && !slices.Contains(requestSchemas, name) {
			t.Errorf("schema %s is not mapped to a Go type in schemaTypes (or listed in requestSchemas)", name)
		}
	}
	for name, typ := range schemaTypes {
		s, ok := d.Components.Schemas[name]
		if !ok {
			t.Errorf("schema %s is missing from docs/openapi.json", name)
			continue
		}
		for _, p := range compareField(d, s, typ, name, false) {
			t.Error(p)
		}
	}
}

// nullable reports whether s allows null, and the schema of the non-null alternative.
func nullable(s *apicontract.Schema) (bool, *apicontract.Schema) {
	if len(s.OneOf) == 2 {
		for i, alt := range s.OneOf {
			if slices.Equal(apicontract.TypeNames(alt), []string{"null"}) {
				return true, s.OneOf[1-i]
			}
		}
	}
	return slices.Contains(apicontract.TypeNames(s), "null"), s
}

// compareField compares one schema with one Go type. An omitempty pointer is left out when nil,
// never sent as null.
func compareField(d *apicontract.Doc, s *apicontract.Schema, typ reflect.Type, at string, omitempty bool) []string {
	var problems []string
	isNull, inner := nullable(s)
	if typ == rawMessage {
		if inner.Ref == "" && inner.Items == nil {
			problems = append(problems, at+": raw JSON must be documented with a schema")
		}
		return problems
	}
	if typ.Kind() == reflect.Interface || (typ.Implements(marshaler) && typ.Kind() != reflect.Slice) {
		return append(problems, fmt.Sprintf("%s: %v encodes itself; describe it with a plain type so it can be checked", at, typ))
	}
	if typ.Kind() == reflect.Pointer {
		if !isNull && !omitempty {
			problems = append(problems, at+": the Go field is a pointer (can be null) but the schema does not allow null")
		}
		typ = typ.Elem()
	} else if isNull {
		problems = append(problems, at+": the schema allows null but the Go field is never null")
	}
	if inner.Ref != "" {
		name := strings.TrimPrefix(inner.Ref, "#/components/schemas/")
		if want, ok := schemaTypes[name]; ok && want != typ {
			problems = append(problems, fmt.Sprintf("%s: refers to %s (%v) but the Go type is %v", at, name, want, typ))
		}
		return problems
	}
	kinds := apicontract.TypeNames(inner)
	has := func(k string) bool { return slices.Contains(kinds, k) }
	switch typ.Kind() {
	case reflect.String:
		if !has("string") {
			problems = append(problems, fmt.Sprintf("%s: Go string, schema type %v", at, kinds))
		}
	case reflect.Bool:
		if !has("boolean") {
			problems = append(problems, fmt.Sprintf("%s: Go bool, schema type %v", at, kinds))
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if !has("integer") {
			problems = append(problems, fmt.Sprintf("%s: Go integer, schema type %v", at, kinds))
		}
	case reflect.Slice:
		if !has("array") || inner.Items == nil {
			problems = append(problems, fmt.Sprintf("%s: Go slice, schema type %v", at, kinds))
			break
		}
		problems = append(problems, compareField(d, inner.Items, typ.Elem(), at+"[]", false)...)
	case reflect.Map:
		var values apicontract.Schema
		if !has("object") || json.Unmarshal(inner.AdditionalProperties, &values) != nil {
			problems = append(problems, fmt.Sprintf("%s: Go map, schema needs an object with additionalProperties", at))
			break
		}
		problems = append(problems, compareField(d, &values, typ.Elem(), at+"{}", false)...)
	case reflect.Struct:
		if !has("object") {
			problems = append(problems, fmt.Sprintf("%s: Go struct, schema type %v", at, kinds))
			break
		}
		if string(inner.AdditionalProperties) != "false" {
			problems = append(problems, at+": object schemas of Go structs must set additionalProperties false")
		}
		fields := map[string]reflect.StructField{}
		var required []string
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("json")
			if tag == "-" {
				continue
			}
			if f.Anonymous || tag == "" {
				problems = append(problems, fmt.Sprintf("%s.%s: embedded or untagged fields are not compared; tag it", at, f.Name))
				continue
			}
			name, opts, _ := strings.Cut(tag, ",")
			fields[name] = f
			if !strings.Contains(opts, "omitempty") {
				required = append(required, name)
			}
		}
		for name := range inner.Properties {
			if _, ok := fields[name]; !ok {
				problems = append(problems, fmt.Sprintf("%s.%s is documented but the Go type has no such field", at, name))
			}
		}
		for name, f := range fields {
			p, ok := inner.Properties[name]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s.%s is in the Go type but not documented", at, name))
				continue
			}
			_, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			problems = append(problems, compareField(d, p, f.Type, at+"."+name, strings.Contains(opts, "omitempty"))...)
		}
		sort.Strings(required)
		docReq := append([]string(nil), inner.Required...)
		sort.Strings(docReq)
		if !slices.Equal(required, docReq) {
			problems = append(problems, fmt.Sprintf("%s: required %v, but the Go type always sends %v (omitempty fields are optional)", at, docReq, required))
		}
	default:
		problems = append(problems, fmt.Sprintf("%s: Go kind %v is not compared", at, typ.Kind()))
	}
	return problems
}

// Every schema uses only keywords the validator understands, so nothing is silently ignored.
func TestOpenAPISchemaKeywords(t *testing.T) {
	d := loadOpenAPI(t)
	var walk func(v any, at string)
	walk = func(v any, at string) {
		s, ok := v.(map[string]any)
		if !ok {
			return
		}
		for k, sub := range s {
			if !slices.Contains(apicontract.Keywords, k) {
				t.Errorf("%s: schema keyword %q is not supported by apicontract.Validate", at, k)
			}
			switch k {
			case "properties":
				for name, p := range sub.(map[string]any) {
					walk(p, at+"."+name)
				}
			case "items", "additionalProperties":
				walk(sub, at+"."+k)
			case "oneOf":
				for i, alt := range sub.([]any) {
					walk(alt, fmt.Sprintf("%s.oneOf[%d]", at, i))
				}
			}
		}
	}
	var find func(v any, at string)
	find = func(v any, at string) {
		switch x := v.(type) {
		case map[string]any:
			for k, sub := range x {
				if k == "schema" {
					walk(sub, at+".schema")
				} else if k != "example" {
					find(sub, at+"."+k)
				}
			}
		case []any:
			for i, sub := range x {
				find(sub, fmt.Sprintf("%s[%d]", at, i))
			}
		}
	}
	for name, s := range d.Raw["components"].(map[string]any)["schemas"].(map[string]any) {
		walk(s, "components.schemas."+name)
	}
	find(d.Raw["paths"], "paths")
	find(d.Raw["webhooks"], "webhooks")
}

// The docs site renders every property of a component schema as a row, so a property without a
// description is an empty row, and an enum value its description does not name is a value the
// reader has to guess. Both fail here.
func TestOpenAPISchemaPropertiesDescribed(t *testing.T) {
	d := loadOpenAPI(t)
	var walk func(v any, at string)
	walk = func(v any, at string) {
		s, ok := v.(map[string]any)
		if !ok {
			return
		}
		props, _ := s["properties"].(map[string]any)
		for name, pv := range props {
			p, ok := pv.(map[string]any)
			if !ok {
				continue
			}
			here := at + "." + name
			desc, _ := p["description"].(string)
			if strings.TrimSpace(desc) == "" {
				t.Errorf("%s: no description", here)
			} else if vals, ok := p["enum"].([]any); ok {
				for _, v := range vals {
					if str, ok := v.(string); ok && str != "" && !strings.Contains(desc, str) {
						t.Errorf("%s: the description does not explain the enum value %q", here, str)
					}
				}
			}
			walk(p, here)
		}
		if items, ok := s["items"]; ok {
			walk(items, at)
		}
		for _, k := range []string{"allOf", "oneOf", "anyOf"} {
			alts, _ := s[k].([]any)
			for _, alt := range alts {
				walk(alt, at)
			}
		}
	}
	for name, s := range d.Raw["components"].(map[string]any)["schemas"].(map[string]any) {
		walk(s, name)
	}
}

type example struct {
	at     string
	schema *apicontract.Schema
	raw    json.RawMessage
}

func examples(d *apicontract.Doc) []example {
	var out []example
	all := d.Operations()
	for name, methods := range d.Webhooks {
		for m, op := range methods {
			all = append(all, apicontract.Entry{Method: strings.ToUpper(m), Path: "webhook " + name, Op: op})
		}
	}
	for _, e := range all {
		at := e.Method + " " + e.Path
		for _, p := range e.Op.Parameters {
			if len(p.Example) > 0 {
				out = append(out, example{at + " parameter " + p.Name, p.Schema, p.Example})
			}
		}
		if e.Op.RequestBody != nil {
			m := e.Op.RequestBody.Content["application/json"]
			out = append(out, example{at + " request", m.Schema, m.Example})
		}
		for status, r := range e.Op.Responses {
			if m, ok := r.Content["application/json"]; ok {
				out = append(out, example{at + " " + status, m.Schema, m.Example})
			}
		}
	}
	return out
}

func TestOpenAPIExamplesValidate(t *testing.T) {
	d := loadOpenAPI(t)
	for _, ex := range examples(d) {
		if len(ex.raw) == 0 {
			if !strings.Contains(ex.at, " parameter ") {
				t.Errorf("%s: no example", ex.at)
			}
			continue
		}
		v, err := apicontract.DecodeJSON(ex.raw)
		if err != nil {
			t.Errorf("%s: %v", ex.at, err)
			continue
		}
		for _, p := range d.Validate(ex.schema, v, ex.at) {
			t.Error(p)
		}
	}
	for _, e := range d.Operations() {
		at := e.Method + " " + e.Path
		for status, r := range e.Op.Responses {
			if status[0] != '4' && status[0] != '5' {
				continue
			}
			var body view.ErrorBody
			_ = json.Unmarshal(r.Content["application/json"].Example, &body)
			var n int
			if _, err := fmt.Sscan(status, &n); err != nil {
				t.Errorf("%s: response status %q is not a number", at, status)
				continue
			}
			codes := e.Op.ErrorCodes(n)
			if len(codes) == 0 {
				t.Errorf("%s: response %s has no x-errors row", at, status)
			} else if !slices.Contains(codes, body.Error.Code) {
				t.Errorf("%s %s: example code %s is not in x-errors for %s", at, status, body.Error.Code, status)
			}
		}
		for _, row := range e.Op.Errors {
			if _, ok := e.Op.Responses[fmt.Sprint(row.Status)]; !ok {
				t.Errorf("%s: x-errors lists %d but responses has no %d", at, row.Status, row.Status)
			}
		}
		if nf := e.Op.NotFound; nf != nil && !slices.Contains(e.Op.ErrorCodes(404), nf.Code) {
			t.Errorf("%s: x-not-found %s is not a documented 404", at, nf.Code)
		}
	}
}

// Example ids are real ids (prefix and ULID), so a developer can rely on their format.
func TestOpenAPIExampleIDs(t *testing.T) {
	d := loadOpenAPI(t)
	idLike := regexp.MustCompile(`^(prod|plink|caddr|ses|evt|wal|we|pay|txj|whd|key)_[0-9A-Z]{8,}`)
	var walk func(v any, at string)
	walk = func(v any, at string) {
		switch x := v.(type) {
		case string:
			if m := idLike.FindStringSubmatch(x); m != nil && !strings.Contains(x, "…") && !ids.Valid(m[1], x) {
				t.Errorf("%s: %q is not a valid %s_ id", at, x, m[1])
			}
		case map[string]any:
			for k, sub := range x {
				walk(sub, at+"."+k)
			}
		case []any:
			for i, sub := range x {
				walk(sub, fmt.Sprintf("%s[%d]", at, i))
			}
		}
	}
	for _, ex := range examples(d) {
		if v, err := apicontract.DecodeJSON(ex.raw); err == nil {
			walk(v, ex.at)
		}
	}
}

// Rows every operation of a kind must list: 500 everywhere, the key errors on merchant routes,
// 429 where a limit applies, idempotency conflicts and the test mode refusal.
func TestOpenAPIErrorInvariants(t *testing.T) {
	d := loadOpenAPI(t)
	for _, e := range d.Operations() {
		at := e.Method + " " + e.Path
		need := func(status int, code, why string) {
			t.Helper()
			if !slices.Contains(e.Op.ErrorCodes(status), code) {
				t.Errorf("%s: %s, so x-errors must list %d %s", at, why, status, code)
			}
		}
		need(500, "internal_error", "any route answers it when something fails on our side (service.AsError)")
		if e.Op.Scope != "" {
			need(401, "invalid_api_key", "it takes a secret key")
			need(403, "insufficient_scope", "it requires a scope")
		}
		if e.Op.RateLimit != nil {
			need(429, "rate_limited", "it is rate limited")
		}
		if slices.ContainsFunc(e.Op.Parameters, func(p apicontract.Param) bool { return p.In == "header" && p.Name == "Idempotency-Key" }) {
			need(409, "idempotency_conflict", "it takes an Idempotency-Key")
		}
		if e.Op.TestOnly {
			need(403, "test_mode_only", "it is test mode only")
		}
	}
}

func TestOpenAPIErrorCodesExistInCode(t *testing.T) {
	d := loadOpenAPI(t)
	var src strings.Builder
	err := filepath.WalkDir("..", func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, err := os.ReadFile(p)
		src.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	text := src.String()
	for _, e := range d.Operations() {
		for _, row := range e.Op.Errors {
			declared := regexp.MustCompile(`Code\w*(\s+\w+)?\s*=\s*"` + regexp.QuoteMeta(row.Code) + `"`)
			if !declared.MatchString(text) {
				t.Errorf("%s %s: error code %s is not declared as a Code constant in the Go code", e.Method, e.Path, row.Code)
			}
		}
	}
}

func enumOf(t *testing.T, d *apicontract.Doc, schema, field string) []string {
	t.Helper()
	s := d.Components.Schemas[schema].Properties[field]
	var out []string
	for _, e := range s.Enum {
		if e != nil {
			out = append(out, fmt.Sprint(e))
		}
	}
	sort.Strings(out)
	return out
}

func sortedStrings[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	sort.Strings(out)
	return out
}

func TestOpenAPIEnumsMatchCode(t *testing.T) {
	d := loadOpenAPI(t)
	statuses := sortedStrings(session.AllStatuses)
	payments := sortedStrings(session.AllPaymentStatuses)
	reasons := sortedStrings([]string{service.ReasonProductInactive, service.ReasonInactive, service.ReasonExpired, service.ReasonExhausted, service.ReasonReserved})
	for _, c := range []struct {
		schema, field string
		want          []string
	}{
		{"Session", "status", statuses},
		{"PublicSession", "status", statuses},
		{"Payment", "status", payments},
		{"PublicPayment", "status", payments},
		{"PaymentLink", "unavailable_reason", reasons},
		{"PublicLink", "unavailable_reason", reasons},
		{"PublicAddress", "unavailable_reason", sortedStrings([]string{service.ReasonAddressRotated, service.ReasonProductInactive, service.ReasonNoNetworks})},
		{"Event", "type", sortedStrings(session.AllEventTypes)},
	} {
		t.Run(c.field, func(t *testing.T) {
			if got := enumOf(t, d, c.schema, c.field); !slices.Equal(got, c.want) {
				t.Errorf("%s.%s enum %v, the code has %v", c.schema, c.field, got, c.want)
			}
		})
	}
}

func TestOpenAPINetworksExampleIsTheRegistry(t *testing.T) {
	d := loadOpenAPI(t)
	op := d.Paths["/v1/checkout/networks"]["get"]
	want, _ := json.Marshal(view.NetworkList{Data: view.Networks(chain.Pilot(), chainpause.Set{})})
	var a, b any
	_ = json.Unmarshal(op.Responses["200"].Content["application/json"].Example, &a)
	_ = json.Unmarshal(want, &b)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("GET /v1/checkout/networks example differs from the registry:\n doc:  %v\n code: %s", a, want)
	}
}

func TestOpenAPIWebhookEvents(t *testing.T) {
	d := loadOpenAPI(t)
	var got []string
	for name, methods := range d.Webhooks {
		got = append(got, name)
		op := methods["post"]
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Session struct {
					Status string `json:"status"`
				} `json:"session"`
			} `json:"data"`
		}
		_ = json.Unmarshal(op.RequestBody.Content["application/json"].Example, &ev)
		if ev.Type != name {
			t.Errorf("webhook %s: example type %s", name, ev.Type)
		}
		if st := op.SessionStatus; st != "unchanged" && st != "recomputed" && st != ev.Data.Session.Status {
			t.Errorf("webhook %s: x-session-status %s, example session status %s", name, st, ev.Data.Session.Status)
		}
	}
	sort.Strings(got)
	if want := sortedStrings(session.AllEventTypes); !slices.Equal(want, got) {
		t.Errorf("webhooks %v, the code sends %v", got, want)
	}
}

// A product's split per network is capped by what the contracts and the forwarder program accept
// (payout.MaxRecipients); the contract states the same cap as maxItems on create and update.
func TestOpenAPIPayoutMaxItems(t *testing.T) {
	d := loadOpenAPI(t)
	for _, op := range []struct{ path, method string }{{"/v1/products", "post"}, {"/v1/products/{id}", "patch"}} {
		t.Run(op.path, func(t *testing.T) {
			s := d.Raw["paths"]
			for _, k := range []string{op.path, op.method, "requestBody", "content", "application/json", "schema", "properties", "payout", "additionalProperties", "maxItems"} {
				m, _ := s.(map[string]any)
				s = m[k]
			}
			if n, ok := s.(float64); !ok || int(n) != payout.MaxRecipients {
				t.Errorf("%s %s payout maxItems is %v, want %d", op.method, op.path, s, payout.MaxRecipients)
			}
		})
	}
}

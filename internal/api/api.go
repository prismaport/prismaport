// Package api is the merchant-facing HTTP API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prismaport/prismaport/internal/apikey"
	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
	"github.com/prismaport/prismaport/internal/version"
	"github.com/prismaport/prismaport/internal/view"
	"github.com/prismaport/prismaport/internal/webhook"
)

const maxBodyBytes = 64 << 10

// Server is the HTTP API: it routes requests, authenticates them and calls the service layer.
type Server struct {
	svc     *service.Service
	st      *store.Store
	log     *slog.Logger
	mux     *http.ServeMux
	limiter *publicLimiter
	routes  []MerchantRoute
	// publicRoutes are the payer-side checkout routes, which take no API key.
	publicRoutes []PublicRoute
	// dashRoutes are the patterns of the signed-in dashboard routes, which act for the session's
	// merchant.
	dashRoutes []string
	// hosts routes requests by their Host header; nil serves every route on every host.
	hosts *hostRouter
	// split is set in split mode, where the pages are served elsewhere; hosts is then nil.
	split          *splitFrontend
	landingMounted bool
	// requestTimeout is the deadline of every request's context (RequestTimeout).
	requestTimeout time.Duration
	// bg runs work that outlives a request; Close waits for it.
	bg *background
}

// Close stops the server's background work and waits for it. Call it after the HTTP server
// shut down, before closing the database pool.
func (s *Server) Close() {
	if s.bg != nil {
		s.bg.Close()
	}
}

// RequestTimeout bounds the context of every request: a hung query or provider call ends the
// request with an error inside the server's 30-second write timeout instead of holding a
// connection and a pool slot. No route streams or long-polls.
const RequestTimeout = 25 * time.Second

// WebhookEndpointList is the answer of GET /v1/webhook_endpoints: every endpoint, one page.
type WebhookEndpointList struct {
	Data    []service.WebhookEndpointView `json:"data"`
	HasMore bool                          `json:"has_more"`
}

// Deleted answers a DELETE.
type Deleted struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
}

// MountRoutes adds every route beyond the merchant API core: the payer-side checkout, the
// dashboard API and the sandbox tools. Test events leave from the API process with the
// worker's delivery rules (d).
func (s *Server) MountRoutes(co *checkout.Checkout, d *webhook.Dispatcher) {
	s.MountCheckout(co)
	s.MountSolanaCheckout(co)
	s.MountCheckoutLinks(co)
	s.MountCheckoutAddresses(co)
	s.MountDashboard()
	s.MountSandbox(d)
}

// MerchantRoute is one route of the merchant API and the scope a key needs for it.
type MerchantRoute struct {
	Pattern string
	Scope   apikey.Scope
}

// New returns a Server that serves requests through svc and st, logging to log.
func New(svc *service.Service, st *store.Store, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{svc: svc, st: st, log: log, mux: http.NewServeMux(), requestTimeout: RequestTimeout}
	// Default limits until ConfigurePublic replaces them, so no request ever runs unlimited.
	if svc != nil {
		_ = s.ConfigurePublic(PublicOptions{})
	}
	// healthz says the process serves HTTP; readyz also needs the database to answer.
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	s.mux.HandleFunc("GET /readyz", s.readyz)
	// version names the running build: the release, the commit and when the image was built.
	s.mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_ = writeJSON(w, http.StatusOK, version.Get())
	})
	s.route("POST /v1/products", apikey.ProductsWrite, jsonRoute(productCreateInput, s.createProduct))
	s.route("GET /v1/products", apikey.ProductsRead, listRoute(s.listProducts))
	s.route("GET /v1/products/{id}", apikey.ProductsRead, reply(s.getProduct))
	s.route("PATCH /v1/products/{id}", apikey.ProductsWrite, jsonRoute(productPatchInput, s.updateProduct))
	s.route("POST /v1/payment_links", apikey.PaymentLinksWrite, createRoute(linkInput, s.createPaymentLink))
	s.route("GET /v1/payment_links", apikey.PaymentLinksRead, listRoute(s.listPaymentLinks))
	s.route("GET /v1/payment_links/{id}", apikey.PaymentLinksRead, reply(s.getPaymentLink))
	s.route("PATCH /v1/payment_links/{id}", apikey.PaymentLinksWrite, jsonRoute(linkInput, s.updatePaymentLink))
	s.route("POST /v1/sessions", apikey.SessionsWrite, s.createSession)
	s.route("GET /v1/sessions/{id}", apikey.SessionsRead, reply(s.getSession))
	s.route("POST /v1/sessions/{id}/reveal", apikey.SessionsWrite, reply(s.revealSession))
	s.route("POST /v1/sessions/{id}/recheck", apikey.SessionsWrite, s.recheckSession)
	s.route("POST /v1/customer_addresses", apikey.CustomerAddressesWrite, s.createCustomerAddress)
	s.route("GET /v1/customer_addresses", apikey.CustomerAddressesRead, listRoute(s.listCustomerAddresses))
	s.route("GET /v1/customer_addresses/{id}", apikey.CustomerAddressesRead, reply(s.getCustomerAddress))
	s.route("POST /v1/customer_addresses/{id}/rotate", apikey.CustomerAddressesWrite, reply(s.rotateCustomerAddress))
	s.route("GET /v1/customer_addresses/{id}/sessions", apikey.CustomerAddressesRead, listRoute(s.listAddressSessions))
	s.route("GET /v1/events", apikey.EventsRead, reply(s.listEvents))
	s.route("POST /v1/webhook_endpoints", apikey.WebhooksWrite, jsonRoute(bodyFields(endpointCreateInput, endpointFields...), s.createWebhookEndpoint))
	s.route("GET /v1/webhook_endpoints", apikey.WebhooksRead, reply(s.listWebhookEndpoints))
	s.route("PATCH /v1/webhook_endpoints/{id}", apikey.WebhooksWrite, jsonRoute(bodyFields(endpointEditInput, endpointEditFields...), s.updateWebhookEndpoint))
	s.route("DELETE /v1/webhook_endpoints/{id}", apikey.WebhooksWrite, reply(s.deleteWebhookEndpoint))
	s.route("POST /v1/webhook_endpoints/{id}/rotate_secret", apikey.WebhooksWrite, reply(s.rotateWebhookSecret))
	s.mountPayouts()
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.writeError(w, r, &service.Error{Kind: service.KindNotFound, Code: service.CodeNotFound, Message: "no such route"})
	})
	return s
}

const readyTimeout = 2 * time.Second

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	if err := s.st.Pool().Ping(ctx); err != nil {
		s.log.Warn("readiness check failed: database unreachable", "err", err)
		s.writeError(w, r, &service.Error{Kind: service.KindUnavailable, Code: service.CodeServiceUnavailable, Message: "database unreachable"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HSTS is the Strict-Transport-Security value of every response. Browsers ignore it over plain
// HTTP, so local runs are unaffected; includeSubDomains is left to the apex's edge setting.
const HSTS = "max-age=15552000"

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.requestTimeout > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), s.requestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}
	w.Header().Set("Strict-Transport-Security", HSTS)
	switch {
	case s.split != nil:
		s.serveSplit(w, r)
	case s.hosts != nil:
		s.serveHost(w, r)
	default:
		s.mux.ServeHTTP(w, r)
	}
}

// MerchantRoutes lists every merchant API route with its scope.
func (s *Server) MerchantRoutes() []MerchantRoute { return append([]MerchantRoute(nil), s.routes...) }

type handler func(w http.ResponseWriter, r *http.Request, a service.Auth) error

// route registers a merchant API route: a Bearer secret key, then the key's rate limit (reads
// and writes have separate buckets), then the scope, then h. Services check scopes again.
func (s *Server) route(pattern string, scope apikey.Scope, h handler) {
	s.routes = append(s.routes, MerchantRoute{Pattern: pattern, Scope: scope})
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			s.writeError(w, r, &service.Error{Kind: service.KindUnauthorized, Code: service.CodeInvalidAPIKey, Message: "missing Bearer API key"})
			return
		}
		a, err := s.svc.Authenticate(r.Context(), token)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if err := s.svc.RecordKeyUse(r.Context(), a); err != nil {
			s.log.Warn("recording API key use failed", "key", a.KeyID, "err", err)
		}
		bucket := RouteAPIWrite
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			bucket = RouteAPIRead
		}
		if err := s.limiter.allowKey(w, r, bucket, a.KeyID); err != nil {
			s.writeError(w, r, err)
			return
		}
		if err := a.Require(scope); err != nil {
			s.writeError(w, r, err)
			return
		}
		if err := h(w, r, a); err != nil {
			s.writeError(w, r, err)
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	writeRaw(w, status, body)
	return nil
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, e := s.errorOf(w, r, err)
	detail := view.ErrorDetail{Code: e.Code, Message: e.Message, Param: e.Param}
	if e.Details != nil {
		// Details stay out of the merchant API's error schema: only dashboard errors set them.
		_ = writeJSON(w, status, map[string]errorWithDetails{"error": {ErrorDetail: detail, Details: e.Details}})
		return
	}
	_ = writeJSON(w, status, view.ErrorBody{Error: detail})
}

// errorWithDetails is the error body of the few dashboard errors that carry structured context.
type errorWithDetails struct {
	view.ErrorDetail
	Details map[string]any `json:"details"`
}

// errorOf is the HTTP status and client-facing error of err: a transport refusal keeps its own
// status, any other error has its kind's. It logs a server error and sets Retry-After; the
// caller writes the body.
func (s *Server) errorOf(w http.ResponseWriter, r *http.Request, err error) (int, *service.Error) {
	e := service.AsError(err)
	status := httpStatus(e.Kind)
	if te := (*transportError)(nil); errors.As(err, &te) {
		status = te.status
	}
	if status >= 500 {
		attrs := []any{"method", r.Method, "path", r.URL.Path, "err", err}
		if e.Cause != nil {
			attrs = append(attrs, "cause", e.Cause) // why: the body the client gets says only that it failed
		}
		s.log.Error("request failed", attrs...)
	}
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
	return status, e
}

func badRequest(format string, args ...any) error {
	return &service.Error{Kind: service.KindInvalid, Code: service.CodeInvalidRequest, Message: fmt.Sprintf(format, args...)}
}

// badParam is badRequest about one request field.
func badParam(param, format string, args ...any) error {
	return &service.Error{Kind: service.KindInvalid, Code: service.CodeInvalidRequest, Message: fmt.Sprintf(format, args...), Param: param}
}

func readBody(r *http.Request) ([]byte, error) { return readBodyUpTo(r, maxBodyBytes) }

// readBodyUpTo is readBody with its own limit, for the one route that takes a file.
func readBodyUpTo(r *http.Request, limit int64) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, limit))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, badRequest("request body exceeds %d bytes", limit)
		}
		return nil, badRequest("could not read request body")
	}
	return body, nil
}

// fields decodes a JSON object body into its raw members, rejecting unknown keys so a typo such
// as "ammount" fails loudly instead of being ignored.
func fields(body []byte, allowed ...string) (map[string]json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(body))) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, badRequest("request body must be a JSON object")
	}
	for k := range m {
		ok := false
		for _, a := range allowed {
			if k == a {
				ok = true
				break
			}
		}
		if !ok {
			return nil, badRequest("unknown field %q", k)
		}
	}
	return m, nil
}

func isNull(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }

func queryInt(r *http.Request, name string, def, min, max int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < min || n > max {
		return 0, badRequest("%s must be an integer between %d and %d", name, min, max)
	}
	return n, nil
}

// MountDashboard adds the merchant dashboard API under /v1/dashboard: cookie sessions, CSRF
// tokens and TOTP. Call ConfigurePublic first for its login
// rate limits.
func (s *Server) MountDashboard() {
	s.mountDashboardAuth()
	s.mountDashboardData()
	s.mountDashboardPayouts()
	s.mountDashboardLogo()
}

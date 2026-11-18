package api

import (
	"net/http"

	"github.com/prismaport/prismaport/internal/checkout"
	"github.com/prismaport/prismaport/internal/service"
)

// PublicRoute is one payer-side checkout route and the rate limit bucket each request takes a
// token from per client IP ("" when the route is not limited).
type PublicRoute struct {
	Pattern string
	Bucket  string
}

// public registers a payer-side checkout route: no API key, CORS for any origin, never cached.
// With a bucket, every request first takes a token for its client IP (429 rate_limited over it).
func (s *Server) public(pattern, bucket string, h func(w http.ResponseWriter, r *http.Request) error) {
	s.publicWith(pattern, bucket, s.writeError, h)
}

// publicWith is public with its own error writer, for every error the route answers (429s
// included).
func (s *Server) publicWith(pattern, bucket string, writeError func(http.ResponseWriter, *http.Request, error),
	h func(w http.ResponseWriter, r *http.Request) error) {
	s.publicRoutes = append(s.publicRoutes, PublicRoute{Pattern: pattern, Bucket: bucket})
	var limiter *publicLimiter
	if bucket != "" {
		limiter = s.publicLimiter()
	}
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		cors(w)
		w.Header().Set("Cache-Control", "no-store")
		if limiter != nil {
			if err := limiter.allow(w, r, bucket); err != nil {
				writeError(w, r, err)
				return
			}
		}
		if err := h(w, r); err != nil {
			writeError(w, r, err)
		}
	})
}

// PublicRoutes lists every payer-side checkout route with its rate limit bucket.
func (s *Server) PublicRoutes() []PublicRoute { return append([]PublicRoute(nil), s.publicRoutes...) }

// MountCheckout adds the public checkout endpoints. They take no API key: the session id is
// the only scope, and responses carry only what a payer may see. Browsers on any origin may
// call them (no credentials are involved). The EIP-3009 endpoints are rate limited per client
// IP (see ConfigurePublic).
func (s *Server) MountCheckout(c *checkout.Checkout) {
	s.mountCheckoutPreflight()
	s.mountLogoFile()
	h := checkoutRoutes{c: c}
	// ?active=1 says the page is open and visible: it records the payer's presence, which makes
	// the worker scan the session's chain at its fast pace. Any other value is ignored.
	s.public("GET /v1/checkout/sessions/{id}", "", h.session)
	// "Generate payment": reveals one network's deposit details and starts its monitoring (D48).
	s.public("POST /v1/checkout/sessions/{id}/reveal", RouteCheckoutReveal, h.reveal)
	s.public("POST /v1/checkout/sessions/{id}/eip3009/typed_data", RouteEIP3009TypedData, h.typedData)
	s.public("POST /v1/checkout/sessions/{id}/eip3009/submit", RouteEIP3009Submit, h.submit)
	// Test mode only: the hosted checkout's panel drives the lifecycle simulator, so a flow can
	// be finished from the browser. Live sessions are refused.
	s.public("POST /v1/checkout/sessions/{id}/test_helpers/simulate", RouteCheckoutTestPanel, h.simulate)
}

// checkoutRoutes are the payer-side session routes of MountCheckout.
type checkoutRoutes struct {
	c *checkout.Checkout
}

func (h checkoutRoutes) session(w http.ResponseWriter, r *http.Request) error {
	ses, err := h.c.Session(r.Context(), r.PathValue("id"), r.URL.Query().Get("active") == "1")
	return respond(w, ses, err)
}

func (h checkoutRoutes) reveal(w http.ResponseWriter, r *http.Request) error {
	chainID, err := revealChain(r)
	if err != nil {
		return err
	}
	ses, err := h.c.Reveal(r.Context(), r.PathValue("id"), chainID)
	return respond(w, ses, err)
}

func (h checkoutRoutes) typedData(w http.ResponseWriter, r *http.Request) error {
	m, err := readFields(r, "chain", "asset", "from")
	if err != nil {
		return err
	}
	var in checkout.TypedDataRequest
	if err := requiredStringFields(m, []string{"chain", "asset", "from"}, &in.Chain, &in.Asset, &in.From); err != nil {
		return err
	}
	out, err := h.c.TypedData(r.Context(), r.PathValue("id"), in)
	return respond(w, out, err)
}

var submitFields = []string{"chain", "asset", "from", "value", "valid_after", "valid_before", "signature"}

// submit answers 202: the transfer is queued for broadcast.
func (h checkoutRoutes) submit(w http.ResponseWriter, r *http.Request) error {
	m, err := readFields(r, submitFields...)
	if err != nil {
		return err
	}
	var in checkout.SubmitRequest
	if err := requiredStringFields(m, submitFields, &in.Chain, &in.Asset, &in.From, &in.Value, &in.ValidAfter, &in.ValidBefore,
		&in.Signature); err != nil {
		return err
	}
	out, err := h.c.Submit(r.Context(), r.PathValue("id"), in)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, out)
}

func (h checkoutRoutes) simulate(w http.ResponseWriter, r *http.Request) error {
	in, err := simulateInput(r)
	if err != nil {
		return err
	}
	out, err := h.c.Simulate(r.Context(), r.PathValue("id"), in)
	return respond(w, out, err)
}

var simulateFields = []string{"action", "amount", "chain", "asset"}

// simulateInput reads a lifecycle simulator request; every member is optional.
func simulateInput(r *http.Request) (service.SimulateInput, error) {
	var in service.SimulateInput
	m, err := readFields(r, simulateFields...)
	if err != nil {
		return in, err
	}
	err = optionalStringFields(m, simulateFields, &in.Action, &in.Amount, &in.Chain, &in.Asset)
	return in, err
}

// revealChain reads the body of a reveal: {"chain": "<registry id>"}.
func revealChain(r *http.Request) (string, error) {
	m, err := readFields(r, "chain")
	if err != nil {
		return "", err
	}
	v, err := decodeField[string](m, "chain", "a string")
	if err != nil {
		return "", err
	}
	if v == nil || *v == "" {
		return "", badRequest("chain is required")
	}
	return *v, nil
}

func (s *Server) mountCheckoutPreflight() {
	s.mux.HandleFunc("OPTIONS /v1/checkout/", func(w http.ResponseWriter, _ *http.Request) {
		cors(w)
		w.WriteHeader(http.StatusNoContent)
	})
}

func cors(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key")
	// The checkout reads the server's clock and rate limit waits from another origin when it is
	// served apart from the API.
	h.Set("Access-Control-Expose-Headers", "Date, Retry-After")
	h.Set("Access-Control-Max-Age", "600")
}

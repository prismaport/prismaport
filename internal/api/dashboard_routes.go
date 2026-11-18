package api

import (
	"encoding/json"
	"net/http"

	"github.com/prismaport/prismaport/internal/service"
	"github.com/prismaport/prismaport/internal/store"
)

// A list page holds defaultPageLimit items unless the request's limit asks for 1 to maxPageLimit.
const (
	defaultPageLimit = 20
	maxPageLimit     = 100
)

// pageLimit reads a list request's limit query parameter.
func pageLimit(r *http.Request) (int, error) {
	n, err := queryInt(r, "limit", defaultPageLimit, 1, maxPageLimit)
	return int(n), err
}

// modeRoute registers a dashboard route scoped to the required mode parameter. While a
// merchant's live mode is closed, it can be read but not changed.
func (s *Server) modeRoute(method, path string, h func(w http.ResponseWriter, r *http.Request, ds service.DashSession, a service.Auth) error) {
	s.dash(method, path, func(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
		live, err := dashMode(r)
		if err != nil {
			return err
		}
		if live && !ds.User.LiveEnabled && changes(r) {
			return service.LiveReviewPending()
		}
		return h(w, r, ds, ds.User.Auth(live))
	})
}

// mountDashboardData adds the dashboard's data routes, one group per resource. The order of the
// groups is the order of DashboardRoutes.
func (s *Server) mountDashboardData() {
	limiter := s.publicLimiter()
	s.mountDashOverview()
	s.mountDashProducts()
	s.mountDashLinks()
	s.mountDashSessions()
	s.mountDashCustomerAddresses()
	s.mountDashAPIKeys(limiter)
	s.mountDashWebhooks(limiter)
	s.mountDashSettings()
}

func (s *Server) mountDashOverview() {
	s.mode("GET", "/overview", reply(func(r *http.Request, a service.Auth) (any, error) {
		return s.svc.DashOverview(r.Context(), a)
	}))
	// Where the merchant stands in Get started for the mode (D57): the menu item, its counter
	// and the step the page opens at.
	s.mode("GET", "/onboarding", reply(func(r *http.Request, a service.Auth) (any, error) {
		return s.svc.DashOnboarding(r.Context(), a)
	}))
}

func (s *Server) mountDashProducts() {
	s.mode("GET", "/products", listRoute(s.dashListProducts))
	s.mode("POST", "/products", createRoute(productCreateInput, s.dashCreateProduct))
	s.mode("GET", "/products/{id}", reply(func(r *http.Request, a service.Auth) (any, error) {
		return s.svc.DashProduct(r.Context(), a, r.PathValue("id"))
	}))
	s.mode("PATCH", "/products/{id}", jsonRoute(productPatchInput, func(r *http.Request, a service.Auth, in service.ProductInput) (any, error) {
		return s.svc.DashUpdateProduct(r.Context(), a, r.PathValue("id"), in)
	}))
	s.mode("GET", "/products/{id}/links", listRoute(s.dashProductLinks))
	s.mode("GET", "/products/{id}/sessions", listRoute(func(r *http.Request, a service.Auth, p listPage) (any, error) {
		return s.svc.DashProductSessions(r.Context(), a, r.PathValue("id"), p.cursor, p.limit)
	}))
}

func (s *Server) dashListProducts(r *http.Request, a service.Auth, p listPage) (any, error) {
	q := r.URL.Query()
	return s.svc.DashListProducts(r.Context(), a, service.ProductFilter{Status: q.Get("status"), Query: q.Get("q"),
		Cursor: p.cursor, Limit: p.limit})
}

func (s *Server) dashCreateProduct(r *http.Request, a service.Auth, key string, body []byte, in service.ProductInput) (any, bool, error) {
	return s.svc.DashCreateProduct(r.Context(), a, key, body, in)
}

func (s *Server) dashProductLinks(r *http.Request, a service.Auth, p listPage) (any, error) {
	q := r.URL.Query()
	return s.svc.DashProductLinks(r.Context(), a, r.PathValue("id"), service.LinkFilter{Status: q.Get("status"), Query: q.Get("q"),
		Cursor: p.cursor, Limit: p.limit})
}

func (s *Server) mountDashLinks() {
	s.mode("GET", "/links", listRoute(s.dashListLinks))
	s.mode("POST", "/links", createRoute(linkInput, s.dashCreateLink))
	s.mode("GET", "/links/{id}", reply(func(r *http.Request, a service.Auth) (any, error) {
		return s.svc.DashLink(r.Context(), a, r.PathValue("id"))
	}))
	s.mode("PATCH", "/links/{id}", jsonRoute(linkInput, func(r *http.Request, a service.Auth, in service.LinkInput) (any, error) {
		return s.svc.DashUpdateLink(r.Context(), a, r.PathValue("id"), in)
	}))
	s.mode("GET", "/links/{id}/sessions", listRoute(func(r *http.Request, a service.Auth, p listPage) (any, error) {
		return s.svc.DashLinkSessions(r.Context(), a, r.PathValue("id"), p.cursor, p.limit)
	}))
}

func (s *Server) dashListLinks(r *http.Request, a service.Auth, p listPage) (any, error) {
	q := r.URL.Query()
	return s.svc.DashListLinks(r.Context(), a, service.LinkFilter{Status: q.Get("status"), ProductID: q.Get("product_id"),
		Query: q.Get("q"), Cursor: p.cursor, Limit: p.limit})
}

func (s *Server) dashCreateLink(r *http.Request, a service.Auth, key string, body []byte, in service.LinkInput) (any, bool, error) {
	return s.svc.DashCreateLink(r.Context(), a, key, body, in)
}

func (s *Server) mountDashSessions() {
	s.mode("GET", "/sessions", listRoute(s.dashListSessions))
	s.mode("GET", "/sessions/{id}", reply(func(r *http.Request, a service.Auth) (any, error) {
		return s.svc.DashSession(r.Context(), a, r.PathValue("id"))
	}))
	// "Check again" on a session past its watch window: 202 with the session detail.
	s.mode("POST", "/sessions/{id}/recheck", s.dashRecheckSession)
}

func (s *Server) dashListSessions(r *http.Request, a service.Auth, p listPage) (any, error) {
	q := r.URL.Query()
	return s.svc.DashListSessions(r.Context(), a, store.SessionFilter{Status: q.Get("status"), ProductID: q.Get("product_id"),
		LinkID: q.Get("link_id"), Query: q.Get("q"), ClientReferenceID: q.Get("client_reference_id")}, p.cursor, p.limit)
}

func (s *Server) dashRecheckSession(w http.ResponseWriter, r *http.Request, a service.Auth) error {
	if _, err := s.svc.RecheckSession(r.Context(), a, r.PathValue("id")); err != nil {
		return err
	}
	d, err := s.svc.DashSession(r.Context(), a, r.PathValue("id"))
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusAccepted, d)
}

// mountDashCustomerAddresses adds customer addresses (B11): the merchant API's list, detail,
// sessions and rotate, plus the product, stats and attention. Rotate takes no step-up: like a
// product payout change, it only points the customer's next payments at the product's current
// payout, which pays the merchant's own wallets (adding a wallet is what takes the step-up).
func (s *Server) mountDashCustomerAddresses() {
	s.mode("GET", "/customer_addresses", listRoute(func(r *http.Request, a service.Auth, p listPage) (any, error) {
		return s.svc.DashListCustomerAddresses(r.Context(), a, r.URL.Query().Get("customer_id"), p.cursor, p.limit)
	}))
	s.mode("GET", "/customer_addresses/{id}", reply(func(r *http.Request, a service.Auth) (any, error) {
		return s.svc.DashCustomerAddress(r.Context(), a, r.PathValue("id"))
	}))
	s.mode("GET", "/customer_addresses/{id}/sessions", listRoute(func(r *http.Request, a service.Auth, p listPage) (any, error) {
		return s.svc.DashAddressSessions(r.Context(), a, r.PathValue("id"), p.cursor, p.limit)
	}))
	// Body ignored, like the merchant API's rotate.
	s.mode("POST", "/customer_addresses/{id}/rotate", reply(func(r *http.Request, a service.Auth) (any, error) {
		return s.svc.DashRotateCustomerAddress(r.Context(), a, r.PathValue("id"))
	}))
}

// dashKeyRoutes are the API key and webhook endpoint routes that take a Live step-up: they share
// the per-user step-up bucket of the public limiter.
type dashKeyRoutes struct {
	s       *Server
	limiter *publicLimiter
}

// stepUpBucket takes a token of the user's step-up bucket (a confirmation code is checked).
func (k dashKeyRoutes) stepUpBucket(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	return k.limiter.allowKey(w, r, RouteDashboardTOTP, ds.User.ID)
}

func (s *Server) mountDashAPIKeys(limiter *publicLimiter) {
	k := dashKeyRoutes{s: s, limiter: limiter}
	s.mode("GET", "/api_keys", reply(func(r *http.Request, a service.Auth) (any, error) {
		list, err := s.svc.DashAPIKeys(r.Context(), a)
		return map[string]any{"data": list}, err
	}))
	// The spec puts mode in this body; the query parameter is accepted too and must agree.
	s.dash("POST", "/api_keys", k.createAPIKey)
	s.mode("POST", "/api_keys/{id}/revoke", reply(func(r *http.Request, a service.Auth) (any, error) {
		return s.svc.DashRevokeAPIKey(r.Context(), a, r.PathValue("id"))
	}))
	s.modeRoute("POST", "/api_keys/{id}/roll", k.rollAPIKey)
}

// apiKeyMode reads the mode of an API key create: the body's, or the query's when the body has
// none; both given must agree.
func apiKeyMode(r *http.Request, body *string) (live bool, err error) {
	q := r.URL.Query().Get("mode")
	mode := q
	if body != nil {
		if q != "" && q != *body {
			return false, badRequest("mode in the body and in the query differ")
		}
		mode = *body
	}
	switch mode {
	case "live":
		return true, nil
	case "test":
		return false, nil
	}
	return false, badRequest("mode must be live or test")
}

func (k dashKeyRoutes) createAPIKey(w http.ResponseWriter, r *http.Request, ds service.DashSession) error {
	m, err := readFields(r, withStepUp("name", "scopes", "mode")...)
	if err != nil {
		return err
	}
	d := &members{m: m}
	name := member[string](d, "name", "a string")
	scopes := member[[]string](d, "scopes", "an array of scopes")
	mode := member[string](d, "mode", "live or test")
	if d.err != nil {
		return d.err
	}
	proof, err := stepUpProof(m)
	if err != nil {
		return err
	}
	live, err := apiKeyMode(r, mode.Ptr())
	if err != nil {
		return err
	}
	if !name.Given() || !scopes.Given() {
		return badRequest("name and scopes are required")
	}
	if live && !ds.User.LiveEnabled {
		return service.LiveReviewPending()
	}
	if live {
		if err := k.stepUpBucket(w, r, ds); err != nil {
			return err
		}
	}
	key, err := k.s.svc.DashCreateAPIKey(r.Context(), ds.User, live, name.Value, scopes.Value, proof)
	return respond(w, key, err)
}

func (k dashKeyRoutes) rollAPIKey(w http.ResponseWriter, r *http.Request, ds service.DashSession, a service.Auth) error {
	m, err := readFields(r, withStepUp("overlap")...)
	if err != nil {
		return err
	}
	overlap, err := decodeField[string](m, "overlap", "one of now, 1h, 24h or 7d")
	if err != nil {
		return err
	}
	proof, err := stepUpProof(m)
	if err != nil {
		return err
	}
	if overlap == nil {
		return badRequest("overlap is required: now, 1h, 24h or 7d")
	}
	if a.Livemode {
		if err := k.stepUpBucket(w, r, ds); err != nil {
			return err
		}
	}
	res, err := k.s.svc.DashRollAPIKey(r.Context(), ds.User, a.Livemode, r.PathValue("id"), *overlap, proof)
	return respond(w, res, err)
}

func (s *Server) mountDashWebhooks(limiter *publicLimiter) {
	k := dashKeyRoutes{s: s, limiter: limiter}
	s.mode("GET", "/webhook_endpoints", reply(func(r *http.Request, a service.Auth) (any, error) {
		list, err := s.svc.ListWebhookEndpoints(r.Context(), a)
		return map[string]any{"data": list}, err
	}))
	// A Live mode create, URL change or secret rotation takes the confirmation code of a live API
	// key (emailed, or the TOTP code with 2FA on; D58), so a stolen session cannot redirect live
	// events (ADR 0008 C-3).
	s.modeRoute("POST", "/webhook_endpoints", k.createWebhookEndpoint)
	// Edit, pause and enable. A Live mode URL change takes the step-up of creating a Live
	// endpoint.
	s.modeRoute("PATCH", "/webhook_endpoints/{id}", k.updateWebhookEndpoint)
	s.modeRoute("POST", "/webhook_endpoints/{id}/rotate_secret", k.rotateWebhookSecret)
	s.mode("DELETE", "/webhook_endpoints/{id}", reply(s.deleteDashWebhookEndpoint))
	s.mode("GET", "/webhook_endpoints/{id}/deliveries", listRoute(func(r *http.Request, a service.Auth, p listPage) (any, error) {
		return s.svc.DashDeliveries(r.Context(), a, r.PathValue("id"), p.cursor, p.limit)
	}))
	s.mode("POST", "/webhook_deliveries/{id}/resend", reply(func(r *http.Request, a service.Auth) (any, error) {
		return s.svc.DashResendDelivery(r.Context(), a, r.PathValue("id"))
	}))
}

// liveStepUp reads the step-up proof of a webhook endpoint change; in Live mode it also takes a
// token of the user's step-up bucket.
func (k dashKeyRoutes) liveStepUp(w http.ResponseWriter, r *http.Request, ds service.DashSession, a service.Auth,
	m map[string]json.RawMessage) (service.StepUpProof, error) {
	proof, err := stepUpProof(m)
	if err != nil || !a.Livemode {
		return proof, err
	}
	return proof, k.stepUpBucket(w, r, ds)
}

func (k dashKeyRoutes) createWebhookEndpoint(w http.ResponseWriter, r *http.Request, ds service.DashSession, a service.Auth) error {
	m, err := readFields(r, withStepUp(endpointFields...)...)
	if err != nil {
		return err
	}
	in, err := endpointCreateInput(m)
	if err != nil {
		return err
	}
	proof, err := k.liveStepUp(w, r, ds, a, m)
	if err != nil {
		return err
	}
	e, err := k.s.svc.DashCreateWebhookEndpoint(r.Context(), ds.User, a, in, proof)
	return respond(w, e, err)
}

func (k dashKeyRoutes) updateWebhookEndpoint(w http.ResponseWriter, r *http.Request, ds service.DashSession, a service.Auth) error {
	m, err := readFields(r, withStepUp(endpointEditFields...)...)
	if err != nil {
		return err
	}
	in, err := endpointEditInput(m)
	if err != nil {
		return err
	}
	proof, err := stepUpProof(m)
	if err != nil {
		return err
	}
	if a.Livemode && in.URL != nil {
		if err := k.stepUpBucket(w, r, ds); err != nil {
			return err
		}
	}
	e, err := k.s.svc.DashUpdateWebhookEndpoint(r.Context(), ds.User, a, r.PathValue("id"), in, proof)
	return respond(w, e, err)
}

func (k dashKeyRoutes) rotateWebhookSecret(w http.ResponseWriter, r *http.Request, ds service.DashSession, a service.Auth) error {
	m, err := readFields(r, withStepUp()...)
	if err != nil {
		return err
	}
	proof, err := k.liveStepUp(w, r, ds, a, m)
	if err != nil {
		return err
	}
	e, err := k.s.svc.DashRotateWebhookSecret(r.Context(), ds.User, a, r.PathValue("id"), proof)
	return respond(w, e, err)
}

func (s *Server) deleteDashWebhookEndpoint(r *http.Request, a service.Auth) (any, error) {
	err := s.svc.DeleteWebhookEndpoint(r.Context(), a, r.PathValue("id"))
	return map[string]any{"id": r.PathValue("id"), "deleted": true}, err
}

func (s *Server) mountDashSettings() {
	s.mode("GET", "/settings", reply(func(r *http.Request, a service.Auth) (any, error) {
		return s.svc.DashSettings(r.Context(), a)
	}))
	s.mode("PUT", "/accepted_assets", s.setAcceptedAssets)
}

func (s *Server) setAcceptedAssets(w http.ResponseWriter, r *http.Request, a service.Auth) error {
	m, err := readFields(r, "items")
	if err != nil {
		return err
	}
	raw, ok := m["items"]
	if !ok {
		return badRequest("items is required")
	}
	var items []struct {
		Chain   *string `json:"chain"`
		Asset   *string `json:"asset"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return badRequest("items must be an array of {chain, asset, enabled}")
	}
	var in []service.AcceptedAsset
	for _, it := range items {
		if it.Chain == nil || it.Asset == nil || it.Enabled == nil {
			return badRequest("every item needs chain, asset and enabled")
		}
		in = append(in, service.AcceptedAsset{Chain: *it.Chain, Asset: *it.Asset, Enabled: *it.Enabled})
	}
	list, err := s.svc.DashSetAcceptedAssets(r.Context(), a, in)
	return respond(w, map[string]any{"accepted_assets": list}, err)
}
